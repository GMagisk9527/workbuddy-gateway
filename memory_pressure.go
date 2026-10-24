package main

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memoryUsage struct{ used, limit uint64 }
type memorySample struct {
	runtime memoryUsage
	cgroups []memoryUsage
}

func (s memorySample) watermarks(high, low uint64) (atHigh, atLow bool) {
	atLow = true
	check := func(scope memoryUsage) {
		if scope.limit == 0 {
			return
		}
		// Divide first to avoid overflow even for large cgroup limits.
		threshold := func(p uint64) uint64 { return scope.limit/100*p + (scope.limit%100*p+99)/100 }
		atHigh = atHigh || scope.used >= threshold(high)
		atLow = atLow && scope.used <= threshold(low)
	}
	check(s.runtime)
	for _, scope := range s.cgroups {
		check(scope)
	}
	return
}

type cgroupMemoryFiles struct{ used, limit string }
type systemMemorySampler struct {
	mu     sync.Mutex
	last   time.Time
	cached memorySample
	files  []cgroupMemoryFiles
}

func newSystemMemorySampler() *systemMemorySampler {
	s := &systemMemorySampler{}
	if runtime.GOOS == "linux" {
		proc, _ := os.ReadFile("/proc/self/cgroup")
		s.files = cgroupMemoryPaths("/sys/fs/cgroup", string(proc))
	}
	return s
}

// Namespace roots and nested limits are both checked. Parent cgroups may impose
// the effective limit even when the process's own memory.max says "max".
func cgroupMemoryPaths(root, proc string) []cgroupMemoryFiles {
	var out []cgroupMemoryFiles
	seen := map[string]bool{}
	add := func(base, relative, usedName, limitName string) {
		base = filepath.Clean(base)
		dir := filepath.Join(base, strings.TrimLeft(relative, "/"))
		if dir != base && !strings.HasPrefix(dir, base+string(os.PathSeparator)) {
			return
		}
		for {
			file := filepath.Join(dir, usedName)
			if !seen[file] {
				seen[file] = true
				out = append(out, cgroupMemoryFiles{file, filepath.Join(dir, limitName)})
			}
			if dir == base {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	for _, line := range strings.Split(proc, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			add(root, fields[2], "memory.current", "memory.max")
		}
		for _, controller := range strings.Split(fields[1], ",") {
			if controller == "memory" {
				add(filepath.Join(root, "memory"), fields[2], "memory.usage_in_bytes", "memory.limit_in_bytes")
				add(root, fields[2], "memory.usage_in_bytes", "memory.limit_in_bytes")
			}
		}
	}
	add(root, "", "memory.current", "memory.max")
	add(filepath.Join(root, "memory"), "", "memory.usage_in_bytes", "memory.limit_in_bytes")
	add(root, "", "memory.usage_in_bytes", "memory.limit_in_bytes")
	return out
}

func readMemoryUsage(files cgroupMemoryFiles) (memoryUsage, bool) {
	limitBytes, err := os.ReadFile(files.limit)
	if err != nil {
		return memoryUsage{}, false
	}
	limit, err := strconv.ParseUint(strings.TrimSpace(string(limitBytes)), 10, 64)
	// v2 "max" and v1's near-MaxInt64 sentinel both mean unlimited.
	if err != nil || limit == 0 || limit >= 1<<60 {
		return memoryUsage{}, false
	}
	usedBytes, err := os.ReadFile(files.used)
	if err != nil {
		return memoryUsage{}, false
	}
	used, err := strconv.ParseUint(strings.TrimSpace(string(usedBytes)), 10, 64)
	return memoryUsage{used: used, limit: limit}, err == nil
}

func (s *systemMemorySampler) snapshot() memorySample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.last.IsZero() && time.Since(s.last) < 250*time.Millisecond {
		return s.cached
	}
	var sample memorySample
	limit := debug.SetMemoryLimit(-1) // A negative value only queries the Go limit.
	if limit > 0 && limit < 1<<60 {
		values := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
		metrics.Read(values)
		total, released := values[0].Value.Uint64(), values[1].Value.Uint64()
		sample.runtime = memoryUsage{used: total - min(total, released), limit: uint64(limit)}
	}
	for _, files := range s.files {
		if usage, ok := readMemoryUsage(files); ok {
			sample.cgroups = append(sample.cgroups, usage)
		}
	}
	s.cached, s.last = sample, time.Now()
	return sample
}
