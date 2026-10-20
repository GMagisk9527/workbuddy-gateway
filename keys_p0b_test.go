package main

// 第三轮审计：跨日恢复、WAL 截断尾部、重置线性化、剩余竞争。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeSnapshotFile(t *testing.T, dir string, snap ledgerSnapshot) {
	t.Helper()
	if snap.Version == 0 {
		snap.Version = 1
	}
	raw, err := json.Marshal(&snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ledgerSnapshotName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// P0-1：快照来自昨天，WAL 里是今天的消费，重启后今天的额度不能被清零。
func TestSnapshotFromYesterdayKeepsTodayUsage(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "cross-day", QuotaDaily: 100})

	now := time.Now().In(keyCST)
	yesterday := now.AddDate(0, 0, -1)
	// 快照：昨天窗口，已经用掉 5 分
	writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 1, Keys: map[string]*apiKeyUsage{
		k.ID: {Total: 5, Daily: 5, Monthly: 5, DayKey: keyDayKey(yesterday), MonthKey: keyMonthKey(now), Requests: 1},
	}})
	// WAL：今天又用了 20 分
	writeRawWAL(t, dir, ledgerEntry{
		Seq: 2, Key: k.ID, Attempt: "today", Kind: "settle", Credit: 20, Known: true,
		At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now), InDelta: 10,
	})

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	u := got.usage
	got.mu.Unlock()
	if u.Daily != 20 {
		t.Fatalf("今天重放的 20 分被清零了：Daily=%v DayKey=%s", u.Daily, u.DayKey)
	}
	if u.Total != 25 {
		t.Fatalf("总额应为 5+20=25，实际 %v", u.Total)
	}
}

// P0-1：跨月同理。
func TestSnapshotFromLastMonthKeepsThisMonthUsage(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "cross-month", QuotaMonthly: 100})

	now := time.Now().In(keyCST)
	lastMonth := now.AddDate(0, -1, 0)
	writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 1, Keys: map[string]*apiKeyUsage{
		k.ID: {Total: 7, Daily: 0, Monthly: 7, DayKey: keyDayKey(now), MonthKey: keyMonthKey(lastMonth)},
	}})
	writeRawWAL(t, dir, ledgerEntry{
		Seq: 2, Key: k.ID, Attempt: "m", Kind: "settle", Credit: 30, Known: true,
		At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now),
	})

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	monthly := got.usage.Monthly
	got.mu.Unlock()
	if monthly != 30 {
		t.Fatalf("本月重放的 30 分被清零了：Monthly=%v", monthly)
	}
}

// P0-1：旧快照 + 跨日多笔 + 中途重置，三者混合重放。
func TestReplayMixOfCrossDayAndReset(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "mix", QuotaDaily: 100})

	now := time.Now().In(keyCST)
	yesterday := now.AddDate(0, 0, -1)
	writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 1, Keys: map[string]*apiKeyUsage{
		k.ID: {Total: 50, Daily: 50, DayKey: keyDayKey(yesterday), MonthKey: keyMonthKey(now)},
	}})
	writeRawWAL(t, dir,
		ledgerEntry{Seq: 2, Key: k.ID, Attempt: "a", Kind: "settle", Credit: 3, Known: true,
			At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)},
		ledgerEntry{Seq: 3, Key: k.ID, Attempt: "b", Kind: "settle", Credit: 4, Known: true,
			At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)},
	)

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	daily := got.usage.Daily
	got.mu.Unlock()
	if daily != 7 {
		t.Fatalf("两笔今日消费应合计 7，实际 %v", daily)
	}
}

// P0-2：WAL 末尾半行必须在启动时被截断，否则下一次追加会拼出坏行。
func TestTruncatedWALTailIsRepaired(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "tail"})

	writeRawWAL(t, dir, ledgerEntry{Seq: 1, Key: k.ID, Attempt: "ok", Kind: "settle", Credit: 1, Known: true,
		DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	appendRaw(t, dir, `{"seq":2,"key":"`+k.ID+`","attempt":"half`) // 断电截断，无换行

	reloadKeysAndLedger(t, dir) // 第一次恢复：允许丢弃半行
	if authenticateAPIKey(plain) == nil {
		t.Fatal("恢复后密钥应仍有效")
	}
	// 恢复后在同一个 WAL 上继续结算
	again := keyByID(k.ID)
	keySettle(again, "after-tail", map[string]any{"credit": 2.0}, 200)
	ledgerCompact()

	reloadKeysAndLedger(t, dir) // 第二次重启：不能因为坏行失败
	got := keyByID(k.ID)
	got.mu.Lock()
	total := got.usage.Total
	got.mu.Unlock()
	if total != 3 {
		t.Fatalf("两次结算应合计 3，实际 %v（说明尾部半行污染了新记录）", total)
	}
}

// P0-2：被丢弃的未确认消费不能静默低估——应记为未知而不是当作没发生。
func TestTruncatedTailCountsAsUnknown(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "lost", QuotaTotal: 100})
	writeRawWAL(t, dir, ledgerEntry{Seq: 1, Key: k.ID, Attempt: "ok", Kind: "settle", Credit: 1, Known: true,
		DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	appendRaw(t, dir, `{"seq":2,"key":"`+k.ID+`","attempt":"lost","kind":"settle","credit":99`)

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	unknown := got.usage.Unknown
	got.mu.Unlock()
	if unknown != 1 {
		t.Fatalf("被截断的结算应记为 1 次未知消费（保守），实际 %d", unknown)
	}
}

// P1-1：重置与结算并发时，内存最终值必须与 WAL 重放结果一致。
func TestResetAndSettleStayConsistent(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "reset-race", QuotaTotal: 1000})

	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				keySettle(k, "att-"+string(rune('a'+i))+"-"+string(rune('0'+j%10)), map[string]any{"credit": 1.0}, 200)
			}
		}(i)
	}
	time.Sleep(5 * time.Millisecond)
	if err := keyResetUsage(k.ID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	k.mu.Lock()
	memTotal := k.usage.Total
	memReq := k.usage.Requests
	k.mu.Unlock()

	reloadKeysAndLedger(t, dir)
	_ = plain
	got := keyByID(k.ID)
	got.mu.Lock()
	diskTotal, diskReq := got.usage.Total, got.usage.Requests
	got.mu.Unlock()
	if memTotal != diskTotal || memReq != diskReq {
		t.Fatalf("重置与结算并发后内存与重放不一致: 内存=%v/%d 重放=%v/%d", memTotal, memReq, diskTotal, diskReq)
	}
}

