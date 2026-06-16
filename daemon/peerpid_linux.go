//go:build linux

package daemon

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func peerPIDFromFD(fd int) (int, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, fmt.Errorf("getsockopt SO_PEERCRED: %w", err)
	}
	return int(cred.Pid), nil
}
