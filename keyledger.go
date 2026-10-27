package main

// 追加式消费账本（WAL）+ 定期快照。
//
// 为什么不用 stats.json：stats.json 是每几秒异步整体重写的统计快照，
// 既不能作为准入依据（滞后），也不适合承载"必须准确"的额度累计。
// 这里用「一行一次结算」的追加日志 + 低频快照，重启后按 seq 重放，
// 既保证不丢账，也不会在高频路径上重写大文件。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ledgerSnapshotName = "keyledger.snapshot.json"
	ledgerWALName      = "keyledger.wal"
	ledgerStateName    = "keyledger.state.json"
	ledgerCompactEvery = 256       // 每积累这么多条追加就压缩一次
	ledgerWALMaxBytes  = 512 << 10 // 或 WAL 超过这个大小
)

// ledgerEntry 是 WAL 的一行。语义必须唯一：**只存增量，绝不混用累计值**。
// Kind 区分结算与重置；DayKey/MonthKey 记录该笔消费归属的北京时区窗口，
// 重放时据此决定是否计入"今日/本月"额度，避免跨日跨月错记。
type ledgerEntry struct {
	Seq         int64   `json:"seq"`
	Key         string  `json:"key"`
	Attempt     string  `json:"attempt,omitempty"`
	Kind        string  `json:"kind,omitempty"` // settle（默认）| reset
	Credit      float64 `json:"credit,omitempty"`
	Known       bool    `json:"known,omitempty"`
	Zero        bool    `json:"zero,omitempty"`
	Unknown     bool    `json:"unknown,omitempty"`
	At          int64   `json:"at,omitempty"`
	DayKey      string  `json:"day,omitempty"`
	MonthKey    string  `json:"month,omitempty"`
	InDelta     int64   `json:"in,omitempty"`
	OutDelta    int64   `json:"out,omitempty"`
	CachedDelta int64   `json:"cached,omitempty"`
	Status      int     `json:"status,omitempty"`
}

type ledgerSnapshot struct {
	Version int                     `json:"version"`
	LastSeq int64                   `json:"lastSeq"`
	Keys    map[string]*apiKeyUsage `json:"keys"`
	// Archived 保存已删除 Key 的历史消费（匿名归属），压缩时不会被丢弃。
	Archived map[string]*apiKeyUsage `json:"archived,omitempty"`
	// LostSettlements 记录 WAL 尾部被截断而无法确认的结算笔数（保守计数）。
	LostSettlements int64             `json:"lostSettlements,omitempty"`
	PendingAttempts map[string]string `json:"pendingAttempts,omitempty"`
}

// ledgerDiskState distinguishes an uncompressed WAL from a lost snapshot.
// Incomplete is sticky: restarting or compacting cannot turn missing costs into zero.
type ledgerDiskState struct {
	Version          int  `json:"version"`
	SnapshotRequired bool `json:"snapshotRequired,omitempty"`
	Incomplete       bool `json:"incomplete,omitempty"`
}

type ledgerWALFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

var ledger struct {
	mu              sync.Mutex
	file            ledgerWALFile
	seq             atomic.Int64
	pending         int
	path            string
	writable        bool
	lostSettlements int64 // WAL 尾部被截断、无法确认的结算笔数（保守计数）
	state           ledgerDiskState
	pendingAttempts map[string]string
}

