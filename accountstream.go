package main

import (
	"io"
	"sync"
	"time"
)

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

// The account remains occupied for the whole upstream response, including SSE.
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
