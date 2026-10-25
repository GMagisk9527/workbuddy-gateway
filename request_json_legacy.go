//go:build !go1.27 || !goexperiment.jsonv2

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func decodeRequestJSONNumbers(raw []byte, body *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(body); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体必须是单个 JSON 对象")
	}
	return nil
}
