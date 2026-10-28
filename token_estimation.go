package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

type tokenEstimationFileConfig struct {
	ModelMultipliers map[string]float64 `json:"modelMultipliers"`
}

var tokenEstimateMultipliers atomic.Pointer[map[string]float64]

func normalizeTokenEstimation(c tokenEstimationFileConfig) (map[string]float64, error) {
	if len(c.ModelMultipliers) > 128 {
		return nil, fmt.Errorf("tokenEstimation 最多配置128个模型")
	}
	out := make(map[string]float64, len(c.ModelMultipliers))
	for raw, factor := range c.ModelMultipliers {
		name := normalizeModelName(raw)
		if !validModelName(raw) || name == "" || math.IsNaN(factor) || math.IsInf(factor, 0) || factor < 0.25 || factor > 4 {
			return nil, fmt.Errorf("tokenEstimation 模型名无效或倍率不在0.25～4之间")
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("tokenEstimation 模型名规范化后重复")
		}
		out[name] = factor
	}
	return out, nil
}

// Aggregate categories across chunks before rounding. No history/tool JSON is
// joined or re-serialized; inputs are the already validated decoded JSON tree.
type inputTokenCounter struct{ cjk, other, images int64 }

func (c *inputTokenCounter) text(s string) {
	var cjk, other int64
	for len(s) > 0 {
		// Code/history is usually mostly ASCII. Skip runs without per-rune
		// decoding, while keeping the original Unicode category semantics.
		if len(s) >= 8 && (s[0]|s[1]|s[2]|s[3]|s[4]|s[5]|s[6]|s[7]) < utf8.RuneSelf {
			other += 8
			s = s[8:]
			continue
		}
		if s[0] < utf8.RuneSelf {
			other++
			s = s[1:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		if r >= 0x2e80 {
			cjk++
		} else {
			other++
		}
	}
	c.cjk += cjk
	c.other += other
}
func (c *inputTokenCounter) quoted(s string) {
	var cjk int64
	other := int64(2)
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		s = s[n:]
		switch {
		case r == utf8.RuneError && n == 1:
			other += 6
		case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			other += 2
		case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
			other += 6
		case r >= 0x2e80:
			cjk++
		default:
			other++
		}
	}
	c.cjk += cjk
	c.other += other
}
func (c *inputTokenCounter) json(v any) {
	switch v := v.(type) {
	case nil:
		c.other += 4
	case string:
		c.quoted(v)
	case bool:
		if v {
			c.other += 4
		} else {
			c.other += 5
		}
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			c.other += 4
			return
		}
		var buf [32]byte
		format := byte('f')
		a := math.Abs(v)
		if a != 0 && (a < 1e-6 || a >= 1e21) {
			format = 'e'
		}
		data := strconv.AppendFloat(buf[:0], v, format, -1, 64)
		n := len(data)
		if format == 'e' && n >= 4 && data[n-3] == '-' && data[n-2] == '0' {
			n--
		}
		c.other += int64(n)
	case int:
		var b [32]byte
		c.other += int64(len(strconv.AppendInt(b[:0], int64(v), 10)))
	case int64:
		var b [32]byte
		c.other += int64(len(strconv.AppendInt(b[:0], v, 10)))
	case json.Number:
		c.other += int64(len(v))
	case []any:
		c.other += 2
		if len(v) > 0 {
			c.other += int64(len(v) - 1)
		}
		for _, item := range v {
			c.json(item)
		}
	case map[string]any:
		c.other += 2
		if len(v) > 0 {
			c.other += int64(len(v) - 1)
		}
		for key, value := range v {
			c.quoted(key)
			c.other++
			c.json(value)
		}
	}
}

func (c *inputTokenCounter) content(value any) {
	if text, ok := value.(string); ok {
		c.text(text)
		return
	}
	blocks, _ := value.([]any)
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strOf(block["type"]) {
		case "text":
			c.text(strOf(block["text"]))
		case "thinking":
			c.text(strOf(block["thinking"]))
		case "image":
			c.images += estimateImageInputTokens(block)
		case "tool_result":
			c.content(block["content"])
		case "tool_use":
			c.text(strOf(block["name"]))
			c.json(block["input"])
		}
	}
}

// A generic image heuristic, not an upstream tokenizer. Decode only bounded
// headers, never pixels, remote URLs or the full base64 payload. Unknown formats
// and dimensions get an explicit fallback instead of being counted as zero.
func estimateImageInputTokens(block map[string]any) int64 {
	media, data := anthropicImageSource(block)
	if media == "" {
		return 1600
	}
	reader := io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)), 64<<10)
	cfg, _, err := image.DecodeConfig(reader)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 1600
	}
	area := float64(cfg.Width) * float64(cfg.Height)
	scale := math.Min(1, 1568/float64(max(cfg.Width, cfg.Height)))
	return max(1, int64(math.Ceil(math.Min(1200000, area*scale*scale)/750)))
}

func estimateAnthropicInputTokens(body map[string]any) int {
	var c inputTokenCounter
	switch system := body["system"].(type) {
	case string:
		if system != "" {
			c.text(system)
			c.other++
		}
	case []any:
		has := false
		for _, raw := range system {
			if b, ok := raw.(map[string]any); ok && b["type"] == "text" {
				if text := strOf(b["text"]); text != "" {
					if has {
						c.other++
					}
					c.text(text)
					has = true
				}
			}
		}
		if has {
			c.other++
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			if message, ok := raw.(map[string]any); ok {
				c.content(message["content"])
				c.other++
			}
		}
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		c.json(tools)
	}
	count := c.cjk + (c.other+3)/4 + c.images
	factor := 1.0
	if table := tokenEstimateMultipliers.Load(); table != nil {
		if f, ok := (*table)[normalizeModelName(strOf(body["model"]))]; ok {
			factor = f
		}
	}
	return int(math.Min(math.MaxInt32, math.Ceil(float64(count)*factor)))
}
