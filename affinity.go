package main

// Prompt Cache 会话/前缀亲和（默认关闭）。
//
// 目的：同一会话的连续请求尽量落在同一个账号上，使上游的 prompt cache 有可能命中。
//
// 设计约束（与调度器的既有约束共存）：
//   - 只作为"同等可用账号之间的优先项"，绝不越过模型名单、免费站点优先、冷却、
//     配额和探测状态；因此它不会为了缓存把请求送到更贵的账号上。
//   - 键包含调用方指纹（API Key 摘要）、模型和站点无关的会话标识；不同用户即使
//     session_id 相同也不会共享绑定。
//   - 客户端未提供稳定会话标识时，用"上游请求体的前 N 字节摘要"作为前缀键；
//     每请求新生成的随机 UUID 不参与亲和。
//   - LRU + TTL，容量固定，不落盘、不记录提示词正文。
//
// 默认关闭：上游缓存是否按账号隔离尚未验证，未验证前不应改变既有路由行为。
// 打开方式见 config.example.json 的 affinity 段。

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	affinityModeSession = "session" // 只用客户端提供的稳定会话标识
	affinityModePrefix  = "prefix"  // 只用提示词前缀摘要
	affinityModeHybrid  = "hybrid"  // 优先会话，其次前缀

	affinityDefaultCapacity   = 4096
	affinityDefaultTTL        = 30 * time.Minute
	affinityDefaultHeadBytes  = 64 << 10 // 64 KiB：足够覆盖 system + 工具定义 + 早期历史
	affinityMinPrefixBytes    = 2 << 10  // 小于 2 KiB 的前缀不具备区分度，不建绑定
	affinityKeyHexLen         = 32       // 存储用 128 bit 摘要即可
	affinitySessionIDMaxBytes = 512
)

// affinitySettings 是运行期生效的配置（由 applyRuntimeConfig 写入）。
type affinitySettings struct {
	Enabled        bool
	Mode           string
	TTL            time.Duration
	Capacity       int
	PrefixHeadByte int
}

var (
	affinityConfigMu sync.RWMutex
	affinityCfg      = affinitySettings{
		Mode:           affinityModeHybrid,
		TTL:            affinityDefaultTTL,
		Capacity:       affinityDefaultCapacity,
		PrefixHeadByte: affinityDefaultHeadBytes,
	}
)

func currentAffinitySettings() affinitySettings {
	affinityConfigMu.RLock()
	s := affinityCfg
	affinityConfigMu.RUnlock()
	return s
}

// setAffinityConfig 由配置加载/热加载调用；非法值回落到默认值而不是报错中断启动。
func setAffinityConfig(enabled bool, mode string, ttlSeconds, capacity, headBytes int) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case affinityModeSession, affinityModePrefix, affinityModeHybrid:
	case "":
		mode = affinityModeHybrid
	default:
		mode = affinityModeHybrid
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl <= 0 {
		ttl = affinityDefaultTTL
	}
	if capacity <= 0 {
		capacity = affinityDefaultCapacity
	}
	if capacity > 1<<20 {
		capacity = 1 << 20
	}
	if headBytes <= 0 {
		headBytes = affinityDefaultHeadBytes
	}
	if headBytes < affinityMinPrefixBytes {
		headBytes = affinityMinPrefixBytes
	}
	if headBytes > 1<<20 {
		headBytes = 1 << 20
	}
	affinityConfigMu.Lock()
	prev := affinityCfg
	affinityCfg = affinitySettings{Enabled: enabled, Mode: mode, TTL: ttl, Capacity: capacity, PrefixHeadByte: headBytes}
	affinityConfigMu.Unlock()

	switch {
	case !enabled:
		if prev.Enabled {
			// 关闭即丢弃绑定：重新启用后不应命中旧关系。
			affinityStoreClear()
		}
	case prev.Capacity != capacity || prev.TTL != ttl:
		// 容量或 TTL 变化：重建存储，新 TTL 立即对既有条目生效。
		affinityStoreReconfigure(capacity, ttl)
	}
}

