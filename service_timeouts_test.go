package main

import (
	"testing"
	"time"
)

func TestServiceTimeoutConfigDefaultsReloadAndValidation(t *testing.T) {
	isolatePolicies(t)
	watcher := runtimeConfigWatcher{}
	writeRuntimeFixture(t, `{"downstream":{"writeTimeoutSeconds":9},"server":{"shutdownGraceSeconds":45,"shutdownCleanupSeconds":7}}`)
	if !watcher.reload() {
		t.Fatal("valid timeout config was rejected")
	}
	want := serviceTimeoutConfig{write: 9 * time.Second, grace: 45 * time.Second, cleanup: 7 * time.Second}
	if got := currentServiceTimeouts(); got != want {
		t.Fatalf("timeout config not applied: %+v", got)
	}
	for _, bad := range []string{
		`{"downstream":{"writeTimeoutSeconds":-1}}`,
		`{"downstream":{"writeTimeoutSeconds":3601}}`,
		`{"server":{"shutdownGraceSeconds":-1}}`,
		`{"server":{"shutdownGraceSeconds":3601}}`,
		`{"server":{"shutdownCleanupSeconds":301}}`,
	} {
		writeRuntimeFixture(t, bad)
		if watcher.reload() || currentServiceTimeouts() != want {
			t.Fatalf("invalid config partially applied: %s", bad)
		}
	}
	writeRuntimeFixture(t, `{}`)
	if !watcher.reload() {
		t.Fatal("default reload failed")
	}
	defaults, _ := normalizeServiceTimeouts(downstreamFileConfig{}, serverFileConfig{})
	if currentServiceTimeouts() != defaults {
		t.Fatal("omitted config retained old timeouts")
	}
}
