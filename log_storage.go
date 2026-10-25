package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errLogCapacity = errors.New("日志存储预算不足，现有活跃日志保留")

type diskLogFile struct {
	size     int64
	modified time.Time
}

// Guarded by activeLogsMu, alongside open-file registration and deletion.
// Production has one log directory. Switching test roots rebuilds the cache.
var managedLogDisk struct {
	root    string
	files   map[string]diskLogFile
	bytes   int64
	scanned time.Time
}

func refreshLogDiskLocked(root string, force bool) error {
	if managedLogDisk.root == root && !force && time.Since(managedLogDisk.scanned) < time.Second {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	files := make(map[string]diskLogFile)
	var total int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !validManagedLogName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		if activeLogs[filepath.Join(root, entry.Name())] > 0 {
			// Windows directory enumeration may report a stale size until the
			// writing handle closes. Never reset our committed byte count to it.
			if managedLogDisk.root == root {
				if previous, ok := managedLogDisk.files[entry.Name()]; ok {
					size = max(size, previous.size)
				}
			} else if f, err := os.Open(filepath.Join(root, entry.Name())); err == nil {
				if current, err := f.Stat(); err == nil {
					size = max(size, current.Size())
				}
				_ = f.Close()
			}
		}
		files[entry.Name()] = diskLogFile{size, info.ModTime()}
		total += size
	}
	managedLogDisk.root, managedLogDisk.files, managedLogDisk.bytes, managedLogDisk.scanned = root, files, total, time.Now()
	return nil
}
func validManagedLogName(name string) bool {
	if !managedLogPattern.MatchString(name) {
		return false
	}
	_, err := time.Parse("2006-01-02", logDatePattern.FindString(name))
	return err == nil
}
func recordLogSizeLocked(path string, size int64) {
	name := filepath.Base(path)
	old := managedLogDisk.files[name]
	managedLogDisk.bytes += size - old.size
	managedLogDisk.files[name] = diskLogFile{size: size, modified: time.Now()}
}
func trimLogDiskLocked(extra int64) error {
	limits := currentLogLimits()
	if managedLogDisk.bytes+extra <= limits.total && len(managedLogDisk.files) <= limits.files {
		return nil
	}
	var names []string
	for name := range managedLogDisk.files {
		if activeLogs[filepath.Join(managedLogDisk.root, name)] == 0 {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := managedLogDisk.files[names[i]], managedLogDisk.files[names[j]]
		if a.modified.Equal(b.modified) {
			return names[i] < names[j]
		}
		return a.modified.Before(b.modified)
	})
	root, err := os.OpenRoot(managedLogDisk.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range names {
		if managedLogDisk.bytes+extra <= limits.total && len(managedLogDisk.files) <= limits.files {
			break
		}
		// Recheck the final path; neither links nor unrelated names are eligible.
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			managedLogDisk.bytes -= managedLogDisk.files[name].size
			delete(managedLogDisk.files, name)
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err = root.Remove(name); err != nil {
			return err
		}
		managedLogDisk.bytes -= managedLogDisk.files[name].size
		delete(managedLogDisk.files, name)
	}
	if managedLogDisk.bytes+extra > limits.total || len(managedLogDisk.files) > limits.files {
		return errLogCapacity
	}
	return nil
}
func (w *dailyLogWriter) openLogLocked(path, day string) error {
	rootPath := filepath.Dir(path)
	info, err := os.Lstat(rootPath)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("日志目录必须为普通目录")
	}
	if err := refreshLogDiskLocked(rootPath, true); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	if before, err := root.Lstat(name); err == nil && !before.Mode().IsRegular() {
		return errors.New("日志目标必须为普通文件")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return errors.New("日志目标打开失败或不是普通文件")
	}
	activeLogs[path]++
	if w.file != nil {
		_ = w.file.Close()
		activeLogs[w.path]--
		if activeLogs[w.path] == 0 {
			delete(activeLogs, w.path)
		}
	}
	w.file, w.path, w.day = f, path, day
	recordLogSizeLocked(path, st.Size())
	return nil
}
func (w *dailyLogWriter) rollSizeLocked() error {
	root := filepath.Dir(w.path)
	base := w.prefix + "-" + w.day
	latest := 0
	for name := range managedLogDisk.files {
		if strings.HasPrefix(name, base+".part-") && strings.HasSuffix(name, w.extension) {
			partText := strings.TrimSuffix(strings.TrimPrefix(name, base+".part-"), w.extension)
			if part, err := strconv.Atoi(partText); err == nil {
				latest = max(latest, part)
			}
		}
	}
	for part := latest + 1; part <= 999999; part++ {
		name := fmt.Sprintf("%s.part-%06d%s", base, part, w.extension)
		if _, exists := managedLogDisk.files[name]; exists {
			continue
		}
		// There is no rename: old file names/offsets remain valid for the UI.
		return w.openLogLocked(filepath.Join(root, name), w.day)
	}
	return errors.New("日志分段编号已耗尽")
}
func newestLogPathLocked(root, prefix, extension, day string) string {
	base := prefix + "-" + day
	name := base + extension
	for candidate := range managedLogDisk.files {
		if strings.HasPrefix(candidate, base+".part-") && strings.HasSuffix(candidate, extension) && candidate > name {
			name = candidate
		}
	}
	return filepath.Join(root, name)
}
