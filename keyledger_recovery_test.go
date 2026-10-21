package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func closeRecoveryTestWAL(t *testing.T) {
	t.Helper()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.file != nil {
		if err := ledger.file.Close(); err != nil {
			t.Fatal(err)
		}
		ledger.file = nil
	}
	ledger.writable = false
}

func restartRecoveryTest(t *testing.T, dir string) {
	t.Helper()
	// A new process starts with a false degraded flag. Reload both the persisted
	// key store and ledger, rather than carrying the previous process's guard.
	apiKeys.degraded.Store(false)
	reloadKeysAndLedger(t, dir)
}

func requireLedgerProtection(t *testing.T, id string) {
	t.Helper()
	admitErr, release := keyAdmit(keyByID(id), "")
	if release != nil {
		release()
	}
	if admitErr == nil || admitErr.Code != "ledger_unavailable" {
		t.Fatalf("incomplete ledger must reject admission: %+v", admitErr)
	}
}

func TestLedgerMissingHistoryRemainsProtectedAcrossRestarts(t *testing.T) {
	for _, missing := range []string{"snapshot", "both"} {
		t.Run(missing, func(t *testing.T) {
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			_, k := mustCreateKey(t, keyCreateOptions{Name: missing, QuotaTotal: 5})
			keySettle(k, "missing-"+missing, map[string]any{"credit": 5.0}, 200)
			ledgerCompact()
			wal, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
			if err != nil || len(wal) != 0 {
				t.Fatalf("successful compaction must leave empty WAL: len=%d err=%v", len(wal), err)
			}
			closeRecoveryTestWAL(t)
			if err := os.Remove(filepath.Join(dir, ledgerSnapshotName)); err != nil {
				t.Fatal(err)
			}
			if missing == "both" {
				if err := os.Remove(filepath.Join(dir, ledgerWALName)); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				restartRecoveryTest(t, dir)
				requireLedgerProtection(t, k.ID)
				// Compaction of subsequent observable data cannot repair the lost
				// historical baseline or remove its persistent safety marker.
				ledgerCompact()
			}
		})
	}
}

func TestLedgerFreshEmptyWALCanRestartAfterKeyCreation(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "new-empty", QuotaTotal: 5})
	for i := 0; i < 3; i++ {
		restartRecoveryTest(t, dir)
		if err, release := keyAdmit(keyByID(k.ID), ""); err != nil {
			t.Fatalf("unspent initialized ledger rejected: %+v", err)
		} else {
			release()
		}
	}
}

func TestLedgerInvalidDocumentSchemasRemainUntouched(t *testing.T) {
	for _, filename := range []string{ledgerStateName, ledgerSnapshotName} {
		for _, invalid := range []string{`{}`, `null`, `{"version":0}`, `{"version":2}`, `[]`} {
			t.Run(filename+"/"+invalid, func(t *testing.T) {
				dir := resetKeyWorld(t)
				if err := ledgerLoad(dir); err != nil {
					t.Fatal(err)
				}
				_, _ = mustCreateKey(t, keyCreateOptions{Name: "invalid-document", QuotaTotal: 5})
				closeRecoveryTestWAL(t)
				path := filepath.Join(dir, filename)
				if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					apiKeys.degraded.Store(false)
					if err := keyStoreLoad(); err != nil {
						t.Fatal(err)
					}
					registerRuntimeKeys()
					if err := ledgerLoad(dir); err == nil {
						t.Fatal("invalid persisted schema must not initialize an empty ledger")
					}
					got, err := os.ReadFile(path)
					if err != nil || string(got) != invalid || ledger.writable {
						t.Fatalf("invalid document changed or ledger became writable: got=%s err=%v writable=%v", got, err, ledger.writable)
					}
				}
			})
		}
	}
}

