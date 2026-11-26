//go:build linux

package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// errNetns marks a failure to enter a network namespace, as opposed to a failure
// of the connection made from inside it.
var errNetns = errors.New("cannot enter network namespace")

// dialInNetns opens a connection as if the caller ran inside the network
// namespace at nsPath (for example /host/proc/<pid>/ns/net).
//
// A network namespace belongs to a thread, so the dial runs in a goroutine that
// is locked to its thread, enters the namespace, dials, and returns to the
// original namespace. The socket keeps working after the thread has left the
// namespace. If the thread cannot be returned, it is never unlocked: the Go
// runtime then discards it when the goroutine ends, so no later goroutine can
// ever run in the wrong namespace.
func dialInNetns(ctx context.Context, nsPath, network, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		threadClean := false
		defer func() {
			if threadClean {
				runtime.UnlockOSThread()
			}
		}()

		self, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			threadClean = true
			done <- result{nil, errors.Join(errNetns, err)}
			return
		}
		defer self.Close()
		target, err := os.Open(nsPath)
		if err != nil {
			threadClean = true
			done <- result{nil, errors.Join(errNetns, err)}
			return
		}
		defer target.Close()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			threadClean = true // nothing changed yet
			done <- result{nil, errors.Join(errNetns, err)}
			return
		}
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err := unix.Setns(int(self.Fd()), unix.CLONE_NEWNET); err != nil {
			slog.Error("Could not return to the node network namespace; retiring this thread", "err", err)
		} else {
			threadClean = true
		}
		done <- result{conn, dialErr}
	}()
	r := <-done
	return r.conn, r.err
}
