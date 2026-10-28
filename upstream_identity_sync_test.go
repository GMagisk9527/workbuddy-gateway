package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpstreamIdentityDefaultsRespectMarkdownAndOptIn(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	const code = "\n`workspace root\nare paths the user`\n```json\n{\"workspace root\":\"are paths the user\"}\n```\n"
	const original = "workspace root and are paths the user." + code
	for _, tc := range []struct{ name, config, want string }{
		{"omitted", `{}`, original},
		{"empty", `{"identity":{}}`, original},
		{"disabled", `{"identity":{"enabled":false}}`, original},
		{"explicit", `{"identity":{"enabled":true}}`, "workspace base and are paths that the user." + code},
		{"custom", `{"identity":{"replacements":{"workspace root":"custom base"}}}`, "custom base and are paths the user." + code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseRuntimeConfig([]byte(tc.config))
			if err != nil {
				t.Fatal(err)
			}
			setIdentityConfig(c.Identity)
			if got, _ := normalizeAssistantIdentityText(original); got != tc.want {
				t.Fatalf("identity text=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestUpstreamSyncLeavesExistingIdentityConfigBytesUnchanged(t *testing.T) {
	for _, original := range []string{
		"{\n  \"gateway\": {\"apiPort\": 18317}\n}\n",
		`{"identity":{"enabled":false}}`,
		`{"identity":{"target":"Keep","replacements":{"kimi":"custom"}}}`,
		`{"identity":{"enabled":true,"replacements":null},"promptCompatibility":{"replacements":{"private-rule":"old"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ensureRuntimeConfigFile(path); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != original {
			t.Fatal("upstream sync changed existing config bytes")
		}
	}
}