// ledgerLoad 先读快照，再重放 WAL 中比快照更新的条目。
// 快照损坏时返回错误——绝不把损坏文件当成空账本（否则等于免费重置全部额度）。
func ledgerLoad(dir string) error {
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.pending = 0
	ledger.mu.Unlock()
	ledger.path = filepath.Join(dir, ledgerWALName)
	statePath := filepath.Join(dir, ledgerStateName)
	state := ledgerDiskState{}
	stateRaw, stateErr := os.ReadFile(statePath)
	if stateErr == nil {
		if err := json.Unmarshal(stateRaw, &state); err != nil || state.Version != 1 {
			return fmt.Errorf("账本状态标记损坏（%s），拒绝以空账本启动", statePath)
		}
	} else if !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	} else {
		state.Version = 1
	}
	snapPath := filepath.Join(dir, ledgerSnapshotName)
	snap := ledgerSnapshot{}

	data, snapErr := os.ReadFile(snapPath)
	switch {
	case errors.Is(snapErr, os.ErrNotExist):
		// 首次启动：空账本
		snap.Version = 1
	case snapErr != nil:
		return snapErr
	default:
		if err := json.Unmarshal(data, &snap); err != nil {
			return fmt.Errorf("账本快照损坏（%s），拒绝以空账本启动: %w", snapPath, err)
		}
		if snap.Version != 1 {
			return fmt.Errorf("账本快照缺少有效 version=1（%s），拒绝以空账本启动", snapPath)
		}
	}
	if snap.Keys == nil {
		snap.Keys = map[string]*apiKeyUsage{}
	}
	if snap.Archived == nil {
		snap.Archived = map[string]*apiKeyUsage{}
	}
	apiKeys.mu.Lock()
	apiKeys.archive = snap.Archived
	apiKeys.mu.Unlock()
	ledger.seq.Store(snap.LastSeq)
	ledger.pendingAttempts = maps.Clone(snap.PendingAttempts)
	if ledger.pendingAttempts == nil {
		ledger.pendingAttempts = map[string]string{}
	}
	ledger.lostSettlements = snap.LostSettlements
	if snap.LostSettlements > 0 {
		state.Incomplete = true
	}
	if snapErr == nil {
		state.SnapshotRequired = true
	}

	apiKeys.mu.Lock()
	for id, usage := range snap.Keys {
		if k := apiKeys.byID[id]; k != nil && usage != nil {
			k.mu.Lock()
			k.usage = *usage
			k.mu.Unlock()
		} else if usage != nil && apiKeys.archive[id] == nil {
			// Deletion can be persisted before the next ledger compaction.
			// The last snapshot still owns the deleted key's historical costs.
			cp := *usage
			apiKeys.archive[id] = &cp
		}
	}
	apiKeys.mu.Unlock()

	// 先把快照里的窗口对齐到当前北京时间：昨天快照的 Daily/Monthly 必须归零，
	// 但 Total/Lifetime 保持不变。必须在重放之前做，否则重放累积的今日消费
	// 会在最后一步被 rollWindow 一起清掉。
	now := time.Now()
	todayDay, todayMonth := keyDayKey(now), keyMonthKey(now)
	apiKeys.mu.RLock()
	for _, k := range apiKeys.byID {
		if k.legacy {
			continue
		}
		k.mu.Lock()
		k.rollWindowLocked(now)
		k.mu.Unlock()
	}
	apiKeys.mu.RUnlock()

	// 读入 WAL 并定位最后一条完整记录。末尾半行必须物理截断，
	// 否则下一次 O_APPEND 会把新 JSON 接在残缺内容后面，制造"中段损坏"。
	tailLost, tailKeyID := false, ""
	raw, readErr := os.ReadFile(ledger.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	body := raw
	repairWAL := false
	if n := bytes.LastIndexByte(raw, '\n') + 1; n < len(raw) {
		tail := raw[n:]
		var e ledgerEntry
		if err := json.Unmarshal(tail, &e); err == nil {
			// 其实是完整 JSON，只是缺换行：补一个换行即视为完整记录。
			body = raw
			body = append(body, '\n')
			repairWAL = true
		} else {
			// 真正的半行：截断到最后一个换行，并把丢失的这笔结算保守计为未知。
			body = raw[:n]
			repairWAL = true
			tailLost = true
			tailKeyID = extractKeyIDFromBrokenEntry(string(tail))
			if tailKeyID == "" || keyByID(tailKeyID) == nil {
				// 无法归属到具体 Key：不能当作免费，进入安全状态。
				apiKeys.degraded.Store(true)
				state.Incomplete = true
				log.Printf("[账本] 警告=WAL 尾部残缺记录无法归属任一 Key，已进入安全状态（有限额 Key 停止放行）")
			} else {
				log.Printf("[账本] 警告=WAL 尾部有 1 笔无法确认的结算，将按未知费用计入 key=%s（保守处理，不低估额度）", tailKeyID)
			}
		}
	}

	replayed := 0
	firstSeq := int64(0)
	previousSeq := int64(0)
	nextReplay := snap.LastSeq + 1
	if len(body) > 0 {
		lines := strings.Split(string(body), "\n")
		for idx, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e ledgerEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				// 到这里说明损坏不在文件末尾：拒绝启动，绝不静默少计。
				return fmt.Errorf("WAL 第 %d 行损坏，拒绝以部分账本启动: %w", idx+1, err)
			}
			if firstSeq == 0 {
				firstSeq = e.Seq
			}
			if e.Seq <= 0 || (previousSeq > 0 && e.Seq != previousSeq+1) {
				return fmt.Errorf("WAL 第 %d 行序号不连续，拒绝以部分账本启动", idx+1)
			}
			previousSeq = e.Seq
			if e.Seq <= snap.LastSeq {
				continue
			}
			if e.Seq != nextReplay {
				return fmt.Errorf("WAL 第 %d 行缺少快照后的结算序号，拒绝启动", idx+1)
			}
			nextReplay++
			ledgerApply(e)
			if e.Seq > ledger.seq.Load() {
				ledger.seq.Store(e.Seq)
			}
			replayed++
		}
	}
	// A marker written before compaction makes snapshot loss unambiguous. Legacy
	// WAL-only installs can migrate when their history starts at sequence 1.
	if errors.Is(snapErr, os.ErrNotExist) {
		missingHistory := state.SnapshotRequired || (errors.Is(readErr, os.ErrNotExist) && (stateErr == nil || keysEverConfigured()))
		if errors.Is(stateErr, os.ErrNotExist) && keysEverConfigured() && firstSeq != 1 {
			missingHistory = true
		}
		if firstSeq > 1 {
			missingHistory = true
		}
		if missingHistory {
			state.Incomplete = true
			log.Printf("[账本] 严重=账本缺少可信历史基线 业务影响=有限额 Key 停止放行，重启不会解除保护")
		}
	}
	if ledger.lostSettlements > 0 {
		state.Incomplete = true
	}
	if len(ledger.pendingAttempts) > 0 {
		state.Incomplete = true
		log.Printf("[账本] 严重=存在未完成的持久化调用凭据 数量=%d 业务影响=有限额 Key 停止放行，需核对消耗后恢复", len(ledger.pendingAttempts))
	}
	ledger.state = state
	if err := persistLedgerState(dir, state); err != nil {
		apiKeys.degraded.Store(true)
		return err
	}
	if state.Incomplete {
		apiKeys.degraded.Store(true)
	}

	// Repair the tail and its compensation in one atomic replacement. A crash
	// must retain either the broken tail or its durable recovery record.
	if tailLost {
		ev := ledgerEntry{Seq: nextLedgerSeq(), Key: tailKeyID, Kind: "recovery", Unknown: true,
			At: now.UnixMilli(), DayKey: todayDay, MonthKey: todayMonth}
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		body = append(append(body, line...), '\n')
		if err := writeLedgerFileAtomic(ledger.path, body); err != nil {
			apiKeys.degraded.Store(true)
			return err
		}
		ledgerApply(ev)
		replayed++
	} else if repairWAL {
		if err := writeLedgerFileAtomic(ledger.path, body); err != nil {
			return err
		}
	}

	// All repairs and their state are durable before opening the append handle.
	f, err := os.OpenFile(ledger.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	ledger.file = f
	ledger.writable = true
	ledger.pending = replayed

	// 重放结束后固定窗口键（不再调用 rollWindowLocked，避免把刚恢复的今日消费清掉）。
	apiKeys.mu.RLock()
	for _, k := range apiKeys.byID {
		if k.legacy {
			continue
		}
		k.mu.Lock()
		k.usage.DayKey, k.usage.MonthKey = todayDay, todayMonth
		k.mu.Unlock()
	}
	apiKeys.mu.RUnlock()
	if replayed > 0 {
		log.Printf("[账本] 结果=已重放 %d 条消费记录 当前序号=%d", replayed, ledger.seq.Load())
	}
	return nil
}

