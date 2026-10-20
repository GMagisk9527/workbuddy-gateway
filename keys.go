package main

// 多 API Key 管理与积分配额。
//
// 设计要点（与三份参考实现的取舍见最终报告）：
//   - 密钥只以 SHA-256 哈希 + 非敏感前缀落盘，明文只在创建/轮换时返回一次；
//   - 账本用「内存累加 + 追加式 WAL + 定期快照」，不引入 Redis/数据库；
//   - 准入用原子并发占位 + 内存实时余额，结算用上游真实 usage.credit；
//   - 单次请求的费用在结束时才知道，因此额度是「软封顶」：能保证不再放行新请求，
//     无法保证单个已在途请求不超支，这里不假装能硬封顶。

import (
	"crypto/rand"
	"crypto/sha256"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiKeyPrefix         = "wbk_"
	apiKeyRandomBytes    = 32
	apiKeyPrefixLen      = 12 // 展示/检索用前缀长度（含 wbk_）
	legacyKeyID          = "legacy"
	defaultKeyConcurrent = 1
	// 有限额 Key 在连续这么多次拿不到 usage.credit 后进入安全状态，
	// 防止上游长期不返回计费字段导致无限放行。
	defaultUnknownTolerance = 10
	keyWindowZoneOffset     = 8 * 3600
)

var keyCST = time.FixedZone("CST", keyWindowZoneOffset)

var (
	errKeyNotFound  = errors.New("密钥不存在")
	errKeyDuplicate = errors.New("密钥前缀冲突，请重试")
)

// apiKeyUsage 是账本里的可持久化累计值。DayKey/MonthKey 用于北京时间窗口滚动。
type apiKeyUsage struct {
	Total      float64 `json:"total"`
	Daily      float64 `json:"daily"`
	Monthly    float64 `json:"monthly"`
	DayKey     string  `json:"dayKey,omitempty"`
	MonthKey   string  `json:"monthKey,omitempty"`
	Requests   int64   `json:"requests"`
	Input      int64   `json:"inputTokens"`
	Output     int64   `json:"outputTokens"`
	Cached     int64   `json:"cachedTokens"`
	Unknown    int64   `json:"unknownSettlements"`
	ZeroCost   int64   `json:"zeroCostSettlements"`
	// Lifetime 是生命周期累计，管理员的"重置用量"不会清除它；
	// 用于区分"当前准入额度"和"历史真实消费"。
	Lifetime float64 `json:"lifetime,omitempty"`
	LastUsedAt int64   `json:"lastUsedAt,omitempty"`
}

// apiKey 是一把受管密钥。持久化字段与运行时字段分开标注。
type apiKey struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Prefix        string   `json:"prefix"`
	Hash          string   `json:"hash"`
	Enabled       bool     `json:"enabled"`
	CreatedAt     int64    `json:"createdAt"`
	ExpiresAt     int64    `json:"expiresAt,omitempty"`
	Models        []string `json:"models,omitempty"`
	MaxConcurrent int      `json:"maxConcurrent,omitempty"`
	QuotaTotal    float64  `json:"quotaTotal,omitempty"`
	QuotaDaily    float64  `json:"quotaDaily,omitempty"`
	QuotaMonthly  float64  `json:"quotaMonthly,omitempty"`
	Note          string   `json:"note,omitempty"`

	// 运行时（不落盘）：余额、在途并发、最近一次拒绝原因。
	mu         sync.Mutex
	usage      apiKeyUsage
	inflight   int32
	lastReject string
	legacy     bool
}

func (k *apiKey) isLegacy() bool { return k != nil && k.legacy }

// limits 返回该 Key 是否配置了任何额度。
func (k *apiKey) limited() bool {
	return k != nil && !k.legacy && (k.QuotaTotal > 0 || k.QuotaDaily > 0 || k.QuotaMonthly > 0)
}

// concurrency 返回生效的并发上限。
func (k *apiKey) concurrency() int {
	if k == nil || k.legacy {
		return 0 // 0 = 不限制
	}
	if k.MaxConcurrent > 0 {
		return k.MaxConcurrent
	}
	return defaultKeyConcurrent
}

// allowModel 判断模型是否在该 Key 的白名单内（空名单 = 不限）。
func (k *apiKey) allowModel(model string) bool {
	if k == nil || k.legacy {
		return true
	}
	k.mu.Lock()
	models := append([]string(nil), k.Models...)
	k.mu.Unlock()
	return keyModelsAllow(models, model)
}

