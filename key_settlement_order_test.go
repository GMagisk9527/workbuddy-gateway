package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func waitSettlementCondition(t *testing.T, label string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", label)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func TestKeyAdmissionWaitsForSettlementBeforeReleasingSlot(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "settlement-order", QuotaTotal: 1, MaxConcurrent: 1})
	const attemptID = "settlement-before-release"
	handler := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*statsAttemptFor(r) = statsAttempt{id: "synthetic-account", attemptID: attemptID, model: "synthetic", started: time.Now()}
		statsObserveUsage(r, map[string]any{"credit": 1.0}, true)
		w.WriteHeader(http.StatusOK)
	}))))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	done := make(chan struct{})
	ledger.mu.Lock()
	locked := true
	defer func() {
		if locked {
			ledger.mu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("first request did not finish")
		}
	}()
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}()
	// markSettled is reached before waiting for the deliberately held WAL lock.
	// This checks the lifecycle without depending on implementation line numbers.
	waitSettlementCondition(t, "the first request to begin settlement", func() bool {
		settledMu.Lock()
		_, started := settledIDs[attemptID]
		settledMu.Unlock()
		return started
	})
	admitErr, release := keyAdmit(k, "")
	if release != nil {
		release()
	}
	ledger.mu.Unlock()
	locked = false
	<-done
	if admitErr == nil {
		t.Fatal("next request was admitted before the previous full-quota settlement finished")
	}
	admitErr, release = keyAdmit(k, "")
	if release != nil {
		release()
	}
	if admitErr == nil || admitErr.Code != "credit_quota_exhausted" {
		t.Fatal("completed settlement did not exhaust the key's quota")
	}
	k.mu.Lock()
	u := k.usage
	k.mu.Unlock()
	if u.Total != 1 || u.Requests != 1 {
		t.Fatalf("settlement was lost or repeated: %+v", u)
	}
}

func TestKeyDeletionDuringSettlementArchivesExactlyOnce(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "delete-settlement", QuotaTotal: 20})
	k.mu.Lock()
	locked := true
	settled := make(chan struct{})
	deleted := make(chan error, 1)
	deleteStarted := false
	defer func() {
		if locked {
			k.mu.Unlock()
		}
		select {
		case <-settled:
		case <-time.After(3 * time.Second):
			t.Error("settlement did not finish")
		}
		if deleteStarted {
			select {
			case <-deleted:
			case <-time.After(3 * time.Second):
				t.Error("deletion did not finish")
			}
		}
	}()
	go func() {
		defer close(settled)
		keySettle(k, "delete-settlement-race", map[string]any{"credit": 3.0}, http.StatusOK)
	}()
	// Wait until settlement is queued on the key lock, then queue deletion.
	// The function/lock stack check avoids a timing race and has no source-line dependency.
	waitSettlementCondition(t, "settlement to wait on the held key lock", func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, stack := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(stack, "workbuddy-gateway.keySettle(") && strings.Contains(stack, "sync.(*Mutex).Lock(") {
				return true
			}
		}
		return false
	})
	deleteStarted = true
	go func() { deleted <- keyDelete(k.ID) }()
	waitSettlementCondition(t, "deletion to hold or wait for membership write access", func() bool {
		if apiKeys.mu.TryRLock() {
			apiKeys.mu.RUnlock()
			return false
		}
		return true
	})
	k.mu.Unlock()
	locked = false
	deleteErr := <-deleted
	deleteStarted = false
	<-settled
	if deleteErr != nil {
		t.Fatal(deleteErr)
	}
	u := archivedUsage(k.ID)
	ledgerCompact()
	restartRecoveryTest(t, dir)
	persisted := archivedUsage(k.ID)
	if u == nil || persisted == nil || u.Total != 3 || u.Requests != 1 || persisted.Total != 3 || persisted.Requests != 1 {
		t.Fatalf("one settlement was archived more than once: memory=%+v persisted=%+v", u, persisted)
	}
}
