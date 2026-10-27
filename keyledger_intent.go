package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"
)

var errLedgerUnavailable = errors.New("记账存储不可用，未发送上游请求")

// Called under ledger.mu. Stop further writes after a partial write or failed
// fsync: neither retrying that append nor compacting its sequence is safe.
func ledgerWriteFailedLocked(cause error) error {
	apiKeys.degraded.Store(true)
	ledger.writable = false
	ledger.state.Incomplete = true
	ledger.state.Version = 1
	if ledger.path != "" {
		if err := persistLedgerState(filepath.Dir(ledger.path), ledger.state); err != nil {
			// The begin record was synced BEFORE the model call. Its unresolved intent
			// protects a restart even when a full/broken disk also rejects this marker.
			log.Printf("[账本] 严重=无法持久化故障标记 原因=%v 业务影响=保留调用前凭据，重启需核对未完成调用", err)
		}
	}
	return cause
}

func beginKeyLedgerAttempt(r *http.Request) error {
	p := statsAttemptFor(r)
	if p == nil || p.attemptID == "" || p.ledgerIntent {
		return nil
	}
	k := settlementKeyFor(r)
	if k == nil {
		if identity := keyIdentityFor(r); identity != nil {
			k = keyByID(identity.ID)
		}
	}
	if k == nil || k.legacy {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.state.Incomplete {
		return errLedgerUnavailable
	}
	e := ledgerEntry{Seq: nextLedgerSeq(), Key: k.ID, Attempt: p.attemptID, Kind: "begin", At: time.Now().UnixMilli()}
	if err := ledgerAppendLocked(e); err != nil {
		return fmt.Errorf("%w: %v", errLedgerUnavailable, err)
	}
	p.ledgerIntent = true
	return nil
}

// Explicitly rejected/unsubmitted requests have no cost to settle, but must
// close their durable begin record so a normal restart does not flag lost cost.
func cancelKeyLedgerAttempt(p *statsAttempt) {
	if !p.ledgerIntent {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	key, exists := ledger.pendingAttempts[p.attemptID]
	if !exists {
		return
	}
	e := ledgerEntry{Seq: nextLedgerSeq(), Key: key, Attempt: p.attemptID, Kind: "cancel", At: time.Now().UnixMilli()}
	if err := ledgerAppendLocked(e); err != nil {
		log.Printf("[账本] 警告=取消未计费调用凭据失败 原因=%v", err)
	}
}