// ---------------------------------------------------------------------------
// 绑定表：LRU + TTL
// ---------------------------------------------------------------------------

type affinityEntry struct {
	key     string
	account string // Account.Path（凭据文件路径，进程内唯一且稳定）
	at      int64  // 最近一次使用，UnixNano
}

type affinityStoreT struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	items map[string]*list.Element
	lru   *list.List
}

var (
	affinityStoreMu sync.RWMutex // 保护 affinityStore 指针本身
	affinityStore   = newAffinityStore(affinityDefaultCapacity, affinityDefaultTTL)
)

func newAffinityStore(capacity int, ttl time.Duration) *affinityStoreT {
	return &affinityStoreT{
		cap:   capacity,
		ttl:   ttl,
		items: make(map[string]*list.Element, capacity),
		lru:   list.New(),
	}
}

// affinityStoreClear 真正清空绑定（禁用亲和时必须丢弃旧关系，避免重新启用后
// 沿用过期/错误的账号绑定）。
func affinityStoreClear() {
	affinityStoreMu.Lock()
	old := affinityStore
	affinityStore = newAffinityStore(old.cap, old.ttl)
	affinityStoreMu.Unlock()
	affinityMetrics.reset()
}

// affinityStoreReconfigure 在容量或 TTL 变化时重建存储：
//   - 条目按最近使用顺序保留（LRU -> MRU，逐条 PushFront 才能保持顺序）；
//   - 每条复制成新的 *affinityEntry，绝不让新旧存储共享同一个指针
//     （否则两边各持不同的锁去改 item.at，就是数据竞争）；
//   - 新 TTL 立即对既有条目生效（按各自的最近使用时间判断）。
func affinityStoreReconfigure(capacity int, ttl time.Duration) {
	if capacity <= 0 {
		capacity = affinityDefaultCapacity
	}
	affinityStoreMu.Lock()
	old := affinityStore
	next := newAffinityStore(capacity, ttl)
	old.mu.Lock()
	for e := old.lru.Back(); e != nil; e = e.Prev() {
		item := e.Value.(*affinityEntry)
		next.items[item.key] = next.lru.PushFront(&affinityEntry{key: item.key, account: item.account, at: item.at})
		for next.lru.Len() > next.cap {
			back := next.lru.Back()
			if back == nil {
				break
			}
			next.lru.Remove(back)
			delete(next.items, back.Value.(*affinityEntry).key)
		}
	}
	old.mu.Unlock()
	affinityStore = next
	affinityStoreMu.Unlock()
}

func (s *affinityStoreT) get(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return "", false
	}
	item := el.Value.(*affinityEntry)
	if s.ttl > 0 && now.Sub(time.Unix(0, item.at)) > s.ttl {
		s.lru.Remove(el)
		delete(s.items, key)
		affinityMetrics.expired.Add(1)
		return "", false
	}
	item.at = now.UnixNano()
	s.lru.MoveToFront(el)
	return item.account, true
}

func (s *affinityStoreT) put(key, account string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		item := el.Value.(*affinityEntry)
		item.account = account
		item.at = now.UnixNano()
		s.lru.MoveToFront(el)
		return
	}
	el := s.lru.PushFront(&affinityEntry{key: key, account: account, at: now.UnixNano()})
	s.items[key] = el
	for s.lru.Len() > s.cap {
		back := s.lru.Back()
		if back == nil {
			break
		}
		s.lru.Remove(back)
		delete(s.items, back.Value.(*affinityEntry).key)
		affinityMetrics.evicted.Add(1)
	}
}

func (s *affinityStoreT) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.lru.Remove(el)
		delete(s.items, key)
	}
}

func (s *affinityStoreT) stats() (size int) {
	s.mu.Lock()
	size = s.lru.Len()
	s.mu.Unlock()
	return size
}

// ---------------------------------------------------------------------------
// 指标
// ---------------------------------------------------------------------------

