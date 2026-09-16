package turn

import "errors"

// ErrTerminalDurability reports missing durable terminal proof. Its text is safe
// for an untrusted client; the original cause is retained for the Server log.
var ErrTerminalDurability = errors.New("session unavailable after durability failure")

type durabilityError struct{ cause error }

func (e *durabilityError) Error() string        { return ErrTerminalDurability.Error() }
func (e *durabilityError) Unwrap() error        { return e.cause }
func (e *durabilityError) Is(target error) bool { return target == ErrTerminalDurability }

// DurabilityError also marks permission decision batch failures for the same
// Server-owned quarantine transition used by terminal Entries.
func DurabilityError(cause error) error {
	if errors.Is(cause, ErrTerminalDurability) {
		return cause
	}
	return &durabilityError{cause: cause}
}

// Cause is the failure behind a durability error, for the Server's own log. The error itself
// says only the fixed text, which is what a client is owed; an operator chasing a quarantined
// session needs the path, the errno and the state of the file under it.
func Cause(err error) error {
	if u := errors.Unwrap(err); u != nil {
		return u
	}
	return err
}