// keyStoreData 是 apikeys.json 的结构。
type keyStoreData struct {
	Version int       `json:"version"`
	Keys    []*apiKey `json:"keys"`
	// RequireAuth 只要曾经创建过受管 Key 就为 true，且不会因为删光 Key 自动关闭。
	// 否则"禁用/删除最后一把 Key"会意外退回开放模式。
	RequireAuth bool `json:"requireAuth"`
}

// keyStore 管理全部受管密钥。
type keyStore struct {
	mu       sync.RWMutex
	byID     map[string]*apiKey
	byPrefix    map[string]*apiKey
	archive     map[string]*apiKeyUsage
	path        string
	requireAuth bool
	degraded    atomic.Bool // 落盘失败：有限额 Key 一律拒绝，避免无约束放行
}

var apiKeys = &keyStore{byID: map[string]*apiKey{}, byPrefix: map[string]*apiKey{}, archive: map[string]*apiKeyUsage{}}

// keyDataDir 决定账本与密钥文件目录（测试可覆盖）。
var keyDataDir = "."

func keyFilePath(name string) string { return filepath.Join(keyDataDir, name) }

// ---------------------------------------------------------------------------
// 时间窗口：一律用北京时间，不依赖服务器时区
// ---------------------------------------------------------------------------

func keyDayKey(now time.Time) string   { return now.In(keyCST).Format("2006-01-02") }
func keyMonthKey(now time.Time) string { return now.In(keyCST).Format("2006-01") }

// rollWindowLocked 在窗口变化时把日/月累计清零（历史事实在 WAL 里，不删除）。
func (k *apiKey) rollWindowLocked(now time.Time) {
	day, month := keyDayKey(now), keyMonthKey(now)
	if k.usage.DayKey != day {
		k.usage.Daily = 0
		k.usage.DayKey = day
	}
	if k.usage.MonthKey != month {
		k.usage.Monthly = 0
		k.usage.MonthKey = month
	}
}

// ---------------------------------------------------------------------------
// 创建 / 轮换 / 校验
// ---------------------------------------------------------------------------

func newAPIKeySecret() (plain, prefix, hash string, err error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plain = apiKeyPrefix + base32Lower(buf)
	prefix = plain
	if len(prefix) > apiKeyPrefixLen {
		prefix = prefix[:apiKeyPrefixLen]
	}
	sum := sha256.Sum256([]byte(plain))
	return plain, prefix, hex.EncodeToString(sum[:]), nil
}

// base32Lower 用 RFC4648 字母表的小写形式，避免 0/O、1/l 混淆。
func base32Lower(buf []byte) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var out strings.Builder
	var acc uint32
	var bits uint
	for _, b := range buf {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(alphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		out.WriteByte(alphabet[(acc<<(5-bits))&31])
	}
	return out.String()
}

func hashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// keyStoreLoad 从磁盘载入密钥表；文件不存在视为空表。
// 文件存在但损坏时返回错误，绝不当成空表（否则等于免费重置全部额度）。
func keyStoreLoad() error {
	apiKeys.path = keyFilePath("apikeys.json")
	data, err := os.ReadFile(apiKeys.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc keyStoreData
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("apikeys.json 解析失败: %w", err)
	}
	apiKeys.mu.Lock()
	defer apiKeys.mu.Unlock()
	apiKeys.byID = make(map[string]*apiKey, len(doc.Keys))
	apiKeys.byPrefix = make(map[string]*apiKey, len(doc.Keys))
	if apiKeys.archive == nil {
		apiKeys.archive = map[string]*apiKeyUsage{}
	}
	for _, k := range doc.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue
		}
		k.inflight = 0
		apiKeys.byID[k.ID] = k
		apiKeys.byPrefix[k.Prefix] = k
	}
	apiKeys.requireAuth = doc.RequireAuth || len(doc.Keys) > 0
	return nil
}

// keyStorePersistLocked 原子写盘（tmp + rename）。调用方需持写锁。
func (s *keyStore) persistLocked() error {
	if s.path == "" {
		s.path = keyFilePath("apikeys.json")
	}
	doc := keyStoreData{Version: 1, RequireAuth: s.requireAuth, Keys: make([]*apiKey, 0, len(s.byID))}
	for _, k := range s.byID {
		if k.legacy {
			continue // 兼容旧 Key 是运行时身份，不落盘
		}
		doc.Keys = append(doc.Keys, k)
	}
	sort.Slice(doc.Keys, func(i, j int) bool { return doc.Keys[i].CreatedAt < doc.Keys[j].CreatedAt })
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *keyStore) persist() error {
	s.mu.Lock()
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		s.degraded.Store(true)
		log.Printf("[APIKey] 结果=密钥表落盘失败 原因=%v 业务影响=有限额 Key 将拒绝新请求", err)
	}
	return err
}

