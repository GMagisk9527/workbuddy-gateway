package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestStatsEncodingAllowsNewRequestsAndKeepsSnapshotRevision(t *testing.T) {
	for _, count := range []int{3, statsMaxRequests} {
		t.Run(fmt.Sprintf("rows_%d", count), func(t *testing.T) {
			chdirTemp(t)
			resetStatsState(t)
			requestStats.Lock()
			requestStats.document = syntheticPersistenceStats(count)
			requestStats.revision = 1
			before, err := json.Marshal(requestStats.document)
			requestStats.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			var updated bool
			err = flushOptionalStatsWithEncoder(func(snapshot statsDocument) ([]byte, error) {
				if !requestStats.TryLock() {
					return nil, errors.New("statistics mutex remains locked during JSON encoding")
				}
				// Append and prune while encoding is outside the statistics lock.
				// Existing rows are immutable; do not overwrite their backing array.
				requestStats.document.Requests = append(requestStats.document.Requests,
					statsTestRequest("synthetic-account", "new-model", statsZoneTime(), int64p(999), nil))
				requestStats.document.Requests, _ = trimStatsRequests(requestStats.document.Requests)
				a := requestStats.document.Accounts["synthetic-account"]
				a.ModelStates["synthetic-model"] = modelStateSnapshot{CostClass: modelCostFree}
				requestStats.document.Accounts["new-account"] = statsAccount{File: "new.json"}
				requestStats.revision++
				requestStats.Unlock()
				updated = true
				return json.Marshal(snapshot)
			})
			if err != nil || !updated {
				t.Fatalf("encoding blocked request statistics: updated=%t err=%v", updated, err)
			}
			raw, err := os.ReadFile(statsFile)
			if err != nil || !bytes.Equal(raw, before) {
				t.Fatalf("persisted snapshot changed during encoding: %v", err)
			}
			requestStats.Lock()
			current, saved := requestStats.revision, requestStats.saved
			requestStats.Unlock()
			if current != 2 || saved != 1 {
				t.Fatalf("new writes incorrectly marked saved: current=%d saved=%d", current, saved)
			}
			if err := flushOptionalStats(); err != nil {
				t.Fatal(err)
			}
			requestStats.Lock()
			defer requestStats.Unlock()
			if requestStats.saved != requestStats.revision {
				t.Fatal("next flush did not persist pending revision")
			}
		})
	}
}

func TestStatsSnapshotSurvivesConcurrentAppendAndPrune(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	requestStats.Lock()
	requestStats.document = syntheticPersistenceStats(statsMaxRequests)
	requestStats.revision = 1
	before, err := json.Marshal(requestStats.document)
	requestStats.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	err = flushOptionalStatsWithEncoder(func(snapshot statsDocument) ([]byte, error) {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1024; i++ {
				requestStats.Lock()
				requestStats.document.Requests = append(requestStats.document.Requests,
					statsTestRequest("synthetic-account", "new-model", statsZoneTime(), int64p(int64(i)), nil))
				requestStats.document.Requests, _ = trimStatsRequests(requestStats.document.Requests)
				account := requestStats.document.Accounts["synthetic-account"]
				account.ModelStates["synthetic-model"] = modelStateSnapshot{CostClass: modelCostFree}
				requestStats.revision++
				requestStats.Unlock()
			}
		}()
		defer wg.Wait()
		for i := 0; i < 3; i++ {
			raw, err := json.Marshal(snapshot)
			if err != nil || !bytes.Equal(raw, before) {
				return nil, fmt.Errorf("snapshot changed during concurrent writes: %v", err)
			}
		}
		return before, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatsEncoderFailureKeepsPreviousFileAndRevision(t *testing.T) {
	chdirTemp(t)
	resetStatsState(t)
	const previous = "synthetic previous file"
	if err := os.WriteFile(statsFile, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	requestStats.Lock()
	requestStats.document = syntheticPersistenceStats(1)
	requestStats.revision = 1
	requestStats.Unlock()
	wantErr := errors.New("synthetic encoding failure")
	if err := flushOptionalStatsWithEncoder(func(statsDocument) ([]byte, error) { return nil, wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("encoding error lost: %v", err)
	}
	raw, _ := os.ReadFile(statsFile)
	requestStats.Lock()
	defer requestStats.Unlock()
	if string(raw) != previous || requestStats.saved != 0 || requestStats.mode != "memory" {
		t.Fatal("failed encoding changed file or saved revision")
	}
}
