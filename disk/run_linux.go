package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// run executes a program of the NODE's, in the node's mount namespace: its
// crictl, journalctl, growpart and resize2fs, against its own /dev and /run. The
// image carries none of them.
//
// setns(CLONE_NEWNS) is refused to a thread that shares its filesystem
// attributes, and every Go thread shares them, so the call runs on a thread of
// its own that first unshares them. That thread is never unlocked: it leaves the
// node's namespace only by exiting, which the runtime does when a goroutine ends
// still locked. The child inherits the namespace from the thread that forks it.
func run(ctx context.Context, argv ...string) (string, error) {
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		if err := syscall.Unshare(syscall.CLONE_FS); err != nil {
			done <- result{err: fmt.Errorf("unshare: %w", err)}
			return
		}
		fd, err := syscall.Open("/proc/1/ns/mnt", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _, e := syscall.RawSyscall(sysSetns, uintptr(fd), syscall.CLONE_NEWNS, 0)
		syscall.Close(fd)
		if e != 0 {
			done <- result{err: fmt.Errorf("setns: %w", e)}
			return
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		cmd.Dir = "/"
		out, err := cmd.CombinedOutput()
		if len(out) > 4096 {
			out = out[len(out)-4096:]
		}
		done <- result{strings.TrimSpace(string(out)), err}
	}()
	r := <-done
	return r.out, r.err
}
