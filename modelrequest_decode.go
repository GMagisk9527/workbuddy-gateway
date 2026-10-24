package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Preserve JSON numbers in schemas, tool data and extension fields. Standard
// scalar generation options retain their previous float64 representation.
func decodeModelRequestBody(raw []byte, body *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(body); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体必须是单个 JSON 对象")
	}
	for _, key := range []string{
		"max_tokens", "max_completion_tokens", "max_output_tokens",
		"temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty",
		"n", "top_logprobs", "max_tool_calls",
	} {
		if n, ok := (*body)[key].(json.Number); ok {
			value, err := n.Float64()
			if err != nil {
				return fmt.Errorf("%s 数值超出支持范围", key)
			}
			(*body)[key] = value
		}
	}
	return nil
}