// keyCreateOptions 是创建/编辑共用的可选项。
type keyCreateOptions struct {
	Name          string   `json:"name"`
	QuotaTotal    float64  `json:"quotaTotal"`
	QuotaDaily    float64  `json:"quotaDaily"`
	QuotaMonthly  float64  `json:"quotaMonthly"`
	ExpiresAt     int64    `json:"expiresAt"`
	Models        []string `json:"models"`
	MaxConcurrent int      `json:"maxConcurrent"`
	Note          string   `json:"note"`
}

func normalizeKeyOptions(in keyCreateOptions) keyCreateOptions {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "未命名密钥"
	}
	if len(in.Name) > 64 {
		in.Name = in.Name[:64]
	}
	if in.Note != "" && len(in.Note) > 200 {
		in.Note = in.Note[:200]
	}

	if in.QuotaTotal < 0 {
		in.QuotaTotal = 0
	}
	if in.QuotaDaily < 0 {
		in.QuotaDaily = 0
	}
	if in.QuotaMonthly < 0 {
		in.QuotaMonthly = 0
	}
	if in.MaxConcurrent < 0 {
		in.MaxConcurrent = 0
	}
	if in.MaxConcurrent > 64 {
		in.MaxConcurrent = 64
	}
	models := make([]string, 0, len(in.Models))
	seen := map[string]bool{}
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		n := normalizeModelName(m)
		if seen[n] {
			continue
		}
		seen[n] = true
		models = append(models, n)
	}
	sort.Strings(models)
	in.Models = models
	return in
}

// keyCreate 创建一把新密钥，返回一次性明文。
func keyCreate(in keyCreateOptions) (string, *apiKey, error) {
	in = normalizeKeyOptions(in)
	plain, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	k := &apiKey{
		ID: "key_" + hex.EncodeToString(randBytes(8)), Name: in.Name,
		Prefix: prefix, Hash: hash, Enabled: true, CreatedAt: now.UnixMilli(),
		ExpiresAt: in.ExpiresAt, Models: in.Models, MaxConcurrent: in.MaxConcurrent,
		QuotaTotal: in.QuotaTotal, QuotaDaily: in.QuotaDaily, QuotaMonthly: in.QuotaMonthly,
		Note: in.Note,
	}
	k.usage.DayKey = keyDayKey(now)
	k.usage.MonthKey = keyMonthKey(now)

	apiKeys.mu.Lock()
	if _, exists := apiKeys.byPrefix[prefix]; exists {
		apiKeys.mu.Unlock()
		return "", nil, errKeyDuplicate
	}
	apiKeys.byID[k.ID] = k
	apiKeys.byPrefix[prefix] = k
	apiKeys.requireAuth = true
	err = apiKeys.persistLocked()
	apiKeys.mu.Unlock()
	if err != nil {
		apiKeys.degraded.Store(true)
		return "", nil, err
	}
	ensureAuthSwitchOn(keyDataDir)
	log.Printf("[APIKey] 结果=已创建 id=%s 前缀=%s 总额度=%.1f 日额度=%.1f 月额度=%.1f 并发=%d 说明=明文只在此处生成，不落盘",
		k.ID, k.Prefix, k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly, k.concurrency())
	return plain, k, nil
}

func randBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败属于系统性故障；退化为时间派生值仍能保证流程继续，
		// 但会记录告警，避免静默降级。
		log.Printf("[APIKey] 警告=系统随机源不可用，已退化为时间派生标识")
		now := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	return buf
}

// keyRotate 重新生成明文：旧密钥立即失效。
func keyRotate(id string) (string, *apiKey, error) {
	plain, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		return "", nil, err
	}
	apiKeys.mu.Lock()
	k := apiKeys.byID[id]
	if k == nil {
		apiKeys.mu.Unlock()
		return "", nil, errKeyNotFound
	}
	if other := apiKeys.byPrefix[prefix]; other != nil && other != k {
		apiKeys.mu.Unlock()
		return "", nil, errKeyDuplicate
	}
	k.mu.Lock()
	oldPrefix := k.Prefix
	k.Prefix, k.Hash = prefix, hash
	k.mu.Unlock()
	delete(apiKeys.byPrefix, oldPrefix)
	apiKeys.byPrefix[prefix] = k
	err = apiKeys.persistLocked()
	apiKeys.mu.Unlock()
	if err != nil {
		apiKeys.degraded.Store(true)
		return "", nil, err
	}
	log.Printf("[APIKey] 结果=已轮换 id=%s 新前缀=%s 说明=旧密钥立即失效", id, prefix)
	return plain, k, nil
}