type affinityMetricsT struct {
	hit       atomic.Int64 // 亲和键命中且账号可用，直接沿用
	miss      atomic.Int64 // 有键但无绑定
	disabled  atomic.Int64 // 功能关闭 / 无可用键
	fallback  atomic.Int64 // 有绑定但账号不可用（冷却/禁用/配额/名单），已回退原调度
	rebind    atomic.Int64 // 因账号失效或换号而解除绑定
	bound     atomic.Int64 // 新建绑定
	prefixKey atomic.Int64 // 用前缀摘要作为键的次数
	session   atomic.Int64 // 用客户端会话标识作为键的次数
	expired   atomic.Int64 // TTL 到期被清理
	evicted   atomic.Int64 // LRU 容量淘汰
}

var affinityMetrics affinityMetricsT

func (m *affinityMetricsT) reset() {
	m.hit.Store(0)
	m.miss.Store(0)
	m.disabled.Store(0)
	m.fallback.Store(0)
	m.rebind.Store(0)
	m.bound.Store(0)
	m.prefixKey.Store(0)
	m.session.Store(0)
	m.expired.Store(0)
	m.evicted.Store(0)
}

func affinitySnapshot() map[string]any {
	settings := currentAffinitySettings()
	affinityStoreMu.RLock()
	entries := affinityStore.stats()
	affinityStoreMu.RUnlock()
	return map[string]any{
		"enabled":      settings.Enabled,
		"mode":         settings.Mode,
		"ttlSeconds":   int(settings.TTL / time.Second),
		"capacity":     settings.Capacity,
		"entries":      entries,
		"hit":          affinityMetrics.hit.Load(),
		"miss":         affinityMetrics.miss.Load(),
		"fallback":     affinityMetrics.fallback.Load(),
		"disabled":     affinityMetrics.disabled.Load(),
		"bound":        affinityMetrics.bound.Load(),
		"rebind":       affinityMetrics.rebind.Load(),
		"sessionKeyed": affinityMetrics.session.Load(),
		"prefixKeyed":  affinityMetrics.prefixKey.Load(),
	}
}

// ---------------------------------------------------------------------------
// 键生成
// ---------------------------------------------------------------------------

// affinityKeyFor 生成账号亲和的查表键。
//
// 组成：caller 指纹 + 模型 + 会话标识（或前缀摘要）。
// 返回空串表示"本次不参与亲和"，调用方按原调度走。
func affinityKeyFor(r *http.Request, model string, upstreamBody []byte) string {
	settings := currentAffinitySettings()
	if !settings.Enabled || r == nil {
		affinityMetrics.disabled.Add(1)
		return ""
	}
	caller := apiKeyFingerprint(r.Header.Get("Authorization"))
	if caller == "" {
		caller = apiKeyFingerprint(r.Header.Get("x-api-key"))
	}
	if caller == "" {
		// 未启用 API Key 时没有调用方标识；用来源地址做隔离，
		// 避免不同用户使用相同 session_id 时共享绑定（地址也参与摘要，不落日志）。
		if ip := requestClientIP(r); ip != "" {
			caller = "ip:" + ip
		}
	}
	if caller == "" {
		affinityMetrics.disabled.Add(1)
		return ""
	}
	model = normalizeModelName(model)

	var kind, id string
	if settings.Mode == affinityModeSession || settings.Mode == affinityModeHybrid {
		if h := conversationHeadersFor(r); h != nil && h.Source != "独立生成" {
			if sid := stableSessionID(h.ID); sid != "" {
				kind, id = "s", sid
				affinityMetrics.session.Add(1)
			}
		}
	}
	if id == "" && (settings.Mode == affinityModePrefix || settings.Mode == affinityModeHybrid) {
		// 优先用请求转换阶段已解析对象算出的摘要（无需再扫一遍请求体）；
		// 没有的话才回退到字节扫描。
		digest := prefixDigestFromContext(r)
		if digest == "" {
			digest = promptPrefixDigest(upstreamBody, settings.PrefixHeadByte)
		}
		if digest != "" {
			kind, id = "p", digest
			affinityMetrics.prefixKey.Add(1)
		}
	}
	if id == "" {
		affinityMetrics.disabled.Add(1)
		return ""
	}

	h := sha256.New()
	h.Write([]byte("wbg-affinity-v1\x00"))
	h.Write([]byte(caller))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(id))
	sum := h.Sum(nil)
	return "a" + hex.EncodeToString(sum[:affinityKeyHexLen/2])
}

