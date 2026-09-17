package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// opErr wraps a filesystem failure for a caller that is not the operator. The syscall's
// reason survives, the path does not: this error becomes a turn_failed message, a tool
// result and a protocol error, all of which reach every subscriber of the session, and the
// store's layout is the operator's business (rudy-wpa).
//
// The path is not lost, it is moved: this is the one place that logs it, once, beside the
// operation it failed in.
func opErr(op string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		slog.Error("session: "+op, "path", pe.Path, "syscall", pe.Op, "err", pe.Err)
		return fmt.Errorf("session: %s: %w", op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		slog.Error("session: "+op, "old", le.Old, "new", le.New, "syscall", le.Op, "err", le.Err)
		return fmt.Errorf("session: %s: %w", op, le.Err)
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return fmt.Errorf("session: %s: %w", op, se.Err)
	}
	return fmt.Errorf("session: %s: %w", op, err)
}