// P1-1：重置写盘失败不能假报成功。
func TestResetReportsWriteFailure(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "fail"})
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.mu.Unlock()
	t.Cleanup(func() { ledger.writable = false })

	if err := keyResetUsage(k.ID); err == nil {
		t.Fatal("账本不可写时重置必须报错，不能假报成功")
	}
}

// P1-2：并发鉴权 + 轮换 + 改白名单 + /v1/models。
func TestAuthRotateAndModelFilterConcurrent(t *testing.T) {
	resetKeyWorld(t)
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "rot", Models: []string{"model-a"}})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _, _ = keyRotate(k.ID)
			_, _ = keyUpdate(k.ID, keyCreateOptions{Name: "rot", Models: []string{"model-a", "model-b"}})
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = authenticateAPIKey(plain)
				_ = k.allowModel("model-a")
				req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				req = withKeyIdentity(req, &keyIdentity{ID: k.ID})
				f := keyAllowedModelFilter(req)
				_ = f("model-a")
				_ = keyUsageSnapshot(k)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 删除 Key 后，残留身份不得被当作"无限制"放行。
func TestDeletedKeyDoesNotBypassModelCheck(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "gone", Models: []string{"only"}})
	if err := keyDelete(k.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = withKeyIdentity(req, &keyIdentity{ID: k.ID})
	rec := httptest.NewRecorder()
	if keyModelAllowed(rec, req, "any-model") {
		t.Fatal("Key 已删除时不得默认放行模型权限")
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应返回 401/403，实际 %d", rec.Code)
	}
}

// P1-3：删除 Key 后，历史消费不能随压缩消失。
func TestDeletedKeyHistorySurvivesCompaction(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "hist", QuotaTotal: 10})
	keySettle(k, "h1", map[string]any{"credit": 6.0}, 200)
	id := k.ID
	if err := keyDelete(id); err != nil {
		t.Fatal(err)
	}
	ledgerCompact()

	archived := archivedUsage(id)
	if archived == nil || archived.Total != 6 {
		t.Fatalf("删除后历史应保留 6 分，实际 %+v", archived)
	}

	reloadKeysAndLedger(t, dir)
	if a := archivedUsage(id); a == nil || a.Total != 6 {
		t.Fatalf("重启后归档历史应恢复为 6，实际 %+v", a)
	}
}

// P1-3：重置额度不能抹掉"生命周期累计"。
func TestResetKeepsLifetimeTotal(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "life", QuotaTotal: 100})
	keySettle(k, "l1", map[string]any{"credit": 9.0}, 200)
	if err := keyResetUsage(k.ID); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	u := k.usage
	k.mu.Unlock()
	if u.Total != 0 {
		t.Fatalf("重置后准入累计应清零，实际 %v", u.Total)
	}
	if u.Lifetime != 9 {
		t.Fatalf("重置不得清除历史累计，实际 Lifetime=%v", u.Lifetime)
	}
}

// P1-4：鉴权开关独立于密钥文件。
func TestAuthSwitchSurvivesKeyFileLoss(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, _ = mustCreateKey(t, keyCreateOptions{Name: "persisted"})
	if !authRequired() {
		t.Fatal("创建过 Key 后必须要求鉴权")
	}
	// 模拟 apikeys.json 丢失
	if err := os.Remove(filepath.Join(dir, "apikeys.json")); err != nil {
		t.Fatal(err)
	}
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.requireAuth = false
	apiKeys.mu.Unlock()
	if err := loadAuthSwitch(dir); err != nil {
		t.Fatal(err)
	}
	if !authRequired() {
		t.Fatal("密钥文件丢失后鉴权开关必须仍然生效，不能回到匿名开放")
	}
}

func TestAuthSwitchExplicitOpen(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := loadAuthSwitch(dir); err != nil {
		t.Fatal(err)
	}
	if authRequired() {
		t.Fatal("从未配置过任何密钥时应保持开放")
	}
	if err := setAuthSwitch(dir, true); err != nil {
		t.Fatal(err)
	}
	if !authRequired() {
		t.Fatal("显式打开后应要求鉴权")
	}
	_ = strings.TrimSpace("")
}
