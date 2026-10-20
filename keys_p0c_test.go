package main

// 最后一轮：WAL 尾部补偿的持久化、账本双文件丢失、删除与在途结算、压缩顺序。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 1) 尾部补偿必须落盘：恢复后不压缩直接再重启，未知消费不能消失也不能翻倍。
func TestTailCompensationSurvivesRepeatedRestarts(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "tail", QuotaTotal: 100})
	writeRawWAL(t, dir, ledgerEntry{Seq: 1, Key: k.ID, Attempt: "ok", Kind: "settle", Credit: 1, Known: true,
		DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	appendRaw(t, dir, `{"seq":2,"key":"`+k.ID+`","attempt":"lost","kind":"settle","credit":99`)

	reloadKeysAndLedger(t, dir) // 第一次：发现半行，补偿为未知
	unknownAfterFirst := func() int64 {
		got := keyByID(k.ID)
		got.mu.Lock()
		defer got.mu.Unlock()
		return got.usage.Unknown
	}()
	if unknownAfterFirst != 1 {
		t.Fatalf("第一次恢复应记 1 次未知，实际 %d", unknownAfterFirst)
	}

	// 不压缩，直接第二次、第三次重启
	reloadKeysAndLedger(t, dir)
	if got := unknownAfterFirst; got != 1 {
		_ = got
	}
	second := func() int64 {
		got := keyByID(k.ID)
		got.mu.Lock()
		defer got.mu.Unlock()
		return got.usage.Unknown
	}()
	if second != 1 {
		t.Fatalf("第二次重启后未知次数应为 1（不能消失也不能重复），实际 %d", second)
	}

	reloadKeysAndLedger(t, dir)
	third := func() int64 {
		got := keyByID(k.ID)
		got.mu.Lock()
		defer got.mu.Unlock()
		return got.usage.Unknown
	}()
	if third != 1 {
		t.Fatalf("第三次重启后未知次数应仍为 1，实际 %d", third)
	}
}

// 1b) lostSettlements 必须能从快照恢复。
func TestLostSettlementsRestoredFromSnapshot(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, _ = mustCreateKey(t, keyCreateOptions{Name: "lost-count"})
	ledger.mu.Lock()
	ledger.lostSettlements = 3
	ledger.mu.Unlock()
	ledgerCompact()
	ledger.mu.Lock()
	ledger.lostSettlements = 0
	ledger.mu.Unlock()

	reloadKeysAndLedger(t, dir)
	ledger.mu.Lock()
	got := ledger.lostSettlements
	ledger.mu.Unlock()
	if got != 3 {
		t.Fatalf("lostSettlements 应从快照恢复为 3，实际 %d", got)
	}
}

// 2) 已经用过的账本文件被删除，不能当成"零消费"重新开始。
func TestLedgerFilesMissingAfterUseFailsClosed(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "vanish", QuotaTotal: 10})
	keySettle(k, "v1", map[string]any{"credit": 5.0}, 200)
	ledgerCompact()

	// 先关闭句柄（Windows 下不关闭无法删除），再模拟两个账本文件同时丢失。
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.mu.Unlock()
	if err := os.Remove(filepath.Join(dir, ledgerWALName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ledgerSnapshotName)); err != nil {
		t.Fatal(err)
	}
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.mu.Unlock()
	if err := keyStoreLoad(); err != nil {
		t.Fatal(err)
	}
	registerRuntimeKeys()
	t.Cleanup(func() { apiKeys.degraded.Store(false) })

	err := ledgerLoad(dir)
	if err == nil && !apiKeys.degraded.Load() {
		t.Fatal("账本文件在用过后丢失，必须 fail closed（报错或 degraded），不能静默归零")
	}
}

// 2b) 全新安装（从未创建过 Key）仍要能正常初始化。
func TestLedgerFreshInstallStillInitializes(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatalf("全新安装不应失败: %v", err)
	}
	if apiKeys.degraded.Load() {
		t.Fatal("全新安装不应进入 degraded")
	}
	ledger.mu.Lock()
	writable := ledger.writable
	ledger.mu.Unlock()
	if !writable {
		t.Fatal("全新安装应创建可写 WAL")
	}
}

// 3) 请求在途时删除 Key，结算仍要保留历史归属。
func TestInFlightSettlementAfterDeleteIsArchived(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "inflight", QuotaTotal: 100})

	keySettle(k, "before-delete", map[string]any{"credit": 2.0}, 200) // 删除前已结算
	id := k.ID
	if err := keyDelete(id); err != nil {
		t.Fatal(err)
	}
	// 在途请求此刻才结算（它早就通过了准入，持有 k）
	keySettle(k, "after-delete", map[string]any{"credit": 3.0}, 200)
	ledgerCompact()

	archived := archivedUsage(id)
	if archived == nil || archived.Total != 5 {
		t.Fatalf("删除后在途结算应并入归档（2+3=5），实际 %+v", archived)
	}

	reloadKeysAndLedger(t, dir)
	if a := archivedUsage(id); a == nil || a.Total != 5 {
		t.Fatalf("重启后归档应仍为 5，实际 %+v", a)
	}
}

// 4) 压缩失败时不能截断 WAL（否则账本后半段丢失）。
func TestCompactionFailureKeepsWAL(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "compact"})
	for i := 0; i < 3; i++ {
		keySettle(k, "c"+string(rune('a'+i)), map[string]any{"credit": 1.0}, 200)
	}
	before, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil {
		t.Fatal(err)
	}

	// 让快照写入失败：把快照路径变成目录
	snapPath := filepath.Join(dir, ledgerSnapshotName)
	if err := os.Mkdir(snapPath, 0o700); err != nil {
		t.Fatal(err)
	}
	ledgerCompact()

	after, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == 0 || len(after) < len(before) {
		t.Fatalf("压缩失败后 WAL 不得被截断：前 %d 字节，后 %d 字节", len(before), len(after))
	}
	_ = os.Remove(snapPath)
}
