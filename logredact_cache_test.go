package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestRedactionFieldMatcherWarmAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		if secretLogField("model") {
			t.Fatal("ordinary field became secret")
		}
	})
	if allocs != 0 {
		t.Fatalf("unchanged ordinary field should not allocate a matcher: %.0f allocs/call", allocs)
	}
	for _, key := range []string{"access_token", "Refresh-Token", "ADMIN_KEY", "X-API-Key", "authorization"} {
		if !secretLogField(key) {
			t.Errorf("secret field not recognized: %s", key)
		}
	}
}

func TestRedactionSecretMatcherWarmAllocations(t *testing.T) {
	isolateSecrets(t)
	for i := 0; i < 100; i++ {
		registerSecrets(fmt.Sprintf("synthetic-cached-secret-%03d", i))
	}
	const input = "traceId=keep status=403 reason=permission-denied"
	for _, duplicate := range []bool{false, true} {
		allocs := testing.AllocsPerRun(25, func() {
			if duplicate {
				registerSecrets("synthetic-cached-secret-000")
			}
			if got := redactSensitiveText(input); got != input {
				t.Fatalf("diagnostics changed: %q", got)
			}
		})
		// Enough room for regex results and duplicate registration, but not a
		// rebuild of the 100-secret matcher on every ordinary log line.
		if allocs > 64 {
			t.Errorf("warm redaction rebuilt the secret matcher: duplicate=%t allocs=%.0f", duplicate, allocs)
		}
	}
}

func TestRedactionRegistrationImmediatelyRefreshesMatcher(t *testing.T) {
	isolateSecrets(t)
	const old = "synthetic-old-secret"
	registerSecrets(old, "")
	_ = redactSensitiveText("warm the initial matcher")
	const next = old + "-rotated"
	const escaped = "synthetic-quoted\"secret\\line\nend"
	registerSecrets(next, escaped, "short", "短密")
	quoted := strconv.Quote(escaped)
	input := "traceId=keep " + old + " " + next + " " + quoted[1:len(quoted)-1] + " short shorter 短密 中文短密词"
	got := redactSensitiveText(input)
	want := "traceId=keep [REDACTED] [REDACTED] [REDACTED] [REDACTED] shorter [REDACTED] 中文短密词"
	if got != want {
		t.Fatalf("new registration, escaping, boundaries or historical masking changed: got %q want %q", got, want)
	}
	if again := redactSensitiveText(got); again != got {
		t.Fatalf("redaction not idempotent: %q", again)
	}
	registerSecrets(next, old)
	if again := redactSensitiveText(input); again != want {
		t.Fatal("duplicate registration changed redaction")
	}
}

func TestRedactionConcurrentDistinctRegistrations(t *testing.T) {
	isolateSecrets(t)
	const old = "synthetic-historical-secret"
	registerSecrets(old)
	_ = redactSensitiveText("warm the original snapshot")
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for i := 0; i < 16; i++ {
				secret := fmt.Sprintf("synthetic-concurrent-secret-%02d-%02d", worker, i)
				registerSecrets(secret)
				got := redactSensitiveText("traceId=keep " + secret + " " + old)
				if strings.Contains(got, secret) || strings.Contains(got, old) || strings.Count(got, "[REDACTED]") != 2 {
					t.Errorf("registration published a stale matcher: %q", got)
					return
				}
			}
		})
	}
	workers.Wait()
}