func keyUpdate(id string, in keyCreateOptions) (*apiKey, error) {
	in = normalizeKeyOptions(in)
	apiKeys.mu.Lock()
	k := apiKeys.byID[id]
	if k == nil {
		apiKeys.mu.Unlock()
		return nil, errKeyNotFound
	}
	k.mu.Lock()
	k.Name, k.ExpiresAt, k.Models = in.Name, in.ExpiresAt, in.Models
	k.MaxConcurrent, k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly = in.MaxConcurrent, in.QuotaTotal, in.QuotaDaily, in.QuotaMonthly
	k.Note = in.Note
	k.mu.Unlock()
	err := apiKeys.persistLocked()
	apiKeys.mu.Unlock()
	if err != nil {
		apiKeys.degraded.Store(true)
		return nil, err
	}
	log.Printf("[APIKey] 结果=已更新 id=%s 总额度=%.1f 日额度=%.1f 月额度=%.1f 并发=%d", id, in.QuotaTotal, in.QuotaDaily, in.QuotaMonthly, in.MaxConcurrent)
	return k, nil
}

func keySetEnabled(id string, enabled bool) error {
	apiKeys.mu.Lock()
	k := apiKeys.byID[id]
	if k == nil {
		apiKeys.mu.Unlock()
		return errKeyNotFound
	}
	k.mu.Lock()
	k.Enabled = enabled
	k.mu.Unlock()
	err := apiKeys.persistLocked()
	apiKeys.mu.Unlock()
	if err != nil {
		apiKeys.degraded.Store(true)
		return err
	}
	log.Printf("[APIKey] 结果=%s id=%s", map[bool]string{true: "已启用", false: "已禁用"}[enabled], id)
	return nil
}

func keyDelete(id string) error {
	apiKeys.mu.Lock()
	k := apiKeys.byID[id]
	if k == nil {
		apiKeys.mu.Unlock()
		return errKeyNotFound
	}
	delete(apiKeys.byID, id)
	delete(apiKeys.byPrefix, k.Prefix)
	// 历史消费转为匿名归档：删除 Key 不能抹掉已经发生的消费。
	k.mu.Lock()
	archived := k.usage
	k.mu.Unlock()
	if apiKeys.archive == nil {
		apiKeys.archive = map[string]*apiKeyUsage{}
	}
	apiKeys.archive[id] = &archived
	err := apiKeys.persistLocked()
	apiKeys.mu.Unlock()
	if err != nil {
		apiKeys.degraded.Store(true)
		return err
	}
	// 账本里的历史消费按 Key ID 保留，不随删除消失。
	log.Printf("[APIKey] 结果=已删除 id=%s 说明=账本历史保留匿名归属，统计不丢失", id)
	return nil
}

// keyResetUsage 清零额度累计；历史消费事实仍保留在 WAL 中。
func keyResetUsage(id string) error {
	k := keyByID(id)
	if k == nil {
		return errKeyNotFound
	}
	now := time.Now()
	// 与结算使用同一把锁、同一顺序：先写 reset 事件，再清内存。
	// 写失败就返回错误、不动内存——绝不能假报成功，也不能让内存与 WAL 分叉。
	ledger.mu.Lock()
	e := ledgerEntry{Seq: nextLedgerSeq(), Key: id, Kind: "reset", At: now.UnixMilli()}
	werr := ledgerAppendLocked(e)
	if werr != nil {
		ledger.mu.Unlock()
		log.Printf("[APIKey] 结果=重置失败 id=%s 原因=%v 业务影响=额度累计保持不变", id, werr)
		return werr
	}
	k.mu.Lock()
	keep := apiKeyUsage{
		Lifetime: k.usage.Lifetime, Requests: k.usage.Requests,
		Input: k.usage.Input, Output: k.usage.Output, Cached: k.usage.Cached,
		LastUsedAt: k.usage.LastUsedAt,
		DayKey:     keyDayKey(now), MonthKey: keyMonthKey(now),
	}
	k.usage = keep
	k.lastReject = ""
	k.mu.Unlock()
	ledger.mu.Unlock()

	ledgerCompact()
	log.Printf("[APIKey] 结果=已重置用量 id=%s 说明=仅清零准入累计，Lifetime 与历史消费记录保留", id)
	return nil
}
// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

