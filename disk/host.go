package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// evict drops what the node keeps only to save a download or a rebuild.
func evict(ctx context.Context, log *slog.Logger) {
	// crictl gives each request two seconds by default, and removing an image's
	// layers takes longer than that: the prune reported DeadlineExceeded while
	// containerd went on deleting.
	step(ctx, log, "images", "/usr/local/bin/crictl", "--timeout=5m", "rmi", "--prune")
	step(ctx, log, "journal", "/usr/bin/journalctl", "--vacuum-size=256M")
	log.Info("evict tmp", "bytes", sweep(host+"/tmp", day, plainFiles))
	for _, c := range caches(host) {
		log.Info("evict cache", "path", strings.TrimPrefix(c, host), "bytes", remove(c))
	}
}

// purge is evict and then what is regenerable but costs more to lose: a shorter
// journal, rotated logs, and push quarantines the forge left behind.
func purge(ctx context.Context, log *slog.Logger) {
	evict(ctx, log)
	step(ctx, log, "journal", "/usr/bin/journalctl", "--vacuum-size=64M")
	log.Info("purge logs", "bytes", sweep(host+"/var/log", 0, rotated))
	for _, q := range quarantines(host, 10*time.Minute) {
		log.Info("purge quarantine", "path", strings.TrimPrefix(q, host), "bytes", remove(q))
	}
}

func step(ctx context.Context, log *slog.Logger, what string, argv ...string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := run(ctx, argv...)
	if err != nil {
		log.Warn("evict "+what, "err", err, "out", out)
		return
	}
	log.Info("evict "+what, "out", last(out))
}

func last(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

const day = 24 * time.Hour

func plainFiles(path string, d fs.DirEntry) bool { return d.Type().IsRegular() }

// rotated is a log a rotation already closed: syslog.1, kern.log.2.gz.
func rotated(path string, d fs.DirEntry) bool {
	if !d.Type().IsRegular() {
		return false
	}
	n := d.Name()
	if strings.HasSuffix(n, ".gz") || strings.HasSuffix(n, ".xz") || strings.HasSuffix(n, ".old") {
		return true
	}
	ext := filepath.Ext(n)
	return len(ext) > 1 && strings.Trim(ext[1:], "0123456789") == ""
}

// sweep removes, under root and on root's own filesystem, every file that match
// accepts and that was last written longer than age ago, and returns the bytes.
func sweep(root string, age time.Duration, match func(string, fs.DirEntry) bool) uint64 {
	dev, ok := devOf(root)
	if !ok {
		return 0
	}
	cut := time.Now().Add(-age)
	var freed uint64
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if dd, ok := devOf(p); ok && dd != dev {
				return filepath.SkipDir // another filesystem mounted inside
			}
			return nil
		}
		if !match(p, d) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cut) {
			return nil
		}
		if os.Remove(p) == nil {
			freed += uint64(info.Size())
		}
		return nil
	})
	return freed
}

func devOf(p string) (uint64, bool) {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// caches are the build caches under the node's home directories that nothing has
// written to for a day. A cache says so itself: cargo's target directories carry
// CACHEDIR.TAG, as does any tool following that convention, and Go's is named
// go-build. Age is the NEWEST file inside, never the directory's own mtime, which
// changes only when a direct child is added or removed.
func caches(root string) []string {
	homes, _ := filepath.Glob(root + "/home/*")
	homes = append(homes, root+"/root")
	var out []string
	cut := time.Now().Add(-day)
	for _, h := range homes {
		base := strings.Count(h, string(os.PathSeparator))
		filepath.WalkDir(h, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if strings.Count(p, string(os.PathSeparator))-base > 4 {
				return filepath.SkipDir
			}
			_, tagged := os.Stat(filepath.Join(p, "CACHEDIR.TAG"))
			if tagged != nil && d.Name() != "go-build" {
				return nil
			}
			if newest(p).Before(cut) {
				out = append(out, p)
			}
			return filepath.SkipDir
		})
	}
	return out
}

// quarantines are the forge's push quarantines older than age: git writes a
// push's objects into objects/tmp_objdir-incoming-* and removes the directory
// when the push ends, but not when the connection is cut.
func quarantines(root string, age time.Duration) []string {
	dirs, _ := filepath.Glob(root + "/var/lib/rancher/k3s/storage/*hanzo-git-data*/git/data/repositories/*/*/objects/tmp_objdir-incoming-*")
	cut := time.Now().Add(-age)
	var out []string
	for _, d := range dirs {
		if newest(d).Before(cut) {
			out = append(out, d)
		}
	}
	return out
}

// newest is the latest modification time of anything under dir, dir included.
func newest(dir string) time.Time {
	var t time.Time
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(t) {
			t = info.ModTime()
		}
		return nil
	})
	return t
}

// remove deletes path and returns the bytes it held.
func remove(path string) uint64 {
	var n uint64
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += uint64(info.Size())
			}
		}
		return nil
	})
	if os.RemoveAll(path) != nil {
		return 0
	}
	return n
}
