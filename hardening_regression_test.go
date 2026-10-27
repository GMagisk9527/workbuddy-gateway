package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHardeningModelInputRejectedBeforeRetention(t *testing.T) {
	resetModelStats()
	t.Cleanup(resetModelStats)
	handler := bodyLimitAPIHandler(t)
	for _, endpoint := range modelBodyLimitCases[:3] {
		for _, name := range []string{strings.Repeat("x", 257), "bad\nmodel", "bad\u2028model", "bad\x1bmodel", "bad model"} {
			var body map[string]any
			if err := json.Unmarshal([]byte(endpoint.body), &body); err != nil {
				t.Fatal(err)
			}
			body["model"] = name
			raw, _ := json.Marshal(body)
			w := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(string(raw)))
			request.Header.Set("Authorization", "Bearer body-limit-key")
			handler.ServeHTTP(w, request)
			if w.Code != 400 {
				t.Errorf("%s invalid model status=%d, want 400", endpoint.path, w.Code)
			}
		}
	}
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	if len(modelStats) != 0 {
		t.Fatalf("invalid models retained in stats: %d", len(modelStats))
	}
}

func TestHardeningModelCardinalityBounded(t *testing.T) {
	resetModelStats()
	t.Cleanup(resetModelStats)
	acc := &Account{Auth: &StoredAuth{}, QuotaExhausted: true}
	now := time.Now()
	accountMu.Lock()
	for i := 0; i < 1200; i++ {
		modelStateLocked(acc, fmt.Sprintf("synthetic-%d", i)).QuotaBlocked = true
	}
	if len(acc.ModelStates) > 1024 {
		t.Errorf("account retains %d models", len(acc.ModelStates))
	}
	if _, ok := usableForModelLocked(acc, "one-more-unknown", now); ok {
		t.Error("full account state must not admit an untracked probe")
	}
	accountMu.Unlock()
	for i := 0; i < 1200; i++ {
		recordModelRequest(fmt.Sprintf("synthetic-%d", i))
	}
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	if len(modelStats) > 1024 {
		t.Errorf("global stats retains %d models", len(modelStats))
	}
}

func TestHardeningAccountBudgetEvictsOnlyInactiveObservations(t *testing.T) {
	acc := &Account{Auth: &StoredAuth{}}
	now := time.Now()
	accountMu.Lock()
	defer accountMu.Unlock()
	for i := 0; i < maxTrackedModels; i++ {
		state := modelStateLocked(acc, fmt.Sprintf("model-%d", i))
		state.ObservedAt = now.Add(time.Duration(i) * time.Second)
	}
	acc.ModelStates["model-1"].CooldownUntil = now.Add(time.Hour)
	acc.ModelStates["model-2"].QuotaBlocked = true
	modelStateLocked(acc, "fresh-model")
	if len(acc.ModelStates) != maxTrackedModels || acc.ModelStates["fresh-model"] == nil || acc.ModelStates["model-0"] != nil {
		t.Fatal("inactive observation not evicted within budget")
	}
	if acc.ModelStates["model-1"] == nil || acc.ModelStates["model-2"] == nil {
		t.Fatal("active scheduling block evicted")
	}
}

func TestHardeningOversizedRestoreKeepsLongestProtections(t *testing.T) {
	states := map[string]modelStateSnapshot{}
	for i := 0; i < 1200; i++ {
		states[fmt.Sprintf("timed-%d", i)] = modelStateSnapshot{CooldownUntil: int64(1000 + i)}
	}
	states["quota-block"] = modelStateSnapshot{QuotaBlocked: true}
	states[strings.Repeat("x", 257)] = modelStateSnapshot{QuotaBlocked: true}
	bounded := boundedModelStateSnapshots(states)
	if len(bounded) != maxTrackedModels || !bounded["quota-block"].QuotaBlocked {
		t.Fatal("restore budget or quota protection lost")
	}
	for model, row := range bounded {
		if model != "quota-block" && row.CooldownUntil < 1177 {
			t.Fatal("kept a shorter block while dropping a longer one")
		}
	}
}

