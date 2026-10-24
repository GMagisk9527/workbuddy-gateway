package main

import (
	"errors"
	"net/http"
	"time"
)

var errIncompleteToolIdentity = errors.New("上游工具调用缺少 ID 或名称，本次回复不完整")
var errPendingToolIdentityTooLarge = errors.New("上游工具身份延迟期间输出过大，本次回复不完整")

// Bound only arguments waiting for identity, not the normal accumulated tool
// output. Multiple unidentified calls share one budget rather than one per slot.
type pendingToolArgumentBudget struct {
	bytes int
	slots map[int]int
}

func (b *pendingToolArgumentBudget) reserve(index int, ready bool, additional, eventBytes int) error {
	if ready {
		b.bytes -= b.slots[index]
		delete(b.slots, index)
		return nil
	}
	if additional > maxPendingToolEventBytes-b.bytes-eventBytes {
		return errPendingToolIdentityTooLarge
	}
	if additional > 0 {
		if b.slots == nil {
			b.slots = make(map[int]int)
		}
		b.bytes += additional
		b.slots[index] += additional
	}
	return nil
}

func validateMergedToolIdentity(calls map[int]*mergedToolCall, order []int) error {
	for _, index := range order {
		call := calls[index]
		if call == nil || call.ID == "" || call.Name == "" {
			return errIncompleteToolIdentity
		}
	}
	return nil
}

// Conversion failure must not erase consumption already reported by upstream.
type aggregateToolIdentityError struct {
	usage map[string]any
}

func (e *aggregateToolIdentityError) Error() string { return errIncompleteToolIdentity.Error() }
func (e *aggregateToolIdentityError) Unwrap() error { return errIncompleteToolIdentity }

func observeAggregateToolIdentityFailure(r *http.Request, acc *Account, model string, reqID uint64, start time.Time, err error) {
	var identityError *aggregateToolIdentityError
	if !errors.As(err, &identityError) {
		return
	}
	statsObserveUsage(r, identityError.usage, false)
	observeModelCredit(acc, model, identityError.usage, reqID)
	recordModelTokens(model, identityError.usage, reqID)
	recordModelLatency(model, time.Since(start))
}