// keyIdentity 是挂在请求上下文里的稳定身份。它独立于 statsAttempt，
// 因此换号重试（statsSelectAccount 会重置 attempt）不会丢失归属。
type keyIdentity struct {
	ID     string
	Prefix string
	Legacy bool
	Label  string
}

type keyIdentityContextKey struct{}

// keyIdentityHolder 让鉴权层写入的身份能被**外层中间件**看到。
// 中间件链是「审计(外层) -> CORS -> 鉴权(内层)」，鉴权返回的是新 request，
// 外层拿到的仍是旧 request；如果把身份只放在 request 上下文里，结算时就读不到，
// 结果就是"请求成功但一分钱没记"。这里在审计层放一个可变持有者，鉴权层填进去。
type keyIdentityHolder struct {
	mu  sync.RWMutex
	id  *keyIdentity
	key *apiKey // 准入成功时的结算引用：删除/轮换后仍能把在途请求记到正确归属
}

type keyIdentityHolderKey struct{}

func keyIdentityFor(r *http.Request) *keyIdentity {
	if r == nil {
		return nil
	}
	if id, ok := r.Context().Value(keyIdentityContextKey{}).(*keyIdentity); ok && id != nil {
		return id
	}
	if h, ok := r.Context().Value(keyIdentityHolderKey{}).(*keyIdentityHolder); ok && h != nil {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.id
	}
	return nil
}

func withKeyIdentity(r *http.Request, id *keyIdentity) *http.Request {
	if h, ok := r.Context().Value(keyIdentityHolderKey{}).(*keyIdentityHolder); ok && h != nil {
		h.mu.Lock()
		h.id = id
		h.mu.Unlock()
	}
	return r.WithContext(context.WithValue(r.Context(), keyIdentityContextKey{}, id))
}

// authenticateAPIKey 按前缀定位 + 哈希定长比较。找不到返回 nil。
func authenticateAPIKey(token string) *apiKey {
	token = strings.TrimSpace(token)
	if token == "" || !strings.HasPrefix(token, apiKeyPrefix) {
		return nil
	}
	prefix := token
	if len(prefix) > apiKeyPrefixLen {
		prefix = prefix[:apiKeyPrefixLen]
	}
	apiKeys.mu.RLock()
	k := apiKeys.byPrefix[prefix]
	var wantHash string
	if k != nil {
		wantHash = k.Hash // 与轮换共享同一把锁，避免读到新旧混合状态
	}
	apiKeys.mu.RUnlock()
	if k == nil {
		return nil
	}
	want := hashAPIKey(token)
	if subtle.ConstantTimeCompare([]byte(want), []byte(wantHash)) != 1 {
		return nil
	}
	return k
}

// ---------------------------------------------------------------------------
// 准入
// ---------------------------------------------------------------------------

// keyAdmitError 描述一次准入拒绝。
type keyAdmitError struct {
	Status  int
	Code    string
	Message string
}

