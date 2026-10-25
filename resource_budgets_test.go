package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestResourceBudgetsConfigLastGoodAndLogSettingsPreserved(t *testing.T) {
	isolatePolicies(t)
	watcher := runtimeConfigWatcher{}
	good := `{"output":{"maxRequestBytes":4096,"maxSharedBytes":8192},"retry":{"maxExtraAttempts":1,"maxConcurrent":1,"ratioPercent":10,"minAllowance":1,"backoffMaxMs":50},"logs":{"retentionDays":0,"maxFileBytes":1024,"maxTotalBytes":8192,"maxFiles":8,"sampleFirst":4,"sampleEvery":8},"gateway":{"adminKey":"synthetic-admin"}}`
	writeRuntimeFixture(t, good)
	if !watcher.reload() {
		t.Fatal("valid resource budgets rejected")
	}
	for _, bad := range []string{`{"output":{"maxSharedBytes":-1}}`, `{"retry":{"maxExtraAttempts":65}}`, `{"retry":{"maxConcurrent":-1}}`, `{"retry":{"ratioPercent":101}}`, `{"retry":{"backoffMaxMs":60001}}`, `{"logs":{"maxFileBytes":1024,"maxTotalBytes":512}}`, `{"logs":{"maxFiles":-1}}`, `{"logs":{"sampleEvery":-1}}`} {
		writeRuntimeFixture(t, bad)
		if watcher.reload() {
			t.Fatalf("invalid budget accepted: %s", bad)
		}
		if retainedOutputs.config.MaxRequestBytes != 4096 || upstreamRetries.limits.extra != 1 || currentLogLimits().file != 1024 {
			t.Fatal("invalid budget partially changed live settings")
		}
	}
	writeRuntimeFixture(t, good)
	watcher.reload()
	if err := saveLogRetention(7, "synthetic"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(runtimeConfigFile)
	var after runtimeFileConfig
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if after.Logs.RetentionDays != 7 || after.Logs.MaxFileBytes != 1024 || after.Logs.SampleEvery != 8 || after.Output.MaxSharedBytes != 8192 {
		t.Fatal("retention editor erased independent limits")
	}
}
