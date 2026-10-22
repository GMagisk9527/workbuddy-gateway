package main

import (
	"testing"
	"time"
)

func TestRuntimeStreamKeepaliveValidationAndLastGood(t *testing.T) {
	isolatePolicies(t)
	oldKeepalive, oldRetry := currentStreamKeepalive(), currentFirstByteRetry()
	t.Cleanup(func() {
		upstreamConfigMu.Lock()
		streamKeepalive, enableFirstByteRetry = oldKeepalive, oldRetry
		upstreamConfigMu.Unlock()
	})
	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"omitted", `{}`, streamKeepaliveDefault},
		{"zero default", `{"upstream":{"streamKeepaliveSeconds":0}}`, streamKeepaliveDefault},
		{"positive", `{"upstream":{"streamKeepaliveSeconds":7}}`, 7 * time.Second},
		{"negative default", `{"upstream":{"streamKeepaliveSeconds":-1}}`, streamKeepaliveDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseRuntimeConfig([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRuntimeConfig(c, true, "stream-keepalive-test"); err != nil {
				t.Fatal(err)
			}
			if got := currentStreamKeepalive(); got != tc.want {
				t.Fatalf("keepalive = %v, want %v", got, tc.want)
			}
		})
	}
	watcher := runtimeConfigWatcher{}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"synthetic-admin","apiKeyEnabled":true,"apiKey":"synthetic-key"},"upstream":{"streamKeepaliveSeconds":7}}`)
	if !watcher.reload() {
		t.Fatal("valid configuration did not reload")
	}
	writeRuntimeFixture(t, `{"gateway":{"adminKey":"changed","apiKeyEnabled":false},"upstream":{"streamKeepaliveSeconds":9223372036854775807}}`)
	if watcher.reload() {
		t.Fatal("overflowing keepalive configuration was accepted")
	}
	if currentStreamKeepalive() != 7*time.Second || currentAdminKey() != "synthetic-admin" || currentAPIKey() != "synthetic-key" {
		t.Fatal("invalid keepalive partially applied settings or replaced active authentication")
	}
}