// keyAdmit 做「启用/过期/额度/并发」四道检查，并占用一个并发位。
// 返回的 release 必须且只能调用一次（幂等）。
func keyAdmit(k *apiKey, model string) (*keyAdmitError, func()) {
	if k == nil {
		return &keyAdmitError{401, "invalid_api_key", "未提供有效 API 密钥"}, nil
	}
	if k.legacy {
		// 兼容旧 Key：不受额度与并发限制，行为与升级前一致。
		return nil, func() {}
	}
	now := time.Now()
	// 配置字段一律在 k.mu 下读取：管理接口会并发修改这些字段。
	k.mu.Lock()
	enabled := k.Enabled
	expires := k.ExpiresAt
	models := append([]string(nil), k.Models...)
	limit := k.MaxConcurrent
	total, daily, monthly := k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly
	k.mu.Unlock()
	if limit <= 0 {
		limit = defaultKeyConcurrent
	}

	if !enabled {
		return &keyAdmitError{403, "key_disabled", "该 API Key 已被禁用"}, nil
	}
	if expires > 0 && now.UnixMilli() >= expires {
		return &keyAdmitError{403, "key_expired", "该 API Key 已过期"}, nil
	}
	// 模型未知（准入层拿不到请求体）时不判定白名单；白名单由各协议 handler
	// 在解析出 model 之后用 keyModelAllowed 检查。否则空模型会拦掉所有请求。
	if model != "" && !keyModelsAllow(models, model) {
		return &keyAdmitError{403, "model_not_allowed", "该 API Key 无权使用模型 " + normalizeModelName(model)}, nil
	}
	limited := total > 0 || daily > 0 || monthly > 0
	if limited && apiKeys.degraded.Load() {
		return &keyAdmitError{503, "ledger_unavailable", "记账存储不可用，已暂停该 Key 的请求"}, nil
	}

	// 并发位：CAS 占位，失败即视为超限。
	if limit > 0 {
		for {
			cur := atomic.LoadInt32(&k.inflight)
			if int(cur) >= limit {
				k.setReject("concurrency")
				return &keyAdmitError{429, "key_concurrency_exceeded", fmt.Sprintf("该 API Key 并发上限为 %d，请稍后重试", limit)}, nil
			}
			if atomic.CompareAndSwapInt32(&k.inflight, cur, cur+1) {
				break
			}
		}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if limit > 0 {
				atomic.AddInt32(&k.inflight, -1)
			}
		})
	}

	// 额度：用内存实时累计（结算时立即更新），不依赖异步落盘的 stats.json。
	k.mu.Lock()
	k.rollWindowLocked(now)
	usage := k.usage
	k.lastReject = ""
	k.mu.Unlock()

	if limited {
		if total > 0 && usage.Total >= total {
			release()
			k.setReject("total")
			return &keyAdmitError{429, "credit_quota_exhausted", "该 API Key 的总积分额度已用尽"}, nil
		}
		if daily > 0 && usage.Daily >= daily {
			release()
			k.setReject("daily")
			return &keyAdmitError{429, "credit_quota_exhausted", "该 API Key 的今日积分额度已用尽"}, nil
		}
		if monthly > 0 && usage.Monthly >= monthly {
			release()
			k.setReject("monthly")
			return &keyAdmitError{429, "credit_quota_exhausted", "该 API Key 的本月积分额度已用尽"}, nil
		}
		if usage.Unknown >= defaultUnknownTolerance {
			release()
			k.setReject("unknown")
			return &keyAdmitError{429, "credit_quota_exhausted", "上游长期未返回计费字段，该 Key 已进入安全状态，请联系管理员"}, nil
		}
	}
	return nil, release
}

