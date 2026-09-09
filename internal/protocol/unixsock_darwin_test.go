//go:build darwin

package protocol

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestXucredUIDChecksTheVersion(t *testing.T) {
	uid, err := xucredUID(&unix.Xucred{Version: xucredVersion, Uid: 501})
	if err != nil {
		t.Fatalf("uid from a current xucred: %v", err)
	}
	if uid != 501 {
		t.Fatalf("uid = %d, want 501", uid)
	}
	if _, err := xucredUID(&unix.Xucred{Version: xucredVersion + 1, Uid: 501}); err == nil {
		t.Fatal("read a uid out of an xucred the kernel filled in under another layout")
	}
}
