package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestMessagesToolInputFailureRetainsMeasuredUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, finish := range []string{"tool_calls", "length"} {
			for _, valid := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/%s/valid=%t", stream, finish, valid), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					requestStats.Lock()
					requestStats.enabled = true
					requestStats.Unlock()
					modelStatsMu.Lock()
					beforeTokens := modelStatLocked(compatModel).Tokens
					modelStatsMu.Unlock()

					args := `{"path":`
					if valid {
						args = `{}`
					}
					upstream := strings.TrimSuffix(toolInputValidationSSE(t, args, finish), "data: [DONE]\n\n") +
						"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13,\"credit\":0.25}}\n\ndata: [DONE]\n\n"
					h.reply.Store(&compatReply{200, upstream})
					body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Synthetic tool request."}}}
					status, out := h.send(t, "/v1/messages", body, nil, 0)
					wantStatus := 200
					if !valid && !stream {
						wantStatus = 502
					}
					if status != wantStatus {
						t.Errorf("HTTP status=%d want=%d", status, wantStatus)
					}
					if stream && !valid && (!strings.Contains(string(out), "event: error\n") || strings.Contains(string(out), "event: message_stop\n")) {
						t.Error("invalid tool parameters were reported as a complete stream")
					}

					requestStats.Lock()
					rows := append([]statsRequest(nil), requestStats.document.Requests...)
					requestStats.Unlock()
					if len(rows) != 1 {
						t.Fatalf("expected exactly one recorded attempt, got %d", len(rows))
					}
					row := rows[0]
					if row.Complete != valid || row.Status != wantStatus {
						t.Errorf("attempt completeness/status changed: complete=%t status=%d", row.Complete, row.Status)
					}
					if row.InputTokens == nil || *row.InputTokens != 10 || row.OutputTokens == nil || *row.OutputTokens != 3 || row.TotalTokens == nil || *row.TotalTokens != 13 || row.Credit == nil || *row.Credit != 0.25 {
						t.Error("measured upstream usage was lost or altered")
					}
					if !valid && row.TTFTMS != nil {
						t.Error("invalid tool parameters produced a complete first-token sample")
					}

					h.key.mu.Lock()
					usage := h.key.usage
					h.key.mu.Unlock()
					if usage.Requests != 1 || usage.Total != 0.25 || usage.Input != 10 || usage.Output != 3 {
						t.Errorf("measured usage was not settled exactly once: %+v", usage)
					}
					modelStatsMu.Lock()
					afterTokens := modelStatLocked(compatModel).Tokens
					modelStatsMu.Unlock()
					if afterTokens-beforeTokens != 13 {
						t.Errorf("model tokens were lost or recorded more than once: delta=%d", afterTokens-beforeTokens)
					}
					accountMu.Lock()
					paidCount := 0
					for _, acc := range h.accounts {
						if state := acc.ModelStates[compatModel]; state != nil && state.CostClass == modelCostPaid && !state.ObservedAt.IsZero() {
							paidCount++
						}
					}
					accountMu.Unlock()
					if paidCount != 1 {
						t.Errorf("measured model cost was not observed exactly once: accounts=%d", paidCount)
					}
				})
			}
		}
	}
}