// keyModelsAllow 判断模型是否在给定白名单内（空名单 = 不限）。
func keyModelsAllow(models []string, model string) bool {
	if len(models) == 0 {
		return true
	}
	name := normalizeModelName(model)
	for _, allowed := range models {
		if normalizeModelName(allowed) == name {
			return true
		}
	}
	return false
}
func (k *apiKey) setReject(reason string) {
	k.mu.Lock()
	k.lastReject = reason
	k.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 结算
// ---------------------------------------------------------------------------

// keySettleOutcome 描述一次结算的真实结果，便于日志与测试区分。
type keySettleOutcome string

const (
	settleCharged keySettleOutcome = "charged" // usage.credit > 0
	settleZero    keySettleOutcome = "zero"    // 明确 0
	settleUnknown keySettleOutcome = "unknown" // 字段缺失/非法
	settleDup     keySettleOutcome = "duplicate"
)

// keySettle 按上游真实 usage.credit 记账。
// attemptID 是幂等键：同一 attempt 重复调用只会记账一次。
func keySettle(k *apiKey, attemptID string, usage map[string]any, status int) keySettleOutcome {
	if k == nil || attemptID == "" {
		return settleUnknown
	}
	if !markSettled(attemptID) {
		return settleDup
	}
	credit, hasCredit := usageCredit(usage)
	valid := hasCredit && credit >= 0 && !isBadFloat(credit)
	now := time.Now()

	e := ledgerEntry{
		Key: k.ID, Attempt: attemptID, Kind: "settle", Status: status,
		At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now),
	}
	var outcome keySettleOutcome
	switch {
	case valid && credit > 0:
		e.Credit, e.Known = credit, true
		outcome = settleCharged
	case valid:
		e.Zero = true
		outcome = settleZero
	default:
		e.Unknown = true
		outcome = settleUnknown
	}
	if v := statsTokenValue(usage, "prompt_tokens"); v != nil {
		e.InDelta = *v
	}
	if v := statsTokenValue(usage, "completion_tokens"); v != nil {
		e.OutDelta = *v
	}
	if cached, _, ok := statsCachedValue(usage); ok {
		e.CachedDelta = cached
	}

	// 顺序固定为「分配序号 -> 落盘 -> 更新内存」，全程持 ledger.mu 并与压缩互斥：
	//   - 先落盘再改内存：即使写入后立刻崩溃，重放也能把这笔账补回来；
	//   - 与压缩互斥：快照不会拍到"WAL 有、内存没有"的中间态。
	ledger.mu.Lock()
	e.Seq = nextLedgerSeq()
	werr := ledgerAppendLocked(e)
	k.mu.Lock()
	k.rollWindowLocked(now)
	if e.Known {
		k.usage.Total += e.Credit
		k.usage.Daily += e.Credit
		k.usage.Monthly += e.Credit
		k.usage.Lifetime += e.Credit
	}
	if e.Zero {
		k.usage.ZeroCost++
	}
	if e.Unknown {
		k.usage.Unknown++
	}
	k.usage.Requests++
	k.usage.Input += e.InDelta
	k.usage.Output += e.OutDelta
	k.usage.Cached += e.CachedDelta
	k.usage.LastUsedAt = now.UnixMilli()
	unknownNow := k.usage.Unknown
	k.mu.Unlock()
	// Key 可能在请求在途时被删除：真实消费仍要并入匿名归档，不能丢账。
	if keyByID(k.ID) == nil {
		archiveMergeSettlement(e, now)
	}
	needCompact := ledger.pending >= ledgerCompactEvery
	ledger.mu.Unlock()

	if werr != nil {
		log.Printf("[账本] 警告=结算落盘失败 key=%s attempt=%s 原因=%v 业务影响=内存已记，有限额 Key 已停止放行", k.ID, attemptID, werr)
	}
	if needCompact {
		ledgerCompact()
	}
	k.mu.Lock()
	limited := k.QuotaTotal > 0 || k.QuotaDaily > 0 || k.QuotaMonthly > 0
	k.mu.Unlock()
	if outcome == settleUnknown && limited {
		log.Printf("[APIKey] 警告=本次未获得计费字段 key=%s attempt=%s 业务影响=累计未知次数=%d，达到 %d 次后停止放行",
			k.ID, attemptID, unknownNow, defaultUnknownTolerance)
	}
	return outcome
}
func isBadFloat(v float64) bool { return v != v || v > 1e308 || v < -1e308 }

// settledAttempts 保存进程内已结算的 attempt，避免重复记账。
var (
	settledMu       sync.Mutex
	settledIDs      = map[string]struct{}{}
	settledOrder    []string
	settledCapacity = 8192
)

func markSettled(id string) bool {
	settledMu.Lock()
	defer settledMu.Unlock()
	if _, ok := settledIDs[id]; ok {
		return false
	}
	settledIDs[id] = struct{}{}
	settledOrder = append(settledOrder, id)
	if len(settledOrder) > settledCapacity {
		drop := len(settledOrder) - settledCapacity
		for _, old := range settledOrder[:drop] {
			delete(settledIDs, old)
		}
		settledOrder = settledOrder[drop:]
	}
	return true
}

// ---------------------------------------------------------------------------
// 对外快照
// ---------------------------------------------------------------------------

// keyUsageView 是 /v1/usage 与面板共用的只读视图。
type keyUsageView struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Legacy    bool     `json:"legacy"`
	Used      float64  `json:"used"`
	Lifetime  float64  `json:"lifetime"` // 历史累计，不随重置清零
	Limit     *float64 `json:"limit"`
	Remaining *float64 `json:"remaining"`
	DailyUsed float64  `json:"dailyUsed"`
	DailyLeft *float64 `json:"dailyRemaining"`
	MonthUsed float64  `json:"monthlyUsed"`
	MonthLeft *float64 `json:"monthlyRemaining"`
	Requests  int64    `json:"requests"`
	Input     int64    `json:"inputTokens"`
	Output    int64    `json:"outputTokens"`
	Cached    int64    `json:"cachedTokens"`
	Unknown   int64    `json:"unknownSettlements"`
	ZeroCost  int64    `json:"zeroCostSettlements"`
	LastUsed  int64    `json:"lastUsedAt,omitempty"`
	ExpiresAt int64    `json:"expiresAt,omitempty"`
	DayKey    string   `json:"dayKey"`
	MonthKey  string   `json:"monthKey"`
}

func limitPtr(limit, used float64) (*float64, *float64) {
	if limit <= 0 {
		return nil, nil // 不限额：null，而不是 0
	}
	l := limit
	left := limit - used
	if left < 0 {
		left = 0
	}
	return &l, &left
}