func TestHardeningCountTokensRejectsInvalidShapes(t *testing.T) {
	for _, body := range []string{"null", "{}", "{\"system\":\"hello\"}", "{\"messages\":null}", "{\"messages\":{}}", "{\"messages\":[null]}", "{\"messages\":[{\"role\":\"invalid\",\"content\":\"hi\"}]}"} {
		w := httptest.NewRecorder()
		handleCountTokens(w, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
		if w.Code != 400 {
			t.Errorf("invalid shape %s status=%d", body, w.Code)
		}
	}
}

func failKeyPersistence(t *testing.T) func() {
	t.Helper()
	path := apiKeys.path
	apiKeys.path = filepath.Join(t.TempDir(), "destination")
	if err := os.Mkdir(apiKeys.path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() { apiKeys.path = path }
}

func TestHardeningKeyMutationsRollback(t *testing.T) {
	for _, op := range []string{"create", "rotate", "update", "toggle", "delete"} {
		t.Run(op, func(t *testing.T) {
			resetKeyWorld(t)
			plain, k := mustCreateKey(t, keyCreateOptions{Name: "before", QuotaTotal: 10})
			oldPrefix, oldHash := k.Prefix, k.Hash
			beforeIDs, beforePrefixes := len(apiKeys.byID), len(apiKeys.byPrefix)
			restore := failKeyPersistence(t)
			defer restore()
			var err error
			switch op {
			case "create":
				_, _, err = keyCreate(keyCreateOptions{Name: "failed"})
			case "rotate":
				_, _, err = keyRotate(k.ID)
			case "update":
				_, err = keyUpdate(k.ID, keyCreateOptions{Name: "after", QuotaTotal: 999, Models: []string{"another"}})
			case "toggle":
				err = keySetEnabled(k.ID, false)
			case "delete":
				err = keyDelete(k.ID)
			}
			if err == nil {
				t.Fatal("expected persistence failure")
			}
			if len(apiKeys.byID) != beforeIDs || apiKeys.byID[k.ID] != k || len(apiKeys.byPrefix) != beforePrefixes || apiKeys.byPrefix[oldPrefix] != k {
				t.Fatal("failed change altered key membership")
			}
			if authenticateAPIKey(plain) != k || k.Prefix != oldPrefix || k.Hash != oldHash || k.Name != "before" || k.QuotaTotal != 10 || !k.Enabled || len(k.Models) != 0 {
				t.Fatal("failed change altered the usable key")
			}
			if _, ok := apiKeys.archive[k.ID]; ok {
				t.Fatal("failed deletion archived a live key")
			}
		})
	}
}

func TestHardeningFailedFirstKeyStillRequiresAuthAfterRestart(t *testing.T) {
	dir := resetKeyWorld(t)
	beforeIDs := len(apiKeys.byID)
	restore := failKeyPersistence(t)
	_, _, err := keyCreate(keyCreateOptions{Name: "failure"})
	restore()
	if err == nil {
		t.Fatal("expected failure")
	}
	if len(apiKeys.byID) != beforeIDs {
		t.Error("unreturned key remains live")
	}
	if err := loadAuthSwitch(dir); err != nil {
		t.Fatal(err)
	}
	if !authSwitchEnabled() {
		t.Error("failed first key creation lost the auth switch")
	}
}

func TestHardeningWALFailureIsDurableAndDoesNotSettle(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "limited", QuotaTotal: 10})
	ledger.mu.Lock()
	if err := ledger.file.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	ledger.file, err = os.Open(ledger.path)
	if err != nil {
		t.Fatal(err)
	}
	ledger.mu.Unlock()
	keySettle(k, "wal-failure", map[string]any{"credit": 2.0}, 200)
	if k.usage.Total != 0 || k.usage.Requests != 0 {
		t.Error("failed append published a settled usage increment")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ledgerStateName))
	if err != nil {
		t.Fatal(err)
	}
	var state ledgerDiskState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Incomplete || ledger.writable {
		t.Error("failed WAL not persistently protected")
	}
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
}

func TestHardeningWALSequenceGapsAfterSnapshot(t *testing.T) {
	for _, seqs := range [][]int64{{4}, {2, 4}, {2, 2}, {2, 1}} {
		t.Run(fmt.Sprint(seqs), func(t *testing.T) {
			dir := resetKeyWorld(t)
			_, k := mustCreateKey(t, keyCreateOptions{Name: "gap", QuotaTotal: 10})
			writeSnapshotFile(t, dir, ledgerSnapshot{LastSeq: 1, Keys: map[string]*apiKeyUsage{k.ID: {}}})
			entries := make([]ledgerEntry, len(seqs))
			for i, seq := range seqs {
				entries[i] = ledgerEntry{Seq: seq, Key: k.ID, Known: true, Credit: 1}
			}
			writeRawWAL(t, dir, entries...)
			if err := ledgerLoad(dir); err == nil && !apiKeys.degraded.Load() {
				t.Fatal("sequence gap/duplicate/reversal silently accepted")
			}
		})
	}
}

func TestHardeningIntentSurvivesFailedWALAndFailedFaultMarker(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "intent", QuotaTotal: 10})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = request.WithContext(context.WithValue(request.Context(), statsRequestContextKey{}, &statsAttempt{attemptID: "intent-before-call", id: "account"}))
	request = withKeyIdentity(request, &keyIdentity{ID: k.ID})
	withSettlementKey(request, k)
	if err := beginKeyLedgerAttempt(request); err != nil {
		t.Fatal(err)
	}
	ledgerCompact() // An in-flight intent must survive snapshot + WAL truncation.
	ledger.mu.Lock()
	_ = ledger.file.Close()
	var err error
	ledger.file, err = os.Open(ledger.path)
	ledger.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a disk that also refuses replacement of the fault-state file.
	statePath := filepath.Join(dir, ledgerStateName)
	prior, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if outcome := keySettle(k, "intent-before-call", map[string]any{"credit": 2.0}, 200); outcome != settleStorageError {
		t.Fatalf("outcome=%s", outcome)
	}
	if k.usage.Total != 0 {
		t.Fatal("failed settlement changed committed usage")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, prior, 0600); err != nil {
		t.Fatal(err)
	}
	restartRecoveryTest(t, dir)
	requireLedgerProtection(t, k.ID)
}