// stableSessionID 只接受像会话标识的值；过短或明显是一次性随机值的不作为亲和依据。
// 这里不做"是不是 UUID"的判断——客户端给的 UUID 也可能是稳定的会话 ID，
// 关键是它必须由客户端提供（调用方已通过 Source 字段区分随机生成）。
func stableSessionID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) < 8 || len(id) > affinitySessionIDMaxBytes {
		return ""
	}
	return id
}

var (
	affKeyMessages = []byte("messages")
	affKeyTools    = []byte("tools")
)

// promptPrefixDigest 生成"可缓存前缀"的摘要。
//
// 直接哈希请求体前 N 字节是不够的：当请求体本身就小于 N（小会话）时，尾部一追加
// 消息整段字节都会变，亲和键随之失效。这里改为按**结构**取前缀：
//
//	摘要 = SHA256( messages 之前的全部字段 | tools 原文 | 前若干条完整消息 )
//
// 其中"前若干条"按字节预算在**消息边界**上截断，且至少包含 1 条。这样：
//   - 会话只在尾部追加时，前缀完全不变（核心诉求）；
//   - 改工具定义、改早期消息、换模型都会改变摘要；
//   - 扫描量与哈希量都封顶在 head 字节，不随上下文增长。
//
// 找不到 messages 数组时（异常请求体）回退到旧的"前 N 字节"策略。
func promptPrefixDigest(body []byte, head int) string {
	if len(body) < affinityMinPrefixBytes {
		return ""
	}
	if head <= 0 {
		head = affinityDefaultHeadBytes
	}
	h := sha256.New()
	if !hashStablePrefix(h, body, head) {
		n := len(body)
		if head < n {
			n = head
		}
		h.Reset()
		h.Write([]byte("raw\x00"))
		h.Write(body[:n])
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func hashStablePrefix(h hash.Hash, body []byte, budget int) bool {
	i := skipWS(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	i++
	msgStart, msgEnd := -1, -1
	toolStart, toolEnd := -1, -1
	for i < len(body) {
		i = skipWS(body, i)
		if i >= len(body) || body[i] == '}' {
			break
		}
		ks := i
		n, ok := skipString(body, i)
		if !ok {
			return false
		}
		key := body[ks+1 : n-1]
		i = skipWS(body, n)
		if i >= len(body) || body[i] != ':' {
			return false
		}
		i = skipWS(body, i+1)
		vs := i
		end, ok := skipValue(body, i)
		if !ok {
			return false
		}
		switch {
		case bytes.Equal(key, affKeyMessages):
			msgStart, msgEnd = vs, end
		case bytes.Equal(key, affKeyTools):
			toolStart, toolEnd = vs, end
		}
		i = skipWS(body, end)
		if i < len(body) && body[i] == ',' {
			i++
			continue
		}
		break
	}
	if msgStart < 0 || msgEnd <= msgStart {
		return false
	}

	h.Write([]byte("v2\x00"))
	h.Write(body[:msgStart]) // messages 之前的所有字段（model / max_tokens / ...）
	if toolStart >= 0 {
		h.Write([]byte("tools\x00"))
		h.Write(body[toolStart:toolEnd])
	}
	h.Write([]byte("msgs\x00"))

	// 只取第一条消息（通常是 system prompt，或会话的首个用户消息）。
	// 会话增长只追加尾部，第一条永远不变 —— 这正是"可缓存前缀"的最小充分表示。
	// 取更多条会引入"消息数从 N 变 N+1 时摘要跳变"的问题，反而制造误绑定。
	j := skipWS(body, msgStart+1) // 跳过 '['
	if j >= len(body) || body[j] == ']' {
		return false
	}
	e, ok := skipValue(body, j)
	if !ok {
		return false
	}
	first := body[j:e]
	if len(first) > 32<<10 {
		first = first[:32<<10] // 封顶，避免超长 system 拖慢每轮哈希
	}
	h.Write(first)
	return true
}

// ---------------------------------------------------------------------------
// 与调度器的接线
// ---------------------------------------------------------------------------

// affinityLookup 返回优先账号路径；无绑定或功能关闭时返回空串。
func affinityLookup(key string) string {
	if key == "" {
		return ""
	}
	settings := currentAffinitySettings()
	if !settings.Enabled {
		return ""
	}
	affinityStoreMu.RLock()
	store := affinityStore
	acc, ok := store.get(key, time.Now())
	affinityStoreMu.RUnlock()
	if !ok {
		affinityMetrics.miss.Add(1)
		return ""
	}
	return acc
}

// affinityRemember 记录/刷新绑定。
func affinityRemember(key, accountPath string) {
	if key == "" || accountPath == "" {
		return
	}
	if !currentAffinitySettings().Enabled {
		return
	}
	affinityStoreMu.RLock()
	store := affinityStore
	store.put(key, accountPath, time.Now())
	affinityStoreMu.RUnlock()
	affinityMetrics.bound.Add(1)
}

// affinityForget 解除绑定（账号失效/冷却/换号时调用）。
func affinityForget(key string) {
	if key == "" {
		return
	}
	affinityStoreMu.RLock()
	store := affinityStore
	store.drop(key)
	affinityStoreMu.RUnlock()
	affinityMetrics.rebind.Add(1)
}

// affinityAccountPath 归一化账号标识：凭据文件路径在同一进程内唯一。
func affinityAccountPath(acc *Account) string {
	if acc == nil {
		return ""
	}
	if acc.Path != "" {
		return acc.Path
	}
	return filepath.Base(acc.Path)
}

// affinityLogSummary 在每次调度后按需记录（debug 级别，不含提示词）。
func affinityLogLine(decision, key, account string) {
	if key == "" {
		return
	}
	log.Printf("[会话亲和] 决策=%s 账号=%s 说明=仅在同资格账号间调整顺序；不记录会话键与提示词", decision, filepath.Base(account))
}

// shortDigest 返回内容的匿名摘要，用于诊断日志（不可逆、不含原文）。
func shortDigest(s string) string {
	if s == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// ---------------------------------------------------------------------------
// 前缀摘要：优先用请求转换阶段已经解析好的结构，避免为算键再扫一遍历史
// ---------------------------------------------------------------------------

type affinityDigestCtxKey struct{}

// withPrefixDigest 把当前请求的前缀摘要挂到 context 上。
func withPrefixDigest(r *http.Request, digest string) *http.Request {
	if r == nil || digest == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), affinityDigestCtxKey{}, digest))
}

func prefixDigestFromContext(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v, ok := r.Context().Value(affinityDigestCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// digestFromRequestObject 用已经解析好的请求对象生成前缀摘要。
//
// 成本只与"首条消息 + tools"成正比，与历史长度无关（不再扫描 messages 数组）。
// 覆盖影响上游缓存的三个要素：model、tools（含顺序）、可缓存前缀的第一条消息。
func digestFromRequestObject(obj map[string]any, model string, budget int) string {
	if obj == nil {
		return ""
	}
	if budget <= 0 {
		budget = affinityDefaultHeadBytes
	}
	h := sha256.New()
	h.Write([]byte("v4\x00"))
	h.Write([]byte(normalizeModelName(model)))
	h.Write([]byte{0})
	if tools, ok := obj["tools"]; ok {
		if b, err := json.Marshal(tools); err == nil && len(b) > 0 {
			if len(b) > budget {
				b = b[:budget]
			}
			h.Write([]byte("tools\x00"))
			h.Write(b)
		}
	}
	if msgs, ok := obj["messages"].([]any); ok && len(msgs) > 0 {
		if b, err := json.Marshal(msgs[0]); err == nil && len(b) > 0 {
			if len(b) > budget {
				b = b[:budget]
			}
			h.Write([]byte("msg0\x00"))
			h.Write(b)
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}
