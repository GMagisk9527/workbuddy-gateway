package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Model sends share a credential read lock. Recheck expiry after waiting;
// refresh needs the exclusive lock, so never upgrade while holding a reader.
// The caller releases the read lock after headers (after classification on error).
func lockModelAccountWithContext(ctx context.Context, acc *Account) error {
	refreshed := false
	for {
		if !tryAccountLockWithContext(ctx, acc.lock.TryRLock) {
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			acc.lock.RUnlock()
			return err
		}
		accountMu.Lock()
		now := time.Now().Unix()
		refresh := !acc.Disabled && acc.Auth != nil && now > acc.Auth.Auth.ExpiresAt-900
		expired := acc.Auth != nil && now >= acc.Auth.Auth.ExpiresAt
		accountMu.Unlock()
		if refreshed && expired {
			acc.lock.RUnlock()
			return errors.New("账号令牌在等待凭据维护期间已过期")
		}
		if !refresh || refreshed {
			return nil
		}
		acc.lock.RUnlock()
		if err := ensureValidTokenForContext(ctx, acc); err != nil {
			return err
		}
		refreshed = true
	}
}

// accountMu must be held. Reuse scheduler rules without reserving another probe
// or promoting a previously normal request into a new exhausted-account probe.
func queuedAccountUsableLocked(acc *Account, model string, selected accountSelectionKind, now time.Time) (accountSelectionKind, bool) {
	present := false
	for _, member := range accounts {
		if member == acc {
			present = true
			break
		}
	}
	if !present || acc.Auth == nil {
		return "", false
	}
	if allowed, _ := modelAccountAllowed(model, acc, accounts); !allowed {
		return "", false
	}
	kind, usable := usableForModelReservationLocked(acc, model, now, selected == selectionProbeExhausted)
	return kind, usable && (kind != selectionProbeExhausted || selected == selectionProbeExhausted)
}

// The model slot remains occupied for the whole upstream response, including SSE.
// EOF, read failure and explicit close all clean up and release it exactly once.
type accountStreamBody struct {
	body    io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func newAccountStreamBody(body io.ReadCloser, release func()) *accountStreamBody {
	return &accountStreamBody{body: body, release: release}
}

func (b *accountStreamBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *accountStreamBody) Close() error {
	b.once.Do(func() {
		defer b.release()
		b.err = b.body.Close()
	})
	return b.err
}
