package main

import (
	"errors"
	"net/http"
	"time"
)

var errIncompleteToolIdentity = errors.New("上游工具调用缺少 ID 或名称，本次回复不完整")
var errPendingToolIdentityTooLarge = errors.New("上游工具身份延迟期间输出过大，本次回复不完整")
var errInvalidUpstreamChunk = errors.New("上游 SSE data 包含无效 JSON，本次回复不完整")

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

// aggregateUsageError carries measured usage when an upstream stream contains
// data that cannot be converted. The caller still returns a failure, but the
// account's real consumption must not be lost.
type aggregateUsageError struct {
	usage map[string]any
	cause error
}

func (e *aggregateUsageError) Error() string { return e.cause.Error() }
func (e *aggregateUsageError) Unwrap() error { return e.cause }

func aggregateFailureUsage(err error) (map[string]any, bool) {
	var identityError *aggregateToolIdentityError
	if errors.As(err, &identityError) {
		return identityError.usage, true
	}
	var usageError *aggregateUsageError
	if errors.As(err, &usageError) {
		return usageError.usage, true
	}
	return nil, false
}

func observeAggregateFailureUsage(r *http.Request, acc *Account, model string, reqID uint64, start time.Time, err error) {
	usage, ok := aggregateFailureUsage(err)
	if !ok {
		return
	}
	statsObserveUsage(r, usage, false)
	observeModelCredit(acc, model, usage, reqID)
	recordModelTokens(model, usage, reqID)
	recordModelLatency(model, time.Since(start))
}
