package main

// 追加式消费账本（WAL）+ 定期快照。
//
// 为什么不用 stats.json：stats.json 是每几秒异步整体重写的统计快照，
// 既不能作为准入依据（滞后），也不适合承载"必须准确"的额度累计。
// 这里用「一行一次结算」的追加日志 + 低频快照，重启后按 seq 重放，
// 既保证不丢账，也不会在高频路径上重写大文件。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ledgerSnapshotName = "keyledger.snapshot.json"
	ledgerWALName      = "keyledger.wal"
	ledgerCompactEvery = 256        // 每积累这么多条追加就压缩一次
	ledgerWALMaxBytes  = 512 << 10  // 或 WAL 超过这个大小
)

type ledgerEntry struct {
	Seq     int64   `json:"seq"`
	Key     string  `json:"key"`
	Attempt string  `json:"attempt,omitempty"`
	Credit  float64 `json:"credit,omitempty"`
	Known   bool    `json:"known,omitempty"`
	Reset   bool    `json:"reset,omitempty"`
	At      int64   `json:"at,omitempty"`
	Status  int     `json:"status,omitempty"`
	Input   int64   `json:"input,omitempty"`
	Output  int64   `json:"output,omitempty"`
	Cached  int64   `json:"cached,omitempty"`
}

type ledgerSnapshot struct {
	Version int                  `json:"version"`
	LastSeq int64                `json:"lastSeq"`
	Keys    map[string]*apiKeyUsage `json:"keys"`
}

var ledger struct {
	mu       sync.Mutex
	file     *os.File
	seq      atomic.Int64
	pending  int
	path     string
	writable bool
}

// ledgerLoad 先读快照，再重放 WAL 中比快照更新的条目。
// 快照损坏时返回错误——绝不把损坏文件当成空账本（否则等于免费重置全部额度）。
func ledgerLoad(dir string) error {
	ledger.path = filepath.Join(dir, ledgerWALName)
	snapPath := filepath.Join(dir, ledgerSnapshotName)
	snap := ledgerSnapshot{Version: 1, Keys: map[string]*apiKeyUsage{}}

	data, err := os.ReadFile(snapPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// 首次启动：空账本
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &snap); err != nil {
			return fmt.Errorf("账本快照损坏（%s），拒绝以空账本启动: %w", snapPath, err)
		}
		if snap.Keys == nil {
			snap.Keys = map[string]*apiKeyUsage{}
		}
	}
	ledger.seq.Store(snap.LastSeq)

	apiKeys.mu.RLock()
	for id, usage := range snap.Keys {
		if k := apiKeys.byID[id]; k != nil && usage != nil {
			k.mu.Lock()
			k.usage = *usage
			k.mu.Unlock()
		}
	}
	apiKeys.mu.RUnlock()

	// 重放 WAL
	f, err := os.OpenFile(ledger.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.OpenFile(ledger.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if err != nil {
		return err
	}
	ledger.file = f
	ledger.writable = true

	replayed, dropped := 0, 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ledgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			dropped++ // 半行（断电截断）跳过，不影响其余账目
			continue
		}
		if e.Seq <= snap.LastSeq {
			continue
		}
		ledgerApply(e)
		if e.Seq > ledger.seq.Load() {
			ledger.seq.Store(e.Seq)
		}
		replayed++
	}
	if dropped > 0 {
		log.Printf("[账本] 警告=重放时跳过 %d 行无法解析的记录（多为断电截断）", dropped)
	}
	if replayed > 0 {
		log.Printf("[账本] 结果=已重放 %d 条消费记录 当前序号=%d", replayed, ledger.seq.Load())
	}
	return nil
}

// ledgerApply 把一条记录累加到对应 Key（重放与实时写共用）。
func ledgerApply(e ledgerEntry) {
	apiKeys.mu.RLock()
	k := apiKeys.byID[e.Key]
	apiKeys.mu.RUnlock()
	if k == nil {
		return // Key 已删除：历史仍留在 WAL/快照里，只是不再有归属对象
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if e.Reset {
		k.usage = apiKeyUsage{DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())}
		return
	}
	if e.Known && e.Credit > 0 {
		k.usage.Total += e.Credit
		k.usage.Daily += e.Credit
		k.usage.Monthly += e.Credit
	}
	k.usage.Requests++
	k.usage.Input += e.Input
	k.usage.Output += e.Output
	k.usage.Cached += e.Cached
	if e.At > k.usage.LastUsedAt {
		k.usage.LastUsedAt = e.At
	}
}

func nextLedgerSeq() int64 { return ledger.seq.Add(1) }

// ledgerAppend 只负责落盘；内存累加已由 keySettle 完成。
func ledgerAppend(e ledgerEntry) {
	if e.Seq == 0 {
		e.Seq = nextLedgerSeq()
	}
	if !ledger.writable || ledger.file == nil {
		apiKeys.degraded.Store(true)
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		apiKeys.degraded.Store(true)
		return
	}
	line = append(line, '\n')
	ledger.mu.Lock()
	_, werr := ledger.file.Write(line)
	if werr == nil {
		werr = ledger.file.Sync()
	}
	ledger.pending++
	needCompact := ledger.pending >= ledgerCompactEvery
	ledger.mu.Unlock()
	if werr != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=追加写入失败 原因=%v 业务影响=有限额 Key 将停止放行", werr)
		return
	}
	if needCompact {
		ledgerCompact()
	}
}

func ledgerAppendReset(keyID string, now time.Time) {
	ledgerAppend(ledgerEntry{Seq: nextLedgerSeq(), Key: keyID, Reset: true, At: now.UnixMilli()})
}

// ledgerSnapshotSoon 在重置等低频操作后立即压缩一次，保证重启可见。
func ledgerSnapshotSoon() { ledgerCompact() }

// ledgerCompact 写快照并截断 WAL。快照用临时文件 + 原子重命名。
func ledgerCompact() {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if !ledger.writable {
		return
	}
	snap := ledgerSnapshot{Version: 1, LastSeq: ledger.seq.Load(), Keys: map[string]*apiKeyUsage{}}
	apiKeys.mu.RLock()
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
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=快照写入失败 原因=%v", err)
		return
	}
	if err := os.Rename(tmp, snapPath); err != nil {
		apiKeys.degraded.Store(true)
		log.Printf("[账本] 警告=快照替换失败 原因=%v", err)
		return
	}
	// 快照已包含全部条目：截断 WAL。即使此刻崩溃，重放也会按 lastSeq 跳过旧条目。
	if ledger.file != nil {
		if err := ledger.file.Truncate(0); err == nil {
			_, _ = ledger.file.Seek(0, 0)
		} else {
			log.Printf("[账本] 警告=WAL 截断失败 原因=%v（不影响正确性，只影响文件大小）", err)
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
