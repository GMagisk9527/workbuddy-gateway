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
	"container/list"
	"crypto/sha256"
	"encoding/hex"
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
	if prev.Capacity != capacity {
		affinityStoreResize(capacity)
	}
	if !enabled {
		affinityStoreClear()
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

func affinityStoreResize(capacity int) {
	affinityStoreMu.Lock()
	old := affinityStore
	next := newAffinityStore(capacity, old.ttl)
	old.mu.Lock()
	for e := old.lru.Front(); e != nil; e = e.Next() {
		item := e.Value.(*affinityEntry)
		if el, ok := next.items[item.key]; ok {
			next.lru.MoveToFront(el)
			continue
		}
		next.items[item.key] = next.lru.PushFront(item)
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

func affinityStoreClear() {
	affinityStoreMu.Lock()
	old := affinityStore
	next := newAffinityStore(old.cap, old.ttl)
	old.mu.Lock()
	for e := old.lru.Front(); e != nil; e = e.Next() {
		item := e.Value.(*affinityEntry)
		next.items[item.key] = next.lru.PushFront(&affinityEntry{key: item.key, account: item.account, at: item.at})
	}
	old.mu.Unlock()
	affinityStore = next
	affinityStoreMu.Unlock()
	affinityMetrics.reset()
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
	return map[string]any{
		"enabled":      settings.Enabled,
		"mode":         settings.Mode,
		"ttlSeconds":   int(settings.TTL / time.Second),
		"capacity":     settings.Capacity,
		"entries":      affinityStore.stats(),
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
		if digest := promptPrefixDigest(upstreamBody, settings.PrefixHeadByte); digest != "" {
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

// promptPrefixDigest 对上游请求体的前 head 字节做摘要。
//
// 只取头部而不是全量：多轮会话只在尾部追加，头部（system + 工具定义 + 早期历史）
// 是不变量；全量哈希会随上下文线性变慢，且尾部变化本来就不应影响前缀身份。
func promptPrefixDigest(body []byte, head int) string {
	if len(body) < affinityMinPrefixBytes {
		return ""
	}
	if head <= 0 || head > len(body) {
		head = len(body)
	}
	sum := sha256.Sum256(body[:head])
	return hex.EncodeToString(sum[:16])
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
	affinityStoreMu.RUnlock()
	acc, ok := store.get(key, time.Now())
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
	affinityStoreMu.RUnlock()
	store.put(key, accountPath, time.Now())
	affinityMetrics.bound.Add(1)
}

// affinityForget 解除绑定（账号失效/冷却/换号时调用）。
func affinityForget(key string) {
	if key == "" {
		return
	}
	affinityStoreMu.RLock()
	store := affinityStore
	affinityStoreMu.RUnlock()
	store.drop(key)
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
