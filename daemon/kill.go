package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Test seams: resolved at call time so tests can fake the socket peer and record
// signals instead of delivering them.
var (
	holderPID = func(c *Client) (int, error) { return c.peerPID() }
	killProc  = syscall.Kill
)

// KillHolder SIGKILLs the exact process holding the control socket. It refuses
// to signal init or itself, and treats an already-dead peer (ESRCH) as success.
func (c *Client) KillHolder() (int, error) {
	pid, err := holderPID(c)
	if err != nil {
		return 0, err
	}
	if pid <= 1 || pid == os.Getpid() {
		return 0, nil
	}
	if err := killProc(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return pid, fmt.Errorf("kill holder pid %d: %w", pid, err)
	}
	return pid, nil
}

// peerPID returns the pid of the process accepting on the control socket, read
// from the kernel's socket peer credentials — never by name, so it cannot target
// the wrong process.
func (c *Client) peerPID() (int, error) {
	conn, err := net.DialTimeout("unix", c.socket, 500*time.Millisecond)
	if err != nil {
		return 0, ErrDaemonUnavailable
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	var (
		pid   int
		opErr error
	)
	if err := raw.Control(func(fd uintptr) { pid, opErr = peerPIDFromFD(int(fd)) }); err != nil {
		return 0, fmt.Errorf("control fd: %w", err)
	}
	if opErr != nil {
		return 0, opErr
	}
	return pid, nil
}
