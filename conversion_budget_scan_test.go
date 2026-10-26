package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func checkConversionStringBudget(t testing.TB, text string) {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(string(encoded))
	size, err := measureConversionJSON(text, len(encoded))
	if err != nil || size.plain != len(encoded) || size.quoted != len(quoted)-2 {
		t.Fatalf("size=%+v want=%d/%d err=%v", size, len(encoded), len(quoted)-2, err)
	}
	_, err = measureConversionJSON(text, len(encoded)-1)
	var tooLarge *conversionSizeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("exact size boundary accepted or returned the wrong error: %v", err)
	}
}

func TestConversionJSONAllBytesAndBudgetEdges(t *testing.T) {
	for value := 0; value < 256; value++ {
		checkConversionStringBudget(t, strings.Repeat("ordinary text ", 4)+string([]byte{byte(value)})+" tail")
	}
}

func FuzzConversionJSONStringBudget(f *testing.F) {
	for _, text := range []string{"", "ordinary text", "\x00\b\f\n\r\t\"\\<>&", "中文🙂\u2028\u2029", "\xff\xfe\xc0\x80", strings.Repeat("synthetic ", 4096)} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 64<<10 {
			return
		}
		checkConversionStringBudget(t, text)
	})
}