func TestLedgerLegacyCompleteWALMigratesWithoutReset(t *testing.T) {
	dir := resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "legacy-wal", QuotaTotal: 10})
	now := time.Now()
	writeRawWAL(t, dir,
		ledgerEntry{Seq: 1, Key: k.ID, Kind: "settle", Known: true, Credit: 2, At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)},
		ledgerEntry{Seq: 2, Key: k.ID, Kind: "settle", Known: true, Credit: 3, At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)})
	for i := 0; i < 3; i++ {
		restartRecoveryTest(t, dir)
		u := keyUsageSnapshot(keyByID(k.ID))
		if apiKeys.degraded.Load() || u.Used != 5 || u.Lifetime != 5 {
			t.Fatalf("legacy WAL lost data or was rejected: %+v degraded=%v", u, apiKeys.degraded.Load())
		}
	}
	ledgerCompact()
	restartRecoveryTest(t, dir)
	if got := keyUsageSnapshot(keyByID(k.ID)); got.Used != 5 || got.Lifetime != 5 {
		t.Fatalf("migrated ledger changed after compaction: %+v", got)
	}
}

func TestLedgerLegacyAmbiguousEmptyWALRequiresRecovery(t *testing.T) {
	dir := resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "old-empty", QuotaTotal: 5})
	writeRawWAL(t, dir)
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
}

func TestLedgerUnattributedRecoveryProtectionSurvivesCompaction(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "lost-owner", QuotaTotal: 5})
	appendRaw(t, dir, `{"seq":1,"ke`)
	for i := 0; i < 3; i++ {
		restartRecoveryTest(t, dir)
		requireLedgerProtection(t, k.ID)
		if ledger.lostSettlements != 1 {
			t.Fatalf("lost settlement count changed: %d", ledger.lostSettlements)
		}
		if i == 1 {
			ledgerCompact()
		}
	}
}

func TestLedgerLegacyLostSettlementsSnapshotRestoresProtection(t *testing.T) {
	dir := resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "old-lost", QuotaTotal: 5})
	writeSnapshotFile(t, dir, ledgerSnapshot{LostSettlements: 2, Keys: map[string]*apiKeyUsage{k.ID: {}}})
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
	ledgerCompact()
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
	if ledger.lostSettlements != 2 {
		t.Fatalf("lost settlement count changed: %d", ledger.lostSettlements)
	}
}

func TestLedgerTailRecoveryFollowsResetsAndUsesNextSequence(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "ordered-tail", QuotaTotal: 5})
	now := time.Now()
	writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 4, Keys: map[string]*apiKeyUsage{k.ID: {DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)}}})
	writeRawWAL(t, dir,
		ledgerEntry{Seq: 5, Key: k.ID, Kind: "settle", Known: true, Credit: 4, At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)},
		ledgerEntry{Seq: 6, Key: k.ID, Kind: "reset", At: now.UnixMilli()},
		ledgerEntry{Seq: 7, Key: k.ID, Kind: "settle", Known: true, Credit: 2, At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)})
	appendRaw(t, dir, `{"seq":8,"key":"`+k.ID+`","kind":"settle","credit":99`)
	for i := 0; i < 3; i++ {
		restartRecoveryTest(t, dir)
		u := keyUsageSnapshot(keyByID(k.ID))
		if u.Unknown != 1 || u.Used != 2 || u.Lifetime != 6 || ledger.seq.Load() != 8 {
			t.Fatalf("tail recovery reordered or duplicated: %+v seq=%d", u, ledger.seq.Load())
		}
		if i == 0 {
			raw, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
			if err != nil {
				t.Fatal(err)
			}
			var events []ledgerEntry
			dec := json.NewDecoder(bytes.NewReader(raw))
			for dec.More() {
				var e ledgerEntry
				if err := dec.Decode(&e); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			if len(events) != 4 || events[3].Seq != 8 || events[3].Kind != "recovery" {
				t.Fatalf("recovery event must follow all historical events: %+v", events)
			}
		}
		if i == 1 {
			ledgerCompact()
		}
	}
}

func TestLedgerRecoveryStateWriteFailurePreservesBrokenTail(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, _ = mustCreateKey(t, keyCreateOptions{Name: "recovery-write-failure", QuotaTotal: 5})
	appendRaw(t, dir, `{"seq":1,"ke`)
	closeRecoveryTestWAL(t)
	before, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ledgerStateName)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ledgerLoad(dir); err == nil {
		t.Fatal("state write failure must abort recovery")
	}
	after, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("recovery must not discard tail before compensation is durable: before=%s after=%s err=%v", before, after, err)
	}
}

