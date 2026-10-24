package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMemoryFixture(t *testing.T, dir, usedName, limitName, used, limit string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{usedName: used, limitName: limit} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryPressureNestedCgroupV2AndSamplingCache(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "tenant")
	leaf := filepath.Join(parent, "worker")
	writeMemoryFixture(t, root, "memory.current", "memory.max", "100", "512")
	writeMemoryFixture(t, parent, "memory.current", "memory.max", "96", "128")
	writeMemoryFixture(t, leaf, "memory.current", "memory.max", "10", "max")
	s := &systemMemorySampler{files: cgroupMemoryPaths(root, "0::/tenant/worker\n")}
	sample := s.snapshot()
	sample.runtime = memoryUsage{} // This fixture tests cgroups, independently of GOMEMLIMIT.
	if len(sample.cgroups) != 2 {
		t.Fatalf("expected finite ancestor limits, got %+v", sample.cgroups)
	}
	high, low := sample.watermarks(85, 75)
	if high || !low {
		t.Fatal("wrong parent memory pressure")
	}
	writeMemoryFixture(t, parent, "memory.current", "memory.max", "120", "128")
	if high, _ := (memorySample{cgroups: s.snapshot().cgroups}).watermarks(85, 75); high {
		t.Fatal("sampler ignored cache interval")
	}
	s.last = time.Now().Add(-time.Second)
	if high, _ := (memorySample{cgroups: s.snapshot().cgroups}).watermarks(85, 75); !high {
		t.Fatal("sampler did not refresh parent pressure")
	}
}

func TestMemoryPressureCgroupV1AndUnavailableMonitors(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "memory")
	leaf := filepath.Join(base, "worker")
	writeMemoryFixture(t, base, "memory.usage_in_bytes", "memory.limit_in_bytes", "1", "9223372036854771712")
	writeMemoryFixture(t, leaf, "memory.usage_in_bytes", "memory.limit_in_bytes", "85", "100")
	s := &systemMemorySampler{files: cgroupMemoryPaths(root, "7:cpu,memory:/worker\n")}
	sample := s.snapshot()
	if len(sample.cgroups) != 1 {
		t.Fatalf("unlimited v1 sentinel was treated as a real limit: %+v", sample.cgroups)
	}
	if high, _ := sample.watermarks(85, 75); !high {
		t.Fatal("v1 pressure ignored")
	}
	if _, ok := readMemoryUsage(cgroupMemoryFiles{filepath.Join(root, "missing"), filepath.Join(root, "missing-limit")}); ok {
		t.Fatal("missing monitor accepted")
	}
	for _, files := range cgroupMemoryPaths(root, "0::/../../outside\n") {
		if rel, err := filepath.Rel(root, files.used); err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator) {
			t.Fatalf("cgroup path escaped fixture root: %s", files.used)
		}
	}
}

func TestMemoryPressureWatermarksDoNotOverflow(t *testing.T) {
	s := memorySample{runtime: memoryUsage{^uint64(0) - 1, ^uint64(0)}}
	high, low := s.watermarks(85, 75)
	if !high || low {
		t.Fatal("large limit overflowed watermark arithmetic")
	}
}
