//go:build darwin

package protocol

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// xucredVersion is XNU's XUCRED_VERSION, the only value the kernel fills a LOCAL_PEERCRED
// answer in with. golang.org/x/sys does not export the constant, so it is spelled out here
// against sys/ucred.h.
const xucredVersion = 0

// PeerUID is the uid of the process on the other end of c, as the kernel recorded it when
// the connection was made. Nothing the peer says takes part in the answer.
func PeerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("protocol: peer uid: %w", err)
	}
	var (
		cred    *unix.Xucred
		sockErr error
	)
	// The getsockopt has to happen inside Control, which is what keeps the fd from being
	// closed out from under it.
	if err := rc.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("protocol: peer uid: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("protocol: peer uid (LOCAL_PEERCRED): %w", sockErr)
	}
	return xucredUID(cred)
}

// xucredUID is the uid out of a LOCAL_PEERCRED answer. A struct the kernel filled in under
// another layout is not one to read a uid out of, so the version is checked rather than
// assumed.
func xucredUID(cred *unix.Xucred) (int, error) {
	if cred.Version != xucredVersion {
		return 0, fmt.Errorf("protocol: peer uid: LOCAL_PEERCRED version %d, want %d", cred.Version, xucredVersion)
	}
	return int(cred.Uid), nil
}