func TestLedgerCompactionStateWriteFailurePreservesWAL(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "compaction-state-failure", QuotaTotal: 5})
	keySettle(k, "state-failure-settlement", map[string]any{"credit": 2.0}, 200)
	before, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ledgerStateName)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	ledgerCompact()
	after, err := os.ReadFile(filepath.Join(dir, ledgerWALName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("WAL discarded before snapshot requirement was durable: before=%s after=%s err=%v", before, after, err)
	}
	if err := os.Remove(filepath.Join(dir, ledgerStateName) + ".tmp"); err != nil {
		t.Fatal(err)
	}
	restartRecoveryTest(t, dir)
	if got := keyUsageSnapshot(keyByID(k.ID)); got.Used != 2 || got.Lifetime != 2 {
		t.Fatalf("retry after state write failure changed consumption: %+v", got)
	}
}

func TestDeletedKeySummaryPreservesActualCreditsAcrossReset(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "historical", QuotaTotal: 20})
	keySettle(k, "history-before-reset", map[string]any{"credit": 5.0}, 200)
	if err := keyResetUsage(k.ID); err != nil {
		t.Fatal(err)
	}
	keySettle(k, "history-after-reset", map[string]any{"credit": 2.0}, 200)
	requireCreditSummary(t, 7, 7)
	if got := keyUsageSnapshot(k).Used; got != 2 {
		t.Fatalf("quota reset must retain its admission meaning: %v", got)
	}
	if err := keyDelete(k.ID); err != nil {
		t.Fatal(err)
	}
	requireCreditSummary(t, 7, 7)
	keySettle(k, "history-inflight-after-delete", map[string]any{"credit": 3.0}, 200)
	requireCreditSummary(t, 10, 10)
	ledgerCompact()
	restartRecoveryTest(t, dir)
	requireCreditSummary(t, 10, 10)
}

func TestDeletedKeySnapshotHistorySurvivesRestartBeforeCompaction(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "deleted-after-snapshot"})
	keySettle(k, "snapshot-before-delete", map[string]any{"credit": 5.0}, 200)
	ledgerCompact()
	keySettle(k, "wal-before-delete", map[string]any{"credit": 2.0}, 200)
	if err := keyDelete(k.ID); err != nil {
		t.Fatal(err)
	}
	restartRecoveryTest(t, dir)
	requireCreditSummary(t, 7, 7)
	ledgerCompact()
	restartRecoveryTest(t, dir)
	requireCreditSummary(t, 7, 7)
}

func TestArchivedCreditsRollHistoricalDayIndependently(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 1, Archived: map[string]*apiKeyUsage{
		"deleted": {Lifetime: 5, Daily: 5, DayKey: keyDayKey(yesterday), MonthKey: keyMonthKey(yesterday)},
	}})
	writeRawWAL(t, dir, ledgerEntry{Seq: 2, Key: "deleted", Kind: "settle", Known: true, Credit: 2,
		At: now.UnixMilli(), DayKey: keyDayKey(now), MonthKey: keyMonthKey(now)})
	restartRecoveryTest(t, dir)
	requireCreditSummary(t, 7, 2)
	ledgerCompact()
	restartRecoveryTest(t, dir)
	requireCreditSummary(t, 7, 2)
}

func requireCreditSummary(t *testing.T, lifetime, today float64) {
	t.Helper()
	summary := keyAdminListPayload()["summary"].(map[string]any)
	if summary["allTimeCredits"] != lifetime || summary["todayCredits"] != today {
		t.Fatalf("credit history summary mismatch: got=%+v want lifetime=%v today=%v", summary, lifetime, today)
	}
}

func TestDeletedKeyConcurrentCompactionRetainsHistory(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	const requests = 24
	for i := 0; i < requests; i++ {
		_, k := mustCreateKey(t, keyCreateOptions{Name: "delete-with-compaction"})
		keySettle(k, k.ID, map[string]any{"credit": 1.0}, 200)
		start := make(chan struct{})
		errs := make(chan error, 1)
		var finished sync.WaitGroup
		finished.Add(2)
		go func() {
			defer finished.Done()
			<-start
			ledgerCompact()
		}()
		go func() {
			defer finished.Done()
			<-start
			errs <- keyDelete(k.ID)
		}()
		close(start)
		finished.Wait()
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		// Reload the exact snapshot from the race, without a second compaction
		// hiding an omitted archive behind the current process's memory.
		restartRecoveryTest(t, dir)
		requireCreditSummary(t, float64(i+1), float64(i+1))
	}
}
