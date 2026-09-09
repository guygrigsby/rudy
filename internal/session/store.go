package session

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
)

// Store is the directory that holds one subdirectory per session.
type Store struct{ root string }

// OpenStore creates root when missing.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("session: open store: %w", err)
	}
	return &Store{root: root}, nil
}

// Root is the store directory.
func (st *Store) Root() string { return st.root }

// Dir is the directory of one session.
func (st *Store) Dir(id ulid.ULID) string { return filepath.Join(st.root, id.String()) }

// Summary is what List reports without replaying a log.
type Summary struct {
	ID        ulid.ULID
	OpenedAt  time.Time
	Workspace Workspace
	Model     ModelRef
	Forked    bool
	// ParentSessionID is the session whose tool call opened this one; empty means a root
	// session. Children are listed like any other session.
	ParentSessionID string
}

// List reads the first line of every session log, newest first. A fork's
// workspace and model come from the first session_opened up its parent chain.
func (st *Store) List() ([]Summary, error) {
	dirs, err := os.ReadDir(st.root)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	var out []Summary
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		id, err := ulid.ParseStrict(d.Name())
		if err != nil {
			continue
		}
		first, err := st.firstEntry(id)
		if err != nil {
			continue
		}
		sum := Summary{ID: id, OpenedAt: first.At}
		switch p := first.Payload.(type) {
		case SessionOpened:
			sum.Workspace, sum.Model = p.Workspace, p.Model
			sum.ParentSessionID = p.ParentSessionID
		case ForkPoint:
			sum.Forked = true
			root, err := st.rootOpened(p, 0)
			if err != nil {
				continue
			}
			sum.Workspace, sum.Model = root.Workspace, root.Model
		default:
			continue
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Compare(out[j].ID) > 0 })
	return out, nil
}

func (st *Store) firstEntry(id ulid.ULID) (Entry, error) {
	f, err := os.Open(filepath.Join(st.Dir(id), LogFile))
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReaderSize(f, 64*1024).ReadBytes('\n')
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := e.UnmarshalJSON(line[:len(line)-1]); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (st *Store) rootOpened(fp ForkPoint, depth int) (SessionOpened, error) {
	if depth > 64 {
		return SessionOpened{}, errors.New("session: fork chain deeper than 64")
	}
	first, err := st.firstEntry(fp.ParentSessionID)
	if err != nil {
		return SessionOpened{}, err
	}
	switch p := first.Payload.(type) {
	case SessionOpened:
		return p, nil
	case ForkPoint:
		return st.rootOpened(p, depth+1)
	}
	return SessionOpened{}, errors.New("session: parent log does not start with session_opened or fork_point")
}

// ErrLocked reports that another process holds the session.
var ErrLocked = errors.New("session: locked by another process")

// Lock takes an exclusive flock on <dir>/lock without blocking. The returned
// function releases it.
func (st *Store) Lock(id ulid.ULID) (unlock func(), err error) {
	dir := st.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("session: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lock is the name session.go's create calls; Lock is the exported surface
// named in interfaces.md.
func (st *Store) lock(id ulid.ULID) (func(), error) { return st.Lock(id) }
