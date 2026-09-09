//go:build linux

package protocol

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID is the uid of the process on the other end of c, as the kernel recorded it when
// the connection was made. Nothing the peer says takes part in the answer.
func PeerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("protocol: peer uid: %w", err)
	}
	var (
		uid     int
		sockErr error
	)
	// The getsockopt has to happen inside Control, which is what keeps the fd from being
	// closed out from under it.
	if err := rc.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if sockErr == nil {
			uid = int(cred.Uid)
		}
	}); err != nil {
		return 0, fmt.Errorf("protocol: peer uid: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("protocol: peer uid (SO_PEERCRED): %w", sockErr)
	}
	return uid, nil
}
