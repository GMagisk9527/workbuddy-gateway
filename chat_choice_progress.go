package main

import (
	"encoding/json"
	"math"
)

type chatChoiceFinish struct {
	index    int
	finished bool
}

// Track each candidate independently: one finished choice must not complete
// another choice or hide candidates missing from a requested n>1 response.
type chatChoiceProgress struct {
	expected int
	finished map[int]bool
}

func requestedChatChoices(request []byte) int {
	var options struct {
		N int `json:"n"`
	}
	if len(request) > 0 && json.Unmarshal(request, &options) == nil && options.N > 1 {
		return options.N
	}
	return 1
}

func newChatChoiceProgress(expected int) *chatChoiceProgress {
	return &chatChoiceProgress{expected: expected, finished: make(map[int]bool)}
}

func (p *chatChoiceProgress) observeMeta(choices []chatChoiceFinish) {
	for _, choice := range choices {
		p.finished[choice.index] = p.finished[choice.index] || choice.finished
	}
}

func (p *chatChoiceProgress) observeChunk(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := 0 // Preserve legacy single-choice chunks without an index.
		if value, present := choice["index"]; present {
			number, ok := value.(float64)
			if !ok || number < 0 || number != math.Trunc(number) || number >= float64(int(^uint(0)>>1)) {
				return false
			}
			index = int(number)
		}
		finish, _ := choice["finish_reason"].(string)
		p.finished[index] = p.finished[index] || finish != ""
	}
	return true
}

func (p *chatChoiceProgress) complete() bool {
	if len(p.finished) < p.expected {
		return false
	}
	for index := 0; index < p.expected; index++ {
		if !p.finished[index] {
			return false
		}
	}
	for _, finished := range p.finished {
		if !finished {
			return false
		}
	}
	return true
}
