package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// LogFile is the name of the entries file inside a session directory.
const LogFile = "entries.jsonl"

// Log is the append-only entries.jsonl of one session directory.
type Log struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// OpenLog creates dir when missing and opens its entries file for append.
func OpenLog(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: open log: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session: open log: %w", err)
	}
	return &Log{f: f, w: bufio.NewWriterSize(f, 64*1024)}, nil
}

// Append writes one entry as one line. The write is buffered; Sync makes it durable.
// Appending to a closed log is ErrClosed, not a panic: a session outlives its file whenever
// something else closed it first, and the holder deserves an error it can record.
func (l *Log) Append(e Entry) error {
	line, err := e.MarshalJSON()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return fmt.Errorf("session: append: %w", ErrClosed)
	}
	if _, err := l.w.Write(line); err != nil {
		return fmt.Errorf("session: append: %w", err)
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("session: append: %w", err)
	}
	return nil
}

// Sync flushes the buffer and fsyncs the file. A closed log is ErrClosed, for the same
// reason Append is.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return fmt.Errorf("session: sync: %w", ErrClosed)
	}
	return l.syncLocked()
}

func (l *Log) syncLocked() error {
	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("session: sync: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("session: sync: %w", err)
	}
	return nil
}

// Close syncs and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.syncLocked()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Truncated reports that the final line of a log was incomplete and was
// dropped. Line is 1-based. Callers that resume a session accept it; a
// process died mid-append.
type Truncated struct{ Line int }

func (t Truncated) Error() string {
	return fmt.Sprintf("session: entries.jsonl line %d is truncated", t.Line)
}

// ReadLog returns every complete entry in dir's log. A missing file yields no
// entries and no error. A truncated final line is dropped and reported as a
// Truncated error alongside the entries read before it. A final line with no
// trailing newline is reported as Truncated and dropped even when its JSON
// parses cleanly: the newline is the commit marker for a line, not the JSON
// syntax. Any other malformed line is an error with no entries.
func ReadLog(dir string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(dir, LogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read log: %w", err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReaderSize(f, 64*1024)
	var entries []Entry
	line := 0
	for {
		line++
		raw, rerr := r.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, fmt.Errorf("session: read log: %w", rerr)
		}
		complete := bytes.HasSuffix(raw, []byte("\n"))
		raw = bytes.TrimRight(raw, "\n")
		if len(raw) == 0 {
			if rerr == io.EOF {
				return entries, nil
			}
			continue
		}
		var e Entry
		if uerr := e.UnmarshalJSON(raw); uerr != nil || !complete {
			if rerr == io.EOF {
				return entries, Truncated{Line: line}
			}
			return nil, fmt.Errorf("session: read log line %d: %w", line, uerr)
		}
		entries = append(entries, e)
		if rerr == io.EOF {
			return entries, nil
		}
	}
}