func keyUsageSnapshot(k *apiKey) keyUsageView {
	if k == nil {
		return keyUsageView{Status: "missing"}
	}
	now := time.Now()
	k.mu.Lock()
	k.rollWindowLocked(now)
	u := k.usage
	quotaTotal, quotaDaily, quotaMonthly := k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly
	enabled, expires, name := k.Enabled, k.ExpiresAt, k.Name
	k.mu.Unlock()
	view := keyUsageView{
		Name: name, Legacy: k.legacy, Used: u.Total, Lifetime: u.Lifetime, DailyUsed: u.Daily, MonthUsed: u.Monthly,
		Requests: u.Requests, Input: u.Input, Output: u.Output, Cached: u.Cached,
		Unknown: u.Unknown, ZeroCost: u.ZeroCost, LastUsed: u.LastUsedAt,
		ExpiresAt: expires, DayKey: u.DayKey, MonthKey: u.MonthKey,
	}
	view.Limit, view.Remaining = limitPtr(quotaTotal, u.Total)
	_, view.DailyLeft = limitPtr(quotaDaily, u.Daily)
	view.MonthLeft = nil
	if quotaMonthly > 0 {
		l := quotaMonthly
		left := l - u.Monthly
		if left < 0 {
			left = 0
		}
		view.MonthLeft = &left
	}
	limited := quotaTotal > 0 || quotaDaily > 0 || quotaMonthly > 0
	switch {
	case enabled == false:
		view.Status = "disabled"
	case expires > 0 && now.UnixMilli() >= expires:
		view.Status = "expired"
	case limited && view.Remaining != nil && *view.Remaining <= 0:
		view.Status = "exhausted"
	case limited && view.DailyLeft != nil && *view.DailyLeft <= 0:
		view.Status = "exhausted"
	case limited && view.MonthLeft != nil && *view.MonthLeft <= 0:
		view.Status = "exhausted"
	default:
		view.Status = "active"
	}
	return view
}

// keyList 返回按创建时间排序的密钥副本（用于管理面板）。
func keyList() []*apiKey {
	apiKeys.mu.RLock()
	out := make([]*apiKey, 0, len(apiKeys.byID))
	for _, k := range apiKeys.byID {
		out = append(out, k)
	}
	apiKeys.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func keyByID(id string) *apiKey {
	if id == legacyKeyID {
		return legacyKey // 兼容旧 Key 不依赖注册顺序
	}
	apiKeys.mu.RLock()
	defer apiKeys.mu.RUnlock()
	return apiKeys.byID[id]
}

// legacyKey 是配置里 gateway.apiKey 的运行时身份：始终可用、不受额度与并发限制。
// 它只用于归属与用量展示，不参与持久化的密钥表。
var legacyKey = &apiKey{ID: legacyKeyID, Name: "兼容旧 Key", Prefix: "config.apiKey", Enabled: true, legacy: true}

func legacyKeyIdentity() *apiKey { return legacyKey }

// registerRuntimeKeys 把运行时身份放进索引（不写盘）。
func registerRuntimeKeys() {
	apiKeys.mu.Lock()
	apiKeys.byID[legacyKeyID] = legacyKey
	apiKeys.mu.Unlock()
}
// archivedUsage 返回已删除 Key 的历史消费（匿名归档）。
func archivedUsage(id string) *apiKeyUsage {
	apiKeys.mu.RLock()
	defer apiKeys.mu.RUnlock()
	if u := apiKeys.archive[id]; u != nil {
		cp := *u
		return &cp
	}
	return nil
}
// withSettlementKey 把准入通过的 Key 绑定到请求上。结算时优先用它，
// 而不是重新查索引——否则管理员在请求在途时删除 Key，这笔真实消费会直接丢失。
func withSettlementKey(r *http.Request, k *apiKey) {
	if r == nil || k == nil {
		return
	}
	if h, ok := r.Context().Value(keyIdentityHolderKey{}).(*keyIdentityHolder); ok && h != nil {
		h.mu.Lock()
		h.key = k
		h.mu.Unlock()
	}
}

// settlementKeyFor 返回本次请求的结算引用（可能指向已被删除的 Key）。
func settlementKeyFor(r *http.Request) *apiKey {
	if r == nil {
		return nil
	}
	if h, ok := r.Context().Value(keyIdentityHolderKey{}).(*keyIdentityHolder); ok && h != nil {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.key
	}
	return nil
}