func TestHardeningIntentCompletesWithoutFalseRecovery(t *testing.T) {
	for _, mode := range []string{"settle", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			_, k := mustCreateKey(t, keyCreateOptions{Name: "complete", QuotaTotal: 10})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			p := &statsAttempt{attemptID: "complete-" + mode, id: "account"}
			request = request.WithContext(context.WithValue(request.Context(), statsRequestContextKey{}, p))
			request = withKeyIdentity(request, &keyIdentity{ID: k.ID})
			withSettlementKey(request, k)
			if err := beginKeyLedgerAttempt(request); err != nil {
				t.Fatal(err)
			}
			if mode == "settle" {
				keySettle(k, p.attemptID, map[string]any{"credit": 2.0}, 200)
			} else {
				cancelKeyLedgerAttempt(p)
			}
			if len(ledger.pendingAttempts) != 0 {
				t.Fatal("completed intent retained")
			}
			ledgerCompact()
			restartRecoveryTest(t, dir)
			if apiKeys.degraded.Load() {
				t.Fatal("completed intent incorrectly requires recovery")
			}
			want := 0.0
			if mode == "settle" {
				want = 2
			}
			if keyByID(k.ID).usage.Total != want {
				t.Fatal("intent altered charged cost")
			}
		})
	}
}

func TestHardeningBeginFailureNeverCallsUpstream(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			withStreamTestAccount(t)
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			plain, key := mustCreateKey(t, keyCreateOptions{Name: "before-send", QuotaTotal: 10})
			var calls int
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				raw, err := os.ReadFile(ledger.path)
				if err != nil || !strings.Contains(string(raw), "\"kind\":\"begin\"") {
					t.Error("model called without a durable begin")
				}
				body := "data: {\"id\":\"synthetic\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})}
			if fail {
				_ = ledger.file.Close()
				var err error
				ledger.file, err = os.Open(ledger.path)
				if err != nil {
					t.Fatal(err)
				}
			}
			request := streamTestRequest()
			request.Header.Set("Authorization", "Bearer "+plain)
			writer := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(writer, request)
			if fail {
				if calls != 0 || writer.Code != 503 || key.usage.Requests != 0 {
					t.Fatalf("failed begin sent or settled: calls=%d status=%d usage=%+v", calls, writer.Code, key.usage)
				}
			} else {
				if calls != 1 || writer.Code != 200 || key.usage.Total != 2 || len(ledger.pendingAttempts) != 0 {
					t.Fatalf("successful call did not close intent: calls=%d status=%d usage=%+v", calls, writer.Code, key.usage)
				}
				restartRecoveryTest(t, dir)
				if apiKeys.degraded.Load() {
					t.Fatal("normal completed HTTP call left recovery required")
				}
			}
		})
	}
}

type hardeningFaultWAL struct {
	*os.File
	mode string
}

func (f *hardeningFaultWAL) Write(p []byte) (int, error) {
	if f.mode == "short" {
		return f.File.Write(p[:len(p)/2])
	}
	return f.File.Write(p)
}
func (f *hardeningFaultWAL) Sync() error {
	if f.mode == "sync" {
		return errors.New("synthetic fsync failure")
	}
	return f.File.Sync()
}
func TestHardeningPartialWritesAndSyncFailuresRemainClosed(t *testing.T) {
	for _, mode := range []string{"short", "sync"} {
		t.Run(mode, func(t *testing.T) {
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			_, key := mustCreateKey(t, keyCreateOptions{Name: mode, QuotaTotal: 10})
			ledger.file = &hardeningFaultWAL{File: ledger.file.(*os.File), mode: mode}
			if outcome := keySettle(key, "fault-"+mode, map[string]any{"credit": 2.0}, 200); outcome != settleStorageError {
				t.Fatalf("outcome=%s", outcome)
			}
			if key.usage.Total != 0 || key.usage.Requests != 0 || ledger.writable || !ledger.state.Incomplete {
				t.Fatal("failed durability was published")
			}
			settledMu.Lock()
			_, settled := settledIDs["fault-"+mode]
			settledMu.Unlock()
			if settled {
				t.Fatal("failed append permanently claimed successful settlement")
			}
			restartRecoveryTest(t, dir)
			requireLedgerProtection(t, key.ID)
			if mode == "sync" && keyByID(key.ID).usage.Total != 2 {
				t.Fatal("complete but uncertain append must replay once")
			}
		})
	}
}
