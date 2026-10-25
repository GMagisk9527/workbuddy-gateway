package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func legacyRequestNumbers(raw []byte, body *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(body); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func TestLargeContextDecodePreservesLegacyValues(t *testing.T) {
	fixtures := [][]byte{
		[]byte(`{"numbers":[-0,1.2300e+05,9007199254740993,0.123456789012345678901,1e99999],"bool":true,"nil":null}`),
		[]byte(`{"duplicate":{"old":1},"duplicate":{"new":2},"array":[{"id":9007199254740993},false,"text"]}`),
		[]byte(`{"duplicate":[1,2],"duplicate":[3],"escaped\u005fkey":"\ud800\n<>&\u2028\u2029"}`),
		{'{', '"', 0xff, '"', ':', '"', 0xfe, '"', '}'}, []byte("null \r\n\t"),
	}
	large := []byte(`{"content":"` + strings.Repeat("synthetic line\\n ", 8192) + `","tool":{"id":9007199254740993}}`)
	fixtures = append(fixtures, large)
	rng := rand.New(rand.NewSource(20261025))
	for i := 0; i < 128; i++ {
		text := make([]byte, rng.Intn(256))
		_, _ = rng.Read(text)
		raw, err := json.Marshal(map[string]any{"tool": map[string]any{"id": json.Number("9007199254740993"), "decimal": json.Number("1.2300e+05"), "content": string(text), "values": []any{nil, true, map[string]any{"nested": json.Number("-0")}}}})
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, raw)
	}
	for _, raw := range fixtures {
		var want map[string]any
		if err := legacyRequestNumbers(raw, &want); err != nil {
			t.Fatal(err)
		}
		for _, decode := range []func([]byte, *map[string]any) error{decodeModelRequestBody, decodeAnthropicMessageBody} {
			input := bytes.Clone(raw)
			var got map[string]any
			if err := decode(input, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("request numbers or duplicate-key behavior changed: %v", got)
			}
			if !bytes.Equal(input, raw) {
				t.Fatal("decoder mutated input")
			}
			for i := range input {
				input[i] = 'x'
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("decoded values alias mutable request bytes")
			}
		}
	}
}

func TestLargeContextDecodeRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{"", `{} {}`, `{} null`, `{} invalid`, `[]`, `{"number":01}`, `{"number":1e}`, `{"text":"unterminated}`, "\xef\xbb\xbf{}", strings.Repeat("[", 10002) + strings.Repeat("]", 10002), strings.Repeat(`{"a":`, 10002) + `0` + strings.Repeat(`}`, 10002)} {
		for _, decode := range []func([]byte, *map[string]any) error{decodeModelRequestBody, decodeAnthropicMessageBody} {
			var body map[string]any
			if err := decode([]byte(raw), &body); err == nil {
				t.Fatalf("malformed request accepted: len=%d", len(raw))
			}
		}
	}
}

func TestLargeContextStringSizeMatchesJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(20261025))
	texts := []string{"", strings.Repeat("synthetic ascii ", 8192), strings.Repeat("中文🙂\n\"\\<>&\u2028\u2029\xff", 2048)}
	for _, n := range []int{1, 31, 32, 63, 64, 255, 1024} {
		prefix := strings.Repeat("a", n)
		for _, suffix := range []string{"\x00", "\x7f", "\n", "\"", "\\", "<>&", "中", "\xe2\x80", "\xff"} {
			texts = append(texts, prefix+suffix+prefix)
		}
	}
	for i := 0; i < 200; i++ {
		data := make([]byte, rng.Intn(1024))
		_, _ = rng.Read(data)
		texts = append(texts, string(data))
	}
	for _, text := range texts {
		encoded, _ := json.Marshal(text)
		quoted, _ := json.Marshal(string(encoded))
		size, err := measureConversionJSON(text, len(encoded))
		if err != nil || size.plain != len(encoded) || size.quoted != len(quoted)-2 {
			t.Fatalf("string size changed: len=%d size=%+v err=%v", len(text), size, err)
		}
		if _, err := measureConversionJSON(text, len(encoded)-1); err == nil {
			t.Fatalf("size boundary bypassed: len=%d", len(text))
		}
	}
}

func TestDecodedRequestBodyReadReusesRemainingBytes(t *testing.T) {
	data := []byte("synthetic validated body")
	body := newDecodedRequestBody(data)
	var prefix [3]byte
	_, _ = body.Read(prefix[:])
	_ = body.Close()
	r := &http.Request{Body: body}
	got, err := readModelRequestBody(r)
	if err != nil || !bytes.Equal(got, data[3:]) {
		t.Fatalf("remaining body changed: %q %v", got, err)
	}
	if &got[0] != &data[3] {
		t.Fatal("validated decompressed body was copied again")
	}
	again, err := readModelRequestBody(r)
	if err != nil || len(again) != 0 {
		t.Fatal("body was not consumed exactly once")
	}
}

func TestDecodedRequestBodyReadHonorsOuterLimit(t *testing.T) {
	r := &http.Request{Body: http.MaxBytesReader(nil, newDecodedRequestBody([]byte("synthetic input")), 4)}
	_, err := readModelRequestBody(r)
	var limitErr *http.MaxBytesError
	if !errors.As(err, &limitErr) {
		t.Fatalf("outer body limit bypassed: %v", err)
	}
}

func TestReadModelRequestBodyMatchesReadAll(t *testing.T) {
	for _, length := range []int64{-1, 0, 1, 7, 24, 100, requestDecodedLimit + 1} {
		data := []byte("synthetic request bytes")
		r := &http.Request{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: length}
		got, err := readModelRequestBody(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("length=%d changed body: %q %v", length, got, err)
		}
		limited := &http.Request{Body: http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(data)), 4), ContentLength: length}
		partial, err := readModelRequestBody(limited)
		var sizeErr *http.MaxBytesError
		if !errors.As(err, &sizeErr) || !bytes.Equal(partial, data[:4]) {
			t.Fatalf("length=%d bypassed outer limit: %q %v", length, partial, err)
		}
		failed := &http.Request{Body: unexpectedEOFAuditBody{io.NopCloser(bytes.NewReader(data))}, ContentLength: length}
		partial, err = readModelRequestBody(failed)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(partial, data) {
			t.Fatalf("length=%d changed read failure: %q %v", length, partial, err)
		}
	}
}

func TestLargeContextConcurrentDecode(t *testing.T) {
	raw := []byte(`{"content":"synthetic input","tool":{"number":9007199254740993}}`)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				var body map[string]any
				if err := decodeAnthropicMessageBody(raw, &body); err != nil {
					t.Error(err)
					return
				}
				if body["tool"].(map[string]any)["number"] != json.Number("9007199254740993") {
					t.Error("concurrent number decoding changed precision")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzRequestJSONNumbersParity(f *testing.F) {
	for _, seed := range []string{`{"n":9007199254740993}`, `{"n":-0,"values":[1.200e+4,null,true]}`, `{"a":{"x":1},"a":{"y":2}}`, `{"text":"\ud800\n<>&"}`, `null`, `{} {}`, "{\"x\":\"\xff\"}"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		var want, got map[string]any
		oldErr := legacyRequestNumbers(raw, &want)
		newErr := decodeRequestJSONNumbers(raw, &got)
		if (oldErr == nil) != (newErr == nil) {
			t.Fatalf("JSON acceptance changed: old=%v new=%v", oldErr, newErr)
		}
		if oldErr == nil && !reflect.DeepEqual(want, got) {
			t.Fatal("JSON values or number representation changed")
		}
	})
}