func persistLedgerState(dir string, state ledgerDiskState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeLedgerFileAtomic(filepath.Join(dir, ledgerStateName), raw)
}

func writeLedgerFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// extractKeyIDFromBrokenEntry 从残缺的 JSON 里尽力取出 key 字段，
// 用于把无法确认的结算保守归到具体 Key 上。
func extractKeyIDFromBrokenEntry(text string) string {
	const marker = `"key"`
	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	colon := strings.Index(rest, ":")
	if colon < 0 || colon > 8 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if len(rest) < 2 || rest[0] != '"' {
		return ""
	}
	end := strings.IndexByte(rest[1:], '"')
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

// ledgerApply 重放一条记录。全部按增量累加；日/月额度只累加归属窗口等于
// 当前窗口的条目，昨天的消费不会算进今天的额度。
func ledgerApply(e ledgerEntry) {
	if e.Kind == "begin" {
		ledger.pendingAttempts[e.Attempt] = e.Key
		return
	}
	delete(ledger.pendingAttempts, e.Attempt)
	if e.Kind == "cancel" {
		return
	}
	apiKeys.mu.RLock()
	k := apiKeys.byID[e.Key]
	apiKeys.mu.RUnlock()
	if k == nil {
		// Key 已删除：真实消费仍要保留匿名归属，不能直接丢弃。
		switch e.Kind {
		case "", "settle":
			archiveMergeSettlement(e, time.UnixMilli(e.At))
		case "recovery":
			ledger.mu.Lock()
			ledger.lostSettlements++
			ledger.mu.Unlock()
			apiKeys.degraded.Store(true)
		}
		return
	}
	now := time.Now()
	todayDay, todayMonth := keyDayKey(now), keyMonthKey(now)
	k.mu.Lock()
	defer k.mu.Unlock()
	if e.Kind == "recovery" {
		k.usage.Unknown++
		if e.At > k.usage.LastUsedAt {
			k.usage.LastUsedAt = e.At
		}
		return
	}
	if e.Kind == "reset" {
		at := time.UnixMilli(e.At)
		// 重置只清准入累计，历史累计与请求次数必须保留。
		keep := apiKeyUsage{
			Lifetime: k.usage.Lifetime, Requests: k.usage.Requests,
			Input: k.usage.Input, Output: k.usage.Output, Cached: k.usage.Cached,
			LastUsedAt: k.usage.LastUsedAt, ZeroCost: k.usage.ZeroCost,
			SpentToday: k.usage.SpentToday, SpentDayKey: k.usage.SpentDayKey,
			DayKey: keyDayKey(at), MonthKey: keyMonthKey(at),
		}
		k.usage = keep
		return
	}
	if e.Known && e.Credit > 0 {
		k.usage.Total += e.Credit
		k.usage.Lifetime += e.Credit
		if e.DayKey == todayDay {
			k.usage.Daily += e.Credit
			k.usage.SpentToday += e.Credit
		}
		if e.MonthKey == todayMonth {
			k.usage.Monthly += e.Credit
		}
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
	if e.At > k.usage.LastUsedAt {
		k.usage.LastUsedAt = e.At
	}
}

func nextLedgerSeq() int64 { return ledger.seq.Add(1) }

// ledgerAppendLocked 在已持有 ledger.mu 时追加一行（不负责加锁）。
func ledgerAppendLocked(e ledgerEntry) error {
	if !ledger.writable || ledger.file == nil {
		return ledgerWriteFailedLocked(errors.New("账本不可写"))
	}
	line, err := json.Marshal(e)
	if err != nil {
		return ledgerWriteFailedLocked(err)
	}
	line = append(line, '\n')
	if n, werr := ledger.file.Write(line); werr != nil || n != len(line) {
		if werr == nil {
			werr = errors.New("WAL 写入不完整")
		}
		return ledgerWriteFailedLocked(werr)
	}
	if err := ledger.file.Sync(); err != nil {
		return ledgerWriteFailedLocked(err)
	}
	if ledger.pendingAttempts == nil {
		ledger.pendingAttempts = map[string]string{}
	}
	if e.Kind == "begin" {
		ledger.pendingAttempts[e.Attempt] = e.Key
	} else {
		delete(ledger.pendingAttempts, e.Attempt)
	}
	ledger.pending++
	return nil
}

func ledgerAppend(e ledgerEntry) {
	ledger.mu.Lock()
	if e.Seq == 0 {
		e.Seq = nextLedgerSeq()
	}
	err := ledgerAppendLocked(e)
	ledger.mu.Unlock()
	if err != nil {
		log.Printf("[账本] 警告=追加写入失败 原因=%v 业务影响=有限额 Key 将停止放行", err)
	}
}

func ledgerAppendReset(keyID string, now time.Time) {
	ledger.mu.Lock()
	e := ledgerEntry{Seq: nextLedgerSeq(), Key: keyID, Kind: "reset", At: now.UnixMilli()}
	_ = ledgerAppendLocked(e)
	ledger.mu.Unlock()
}

// ledgerSnapshotSoon 在重置等低频操作后立即压缩一次，保证重启可见。
func ledgerSnapshotSoon() { ledgerCompact() }

// ledgerCompact 写快照并截断 WAL。全程持 ledger.mu，与结算串行，
// 避免"快照已包含 A、WAL 又写了 A"或"快照没包含 A、WAL 被截断"这两类窗口。
func ledgerCompact() {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if !ledger.writable {
		return
	}
	apiKeys.mu.RLock()
	archived := make(map[string]*apiKeyUsage, len(apiKeys.archive))
	for id, u := range apiKeys.archive {
		cp := *u
		archived[id] = &cp
	}
	snap := ledgerSnapshot{Version: 1, LastSeq: ledger.seq.Load(), Keys: map[string]*apiKeyUsage{}, Archived: archived, LostSettlements: ledger.lostSettlements, PendingAttempts: maps.Clone(ledger.pendingAttempts)}
	// Membership must stay fixed across both collections: deletion transfers a
	// key's usage to archive, and must not fall between these two reads.
	for id, k := range apiKeys.byID {
		k.mu.Lock()
		u := k.usage
		k.mu.Unlock()
		snap.Keys[id] = &u
	}
	apiKeys.mu.RUnlock()

	data, err := json.Marshal(&snap)
	if err != nil {
		apiKeys.degraded.Store(true)
		return
	}
	snapPath := filepath.Join(filepath.Dir(ledger.path), ledgerSnapshotName)
	tmp := snapPath + ".tmp"
	// 1) 写临时文件并 fsync，确保内容真正落盘而不是只在页缓存里
	tf, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=快照临时文件创建失败 原因=%v", err)
		return
	}
	if _, err = tf.Write(data); err == nil {
		err = tf.Sync()
	}
	cerr := tf.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		apiKeys.degraded.Store(true)
		_ = os.Remove(tmp)
		log.Printf("[账本] 警告=快照写入失败 原因=%v（WAL 保持不变，可重放）", err)
		return
	}
	// 2) 原子重命名 + 目录 fsync：目录项落盘后才算快照提交完成
	if err := os.Rename(tmp, snapPath); err != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=快照替换失败 原因=%v（WAL 保持不变，可重放）", err)
		return
	}
	syncDir(filepath.Dir(snapPath))
	// Only after the snapshot is durable can we mark it as required, and only
	// after that marker is durable can we discard the WAL's complete history.
	ledger.state.SnapshotRequired = true
	if ledger.lostSettlements > 0 {
		ledger.state.Incomplete = true
	}
	if err := persistLedgerState(filepath.Dir(ledger.path), ledger.state); err != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=快照状态标记写入失败 原因=%v（WAL 保持不变）", err)
		return
	}
	// 3) 快照提交成功，才允许截断 WAL；先 Sync 保证已提交的行不再丢失
	if ledger.file != nil {
		if err := ledger.file.Sync(); err != nil {
			ledgerWriteFailedLocked(err)
			return
		}
		// Windows append handles do not grant truncation access. Keep the append
		// handle, and truncate through a separate write handle under ledger.mu.
		tf, err := os.OpenFile(ledger.path, os.O_WRONLY, 0o600)
		if err == nil {
			err = tf.Truncate(0)
			if err == nil {
				err = tf.Sync()
			}
			_ = tf.Close()
		}
		if err != nil {
			log.Printf("[账本] 警告=WAL 截断失败 原因=%v（不影响正确性，重放会按 lastSeq 跳过旧条目）", err)
		}
	}
	ledger.pending = 0
}

