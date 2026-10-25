package main

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type logFileConfig struct {
	RetentionDays int   `json:"retentionDays"`
	MaxFileBytes  int64 `json:"maxFileBytes"`
	MaxTotalBytes int64 `json:"maxTotalBytes"`
	MaxFiles      int   `json:"maxFiles"`
	SampleFirst   int   `json:"sampleFirst"`
	SampleEvery   int   `json:"sampleEvery"`
}
type logLimits struct {
	file, total         int64
	files, first, every int
}

var logBudgetConfig atomic.Pointer[logLimits]
var logBudgetRejected atomic.Uint64

func normalizeLogBudget(c logFileConfig) (logLimits, error) {
	l := logLimits{file: 8 << 20, total: 64 << 20, files: 16, first: 20, every: 100}
	if c.MaxFileBytes != 0 {
		l.file = c.MaxFileBytes
	}
	if c.MaxTotalBytes != 0 {
		l.total = c.MaxTotalBytes
	}
	if c.MaxFiles != 0 {
		l.files = c.MaxFiles
	}
	if c.SampleFirst != 0 {
		l.first = c.SampleFirst
	}
	if c.SampleEvery != 0 {
		l.every = c.SampleEvery
	}
	if l.file < 1 || l.file > 1<<30 || l.total < l.file || l.total > 1<<40 || l.files < 1 || l.files > 10000 || l.first < 1 || l.first > 100000 || l.every < 1 || l.every > 100000 {
		return l, errors.New("logs 大小、份数或采样配置无效；总量需不小于单文件上限")
	}
	return l, nil
}
func currentLogLimits() logLimits {
	if l := logBudgetConfig.Load(); l != nil {
		return *l
	}
	l, _ := normalizeLogBudget(logFileConfig{})
	return l
}

// A fixed allowlist, never one bucket per Trace ID, model or account.
type routineEvent uint8

const (
	routineSiteEligibility routineEvent = iota
	routineEventCount
)

var routineLogs struct {
	mu    sync.Mutex
	slots [routineEventCount]struct {
		second int64
		count  uint64
	}
	skipped uint64
}

func logRoutine(event routineEvent, format string, args ...any) {
	logRoutineAt(event, time.Now().Unix(), format, args...)
}
func logRoutineAt(event routineEvent, now int64, format string, args ...any) {
	l := currentLogLimits()
	routineLogs.mu.Lock()
	slot := &routineLogs.slots[event]
	if slot.second != now {
		*slot = struct {
			second int64
			count  uint64
		}{second: now}
	}
	slot.count++
	keep := slot.count <= uint64(l.first) || (slot.count-uint64(l.first))%uint64(l.every) == 0
	if !keep {
		routineLogs.skipped++
	}
	routineLogs.mu.Unlock()
	if keep {
		log.Printf(format, args...)
	}
}
