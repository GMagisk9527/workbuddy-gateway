package main

// 亲和默认值回归：省略 affinity 段时应默认开启，显式 false 才关闭。
// 走真实加载入口 parseRuntimeConfig + applyRuntimeConfig，而不是直接调 setAffinityConfig。

import (
	"testing"
)

func applyRuntimeConfigForTest(t *testing.T, raw string) {
	t.Helper()
	c, err := parseRuntimeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if err := applyRuntimeConfig(c, false, ""); err != nil {
		t.Fatalf("应用配置失败: %v", err)
	}
}

func TestAffinityDefaultsEnabledWhenSectionOmitted(t *testing.T) {
	silenceLogs(t)
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setAffinityConfig(false, affinityModeHybrid, 1800, 64, 4096) })

	applyRuntimeConfigForTest(t, `{"gateway":{"apiPort":31817,"webPort":21816}}`)
	if got := affinitySnapshot()["enabled"]; got != true {
		t.Fatalf("省略 affinity 段应默认开启，实际 %v", got)
	}
}

func TestAffinityExplicitFalseDisables(t *testing.T) {
	silenceLogs(t)
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setAffinityConfig(false, affinityModeHybrid, 1800, 64, 4096) })

	applyRuntimeConfigForTest(t, `{"gateway":{"apiPort":31817,"webPort":21816},"affinity":{"enabled":false}}`)
	if got := affinitySnapshot()["enabled"]; got != false {
		t.Fatalf("显式 enabled:false 应关闭亲和，实际 %v", got)
	}
}

func TestAffinityExplicitConfigIsApplied(t *testing.T) {
	silenceLogs(t)
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setAffinityConfig(false, affinityModeHybrid, 1800, 64, 4096) })

	applyRuntimeConfigForTest(t, `{"gateway":{"apiPort":31817,"webPort":21816},"affinity":{"enabled":true,"mode":"prefix","ttlSeconds":600,"capacity":128,"prefixHeadBytes":8192}}`)
	snap := affinitySnapshot()
	if snap["enabled"] != true {
		t.Fatalf("应开启，实际 %v", snap["enabled"])
	}
	if snap["mode"] != affinityModePrefix {
		t.Fatalf("模式应为 prefix，实际 %v", snap["mode"])
	}
	if snap["ttlSeconds"] != 600 {
		t.Fatalf("TTL 应为 600 秒，实际 %v", snap["ttlSeconds"])
	}
	if snap["capacity"] != 128 {
		t.Fatalf("容量应为 128，实际 %v", snap["capacity"])
	}
}
