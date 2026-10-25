//go:build go1.27 && goexperiment.jsonv2

package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
)

// Keep v1 object/string semantics, but decode directly from the already bounded
// input rather than growing a second streaming buffer. Override only numbers
// held in interface values, matching Decoder.UseNumber without float rounding.
var requestNumberOptions = jsonv2.JoinOptions(json.DefaultOptionsV1(),
	jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(func(d *jsontext.Decoder, out *any) error {
		if d.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		token, err := d.ReadToken()
		if err != nil {
			return err
		}
		*out = json.Number(token.String())
		return nil
	})))

func decodeRequestJSONNumbers(raw []byte, body *map[string]any) error {
	return jsonv2.Unmarshal(raw, body, requestNumberOptions)
}
