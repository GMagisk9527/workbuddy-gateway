package main

import (
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxModelNameBytes = 256
	maxTrackedModels  = 1024
	overflowModelStat = "(其他模型)"
)

func validModelName(model string) bool {
	if len(model) == 0 || len(model) > maxModelNameBytes || !utf8.ValidString(model) {
		return false
	}
	return strings.IndexFunc(model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

// Older snapshots can exceed the new budget. Keep quota blocks first and the
// longest remaining timed protections next. Any omitted timed block has expired
// before a retained timed slot becomes evictable, so trimming cannot bypass it.
func boundedModelStateSnapshots(input map[string]modelStateSnapshot) map[string]modelStateSnapshot {
	if len(input) == 0 {
		return nil
	}
	ids := make([]string, 0, len(input))
	for model := range input {
		if validModelName(model) && validModelName(normalizeModelName(model)) {
			ids = append(ids, model)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := input[ids[i]], input[ids[j]]
		if a.QuotaBlocked != b.QuotaBlocked {
			return a.QuotaBlocked
		}
		x, y := max(a.CooldownUntil, a.NextProbeAt), max(b.CooldownUntil, b.NextProbeAt)
		if x != y {
			return x > y
		}
		if a.ObservedAt != b.ObservedAt {
			return a.ObservedAt > b.ObservedAt
		}
		return ids[i] < ids[j]
	})
	out := make(map[string]modelStateSnapshot, min(len(ids), maxTrackedModels))
	for _, id := range ids {
		model := normalizeModelName(id)
		if _, exists := out[model]; exists {
			continue
		}
		row := input[id]
		row.LastReason = truncate(redactSensitiveText(row.LastReason), 400)
		out[model] = row
		if len(out) == maxTrackedModels {
			break
		}
	}
	return out
}

func requestModel(body map[string]any) (string, error) {
	if body == nil {
		return "", errors.New("请求体必须是 JSON 对象")
	}
	raw, exists := body["model"]
	if !exists || raw == "" {
		return "hy4-preview", nil
	}
	model, ok := raw.(string)
	if !ok || !validModelName(model) {
		return "", errors.New("model 必须是 1～256 字节的有效 UTF-8 字符串，不能包含空白或控制字符")
	}
	return model, nil
}

// Only inactive observations may be evicted. Scheduling blocks are never
// discarded just because a caller supplies a fresh model name.
func canTrackAccountModel(acc *Account, model string) bool {
	if model != "" && !validModelName(model) {
		return false
	}
	if acc.ModelStates[normalizeModelName(model)] != nil || len(acc.ModelStates) < maxTrackedModels {
		return true
	}
	_, ok := accountModelEvictionCandidate(acc, time.Now())
	return ok
}

func accountModelEvictionCandidate(acc *Account, now time.Time) (string, bool) {
	var name string
	var oldest time.Time
	found := false
	for model, state := range acc.ModelStates {
		if state != nil && (state.QuotaBlocked || state.CooldownUntil.After(now) || state.NextProbeAt.After(now)) {
			continue
		}
		observed := time.Time{}
		if state != nil {
			observed = state.ObservedAt
		}
		if !found || observed.Before(oldest) {
			name, oldest, found = model, observed, true
		}
	}
	return name, found
}
