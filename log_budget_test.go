package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func withLogBudget(t *testing.T, c logFileConfig) {
	t.Helper()
	chdirTemp(t)
	old := logBudgetConfig.Load()
	l, err := normalizeLogBudget(c)
	if err != nil {
		t.Fatal(err)
	}
	logBudgetConfig.Store(&l)
	t.Cleanup(func() { logBudgetConfig.Store(old) })
}
func TestLogByteAndFileBudgetsRotateWithoutReusingNames(t *testing.T) {
	withLogBudget(t, logFileConfig{MaxFileBytes: 64, MaxTotalBytes: 256, MaxFiles: 3})
	w, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if _, err := fmt.Fprintf(w, "record-%03d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	last := w.path
	_ = w.Close()
	files := listLogFiles()
	var total int64
	if len(files) > 3 {
		t.Fatal("file count unbounded")
	}
	for _, file := range files {
		total += file.Size
		if file.Size > 64 {
			t.Fatalf("file too large: %+v", file)
		}
	}
	if total > 256 {
		t.Fatal("total storage unbounded")
	}
	lines, _, err := readWebLog(filepath.Base(last), 10)
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "record-029") {
		t.Fatal("rotated log not available in UI")
	}
	w, err = newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.path != last {
		t.Fatal("restart reopened an older segment")
	}
	for range 8 {
		if _, err := w.Write([]byte("new-record\n")); err != nil {
			t.Fatal(err)
		}
	}
	if filepath.Base(w.path) <= filepath.Base(last) {
		t.Fatal("deleted segment number reused; saved offsets became ambiguous")
	}
}
func TestLogStorageKeepsActiveAndUnrelatedFiles(t *testing.T) {
	withLogBudget(t, logFileConfig{MaxFileBytes: 32, MaxTotalBytes: 64, MaxFiles: 2})
	a, err := newDailyLogWriter("gateway", ".log")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newDailyLogWriter("debug", ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.Write(bytes.Repeat([]byte("a"), 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(bytes.Repeat([]byte("b"), 32)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "apikeys.json"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("next")); err != nil {
		t.Fatal(err)
	} // Old b is closed and eligible; a stays open.
	if _, err := os.Stat(a.path); err != nil {
		t.Fatal("active file removed")
	}
	if data, err := os.ReadFile(filepath.Join(logDir, "apikeys.json")); err != nil || string(data) != "keep" {
		t.Fatal("unrelated file changed")
	}
	if _, err := b.Write(bytes.Repeat([]byte("x"), 33)); err == nil {
		t.Fatal("oversized record bypassed file cap")
	}
}
func TestLogStorageRejectsLinksAndRetentionPreservesOtherSettings(t *testing.T) {
	withLogBudget(t, logFileConfig{MaxFileBytes: 64, MaxTotalBytes: 128, MaxFiles: 2})
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("keep"), 0600)
	name := "gateway-" + time.Now().Format("2006-01-02") + ".log"
	if err := os.Symlink(outside, filepath.Join(logDir, name)); err == nil {
		if w, err := newDailyLogWriter("gateway", ".log"); err == nil {
			_ = w.Close()
			t.Fatal("writer followed symbolic link")
		}
		data, _ := os.ReadFile(outside)
		if string(data) != "keep" {
			t.Fatal("link target changed")
		}
	}
}
func TestLogFixedSamplingLeavesErrorsIntact(t *testing.T) {
	old := logBudgetConfig.Load()
	limits, _ := normalizeLogBudget(logFileConfig{SampleFirst: 2, SampleEvery: 10})
	logBudgetConfig.Store(&limits)
	defer logBudgetConfig.Store(old)
	var output bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(oldWriter)
	routineLogs.mu.Lock()
	routineLogs.slots[routineSiteEligibility].second = 0
	routineLogs.mu.Unlock()
	for range 20 {
		logRoutineAt(routineSiteEligibility, 1234, "ordinary synthetic event")
	}
	for range 20 {
		log.Printf("ERROR synthetic must retain")
	}
	if strings.Count(output.String(), "ordinary synthetic event") != 3 || strings.Count(output.String(), "ERROR synthetic must retain") != 20 {
		t.Fatal("sampling dropped errors or did not bound routine events")
	}
}
func TestLogConcurrentWritersRespectSharedCaps(t *testing.T) {
	withLogBudget(t, logFileConfig{MaxFileBytes: 256, MaxTotalBytes: 1024, MaxFiles: 8})
	a, _ := newDailyLogWriter("gateway", ".log")
	b, _ := newDailyLogWriter("debug", ".jsonl")
	defer a.Close()
	defer b.Close()
	var group sync.WaitGroup
	for _, w := range []*dailyLogWriter{a, b} {
		group.Go(func() {
			for range 100 {
				if _, err := w.Write([]byte("synthetic-event\n")); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	group.Wait()
	_ = a.Close()
	_ = b.Close()
	var total int64
	for _, file := range listLogFiles() {
		if file.Size > 256 {
			t.Fatalf("shared writer exceeded single-file cap: %+v", file)
		}
		total += file.Size
	}
	if total > 1024 {
		t.Fatalf("shared log byte cap exceeded: %d", total)
	}
}