// ledgerStats 供测试与面板展示。
func ledgerStats() map[string]any {
	ledger.mu.Lock()
	pending := ledger.pending
	ledger.mu.Unlock()
	return map[string]any{
		"seq":      ledger.seq.Load(),
		"pending":  pending,
		"writable": ledger.writable,
		"degraded": apiKeys.degraded.Load(),
	}
}

// keysEverConfigured 报告这个进程是否已经配置过任何鉴权手段（受管 Key 或鉴权开关）。
// 用于区分"首次初始化"与"账本文件被误删"：后者绝不能当成零消费重新开始。
func keysEverConfigured() bool {
	if authSwitchEnabled() {
		return true
	}
	apiKeys.mu.RLock()
	defer apiKeys.mu.RUnlock()
	for _, k := range apiKeys.byID {
		if !k.legacy {
			return true
		}
	}
	return apiKeys.requireAuth
}

// syncDir 把目录项刷到磁盘（Linux/ext4 上重命名后必须做，否则断电可能丢新名字）。
// 非 Unix 平台打开目录会失败，这里按"尽力而为"处理，不把它当成账本错误。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// archiveMergeSettlement 把一笔已删除 Key 的结算并入匿名归档（重启与压缩后仍保留）。
func archiveMergeSettlement(e ledgerEntry, at time.Time) {
	apiKeys.mu.Lock()
	defer apiKeys.mu.Unlock()
	if apiKeys.archive == nil {
		apiKeys.archive = map[string]*apiKeyUsage{}
	}
	u := apiKeys.archive[e.Key]
	if u == nil {
		u = &apiKeyUsage{}
		apiKeys.archive[e.Key] = u
	}
	// Archived entries have no admission calls to roll their windows. Use the
	// current window when replaying historical entries, including across days.
	now := time.Now()
	day, month := keyDayKey(now), keyMonthKey(now)
	rollUsageHistory(u, day)
	if u.DayKey != day {
		u.DayKey, u.Daily = day, 0
	}
	if u.MonthKey != month {
		u.MonthKey, u.Monthly = month, 0
	}
	if e.Known && e.Credit > 0 {
		u.Total += e.Credit
		u.Lifetime += e.Credit
		if e.DayKey == day {
			u.Daily += e.Credit
			u.SpentToday += e.Credit
		}
		if e.MonthKey == month {
			u.Monthly += e.Credit
		}
	}
	if e.Zero {
		u.ZeroCost++
	}
	if e.Unknown {
		u.Unknown++
	}
	u.Requests++
	u.Input += e.InDelta
	u.Output += e.OutDelta
	u.Cached += e.CachedDelta
	if e.At > u.LastUsedAt {
		u.LastUsedAt = e.At
	}
}
