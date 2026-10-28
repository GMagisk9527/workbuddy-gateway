package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loadStatusForTest(t *testing.T) statusSnapshot {
	t.Helper()
	raw, err := os.ReadFile(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestStatusSnapshotHeartbeatAndTimeBasedChanges(t *testing.T) {
	isolateAdminFeatures(t)
	now := time.Now().Truncate(time.Second)
	a := &Account{Path: "clock.json", Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{ExpiresAt: now.Add(6 * time.Second).Unix()}},
		CooldownUntil: now.Add(3 * time.Second), QuotaKnown: true, QuotaRemaining: 5,
		ModelStates: map[string]*modelRuntimeState{"synthetic": {CooldownUntil: now.Add(6 * time.Second)}}}
	accounts = []*Account{a}
	write := func(at time.Time) statusSnapshot {
		t.Helper()
		if err := writeStatusSnapshotAt(at); err != nil {
			t.Fatal(err)
		}
		return loadStatusForTest(t)
	}
	if snap := write(now); snap.Accounts[0].State != "cooldown" {
		t.Fatal(snap.Accounts[0].State)
	}
	if snap := write(now.Add(time.Second)); snap.UpdatedAt != now.Unix() {
		t.Fatal("unchanged state wrote before heartbeat")
	}
	if snap := write(now.Add(3 * time.Second)); snap.Accounts[0].State != "active" || snap.UpdatedAt != now.Add(3*time.Second).Unix() {
		t.Fatal("cooldown expiry was not published")
	}
	if snap := write(now.Add(6 * time.Second)); snap.Accounts[0].State != "expired" || snap.Accounts[0].ModelCooldowns != 0 {
		t.Fatal("token/model expiry was not published")
	}
	if snap := write(now.Add(14 * time.Second)); snap.UpdatedAt != now.Add(6*time.Second).Unix() {
		t.Fatal("unchanged expired state rewrote early")
	}
	if snap := write(now.Add(15 * time.Second)); snap.UpdatedAt != now.Add(15*time.Second).Unix() {
		t.Fatal("heartbeat did not refresh")
	}
	a.QuotaRemaining = 4
	if snap := write(now.Add(16 * time.Second)); snap.Accounts[0].QuotaRemaining != 4 || snap.UpdatedAt != now.Add(16*time.Second).Unix() {
		t.Fatal("account change was not immediately published")
	}
}

func TestStatusSnapshotFailedWriteRetriesAndExternalReplacementIsRepaired(t *testing.T) {
	isolateAdminFeatures(t)
	accounts = nil
	now := time.Now()
	if err := writeStatusSnapshotAt(now); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statusSnapshotFile+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshotAt(now.Add(statusSnapshotHeartbeat)); err == nil {
		t.Fatal("write error was hidden")
	}
	if err := os.Remove(statusSnapshotFile + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshotAt(now.Add(statusSnapshotHeartbeat)); err != nil {
		t.Fatal(err)
	}
	if snap := loadStatusForTest(t); snap.UpdatedAt != now.Add(statusSnapshotHeartbeat).Unix() {
		t.Fatal("failed write was cached")
	}
	if err := os.WriteFile(statusSnapshotFile, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshotAt(now.Add(statusSnapshotHeartbeat + time.Second)); err != nil {
		t.Fatal(err)
	}
	loadStatusForTest(t)
}

func TestStatusSnapshotCaptureOwnsMutableState(t *testing.T) {
	isolateAdminFeatures(t)
	a := &Account{Path: "copy.json", Auth: &StoredAuth{Edition: "intl", Account: StoredAccount{UID: "old"}, Auth: StoredTokens{AccessToken: "must-not-copy"}},
		ModelStates: map[string]*modelRuntimeState{"synthetic": {CostClass: modelCostFree}}, credits: accountCredits{Packages: []creditPackage{{Name: "old"}}}}
	accounts = []*Account{a}
	copy := captureStatusSnapshotAccounts()[0]
	accountMu.Lock()
	a.Auth.Account.UID = "new"
	a.ModelStates["synthetic"].CostClass = modelCostPaid
	a.credits.Packages[0].Name = "new"
	accountMu.Unlock()
	if copy.Auth.Account.UID != "old" || copy.ModelStates["synthetic"].CostClass != modelCostFree || copy.credits.Packages[0].Name != "old" {
		t.Fatal("capture aliases live account state")
	}
	if copy.Auth.Auth.AccessToken != "" {
		t.Fatal("presentation capture copied an access token")
	}
}

func TestStatusSnapshotIgnoresModelOrderButPreservesDisplayAndClockRollback(t *testing.T) {
	isolateAdminFeatures(t)
	statusSnapshotWriteMu.Lock()
	defer statusSnapshotWriteMu.Unlock()
	now := time.Now().Truncate(time.Second)
	snap := statusSnapshot{UpdatedAt: now.Unix(), Models: []modelStatSnapshot{{ID: "b"}, {ID: "a"}}}
	if err := persistStatusSnapshot(snap, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	snap.Models = []modelStatSnapshot{{ID: "a"}, {ID: "b"}}
	snap.UpdatedAt = now.Add(time.Second).Unix()
	if err := persistStatusSnapshot(snap, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("model map iteration order caused a write")
	}
	if loadStatusForTest(t).Models[0].ID != "b" {
		t.Fatal("deduplication reordered the displayed directory")
	}
	snap.UpdatedAt = now.Add(-time.Minute).Unix()
	if err := persistStatusSnapshot(snap, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if loadStatusForTest(t).UpdatedAt != snap.UpdatedAt {
		t.Fatal("clock rollback suppressed heartbeat")
	}
}

func TestStatusSnapshotUnchangedSkipsWriteAndRepairsMissing(t *testing.T) {
	isolateAdminFeatures(t)
	accountMu.Lock()
	accounts = nil
	accountMu.Unlock()
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(statusSnapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged status was rewritten before its heartbeat")
	}
	if err := os.Remove(statusSnapshotFile); err != nil {
		t.Fatal(err)
	}
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statusSnapshotFile); err != nil {
		t.Fatal("missing status was not recreated:", err)
	}
}

func TestStatusSnapshotModelAggregationDoesNotHoldAccountLock(t *testing.T) {
	isolateAdminFeatures(t)
	modelStatsMu.Lock()
	requestStats.Lock()
	statsLocked := true
	done := make(chan error, 1)
	go func() { done <- writeStatusSnapshot() }()
	defer func() {
		if statsLocked {
			requestStats.Unlock()
		}
		modelStatsMu.Unlock()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for accountMu.TryLock() {
		accountMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("writer did not enter account capture")
		}
		time.Sleep(time.Millisecond)
	}
	requestStats.Unlock()
	statsLocked = false
	// The model lock keeps aggregation in flight. Account selection must still
	// be able to acquire accountMu while that unrelated work is blocked.
	deadline = time.Now().Add(200 * time.Millisecond)
	for {
		if accountMu.TryLock() {
			accountMu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status model aggregation blocked account selection")
		}
		time.Sleep(time.Millisecond)
	}
}
