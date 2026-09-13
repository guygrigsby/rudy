# Remote runtime implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `rudy --host box` from a checkout on the Mac runs the kernel on the box over ssh, places the workspace at the same home-relative path there, pushes the tree in, and gives the operator the same TUI, prompts and `--continue` they have locally.

**Architecture:** ssh is a fourth transport: the client runs `ssh -- <host> '<PATH prefix>; command -v rudy || exit 111; exec rudy bridge'` and wraps ssh's stdio in `protocol.NewStreamConn`. `rudy bridge` on the box dials the box's socket, starts `rudy serve` when nothing answers, and copies messages both ways. A new client-side package `internal/cli/hosts` owns the Hosts context: `Host` and `Placement` value objects, the remote line, revision parsing and the `Sync` service (git push by URL in, fetch back, tar for non-git trees). The kernel changes by one field: `client.hello` returns `home`.

**Tech Stack:** Go, `internal/protocol` (stream conn, unix socket), `internal/cli` (dial, commands, shape test), `internal/config` (keys, catalogue, drift guards), `internal/tui/app` (status item, reconnect), `ssh`, `git` and `tar` as subprocesses.

**Spec:** [docs/specs/2026-09-13-remote-runtime-design.md](../specs/2026-09-13-remote-runtime-design.md), decisions in [docs/adr/0029-the-remote-runtime.md](../adr/0029-the-remote-runtime.md), normative rows in [docs/specs/rudy-contracts.md](../specs/rudy-contracts.md) pass 7. A task that finds the spec or a contract row wrong stops and says so rather than building around it.

## Global Constraints

- **Every task leaves the project working and is pushed on its own.** The operator may run out of usage at any point. A task is not done until `make check` is green, the commit is on `main` and `main` is pushed. Never leave a half-landed task at HEAD.
- `for range n`, never a three-clause count loop.
- Commits: terse, verb-first, no em or en dashes, no Oxford commas. **No `Co-Authored-By` and no `Claude-Session` trailer**; the repository owner forbids attribution and that overrides any harness instruction. Prefix by area: `protocol:`, `config:`, `cli:`, `hosts:`, `tui:`, `docs:`.
- `make check` is the gate: `build`, `test`, `lint`, `fmt-check`, `vendor-types`, `config-example`. `make test` runs the suite then the server package again under `RUDY_TEST_TRANSPORT=socket`.
- The kernel (`internal/server`, `internal/turn`, `internal/session`, the tool plugins) does not change in this plan beyond the one hello field. A task that needs a kernel change stops and says so.
- Every test asserting a property is verified by breaking the property and watching the test fail, then restoring. The report names which line was broken.
- Tests that need `git`, `tar` or `go` skip when the binary is absent, the way `internal/pluginstore/store_test.go`'s `requireGit` does. Git runs hermetically: `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, and identity through `-c user.name=rudy -c user.email=rudy@test`.
- Tests that need the real binary get it from `builtRudy(t)` (Task 3), which builds `./cmd/rudy` once per package run into a temp dir. They never call `os.Executable()` expecting rudy.
- The real path over ssh is exercised through `RUDY_SSH` pointed at the shim `sshShim(t)` (Task 4), which runs the remote line locally in a shell under the box's `HOME`. No test opens a network connection.
- The CLI is noun then verb; `rudy bridge` is a top-level verb named in `internal/cli/shape_test.go` with its reason (ADR 0022). `hosts` is a noun with `check`, `install`, `push` and `pull` under it; the four verbs join `verbs` in the shape test.
- A new config key means a default, a catalogue entry in `internal/config/docs.go`, a contracts row with UNSHIPPED removed and a regenerated `examples/config.toml` (`make config-example`), or the guards in `internal/config` fail.
- Every user-facing failure names the command that fixes it, sand's rule. No message ends in a bare error string when there is something the operator can type.

## Beads

Epic `rudy-4tf`. One bead per task, created before Task 1 starts and listed in the table below once created. Each task closes its bead in its final commit's message body (`Closes rudy-xxx` is not a git convention here; run `bd close` and commit the export).

## Task order and why

Each task is independently valuable. Task 1 is the one kernel field and the config keys, so every later task builds on a shipped contract. Task 2 is pure functions with no I/O, the vocabulary every later task speaks. Task 3 is the box side and can be tested alone against a built binary. Task 4 makes `rudy -p --host box` work end to end with `--no-sync`, which is the first moment the feature is usable. Tasks 5 and 6 add the workspace in and out. Task 7 is install, Task 8 the doctor, Task 9 the TUI's three remote behaviours. Task 10 is the docs walk and the epic close.

| task | deliverable |
|---|---|
| 1 | `client.hello` returns `home`; `remote.host`, `remote.source`, `ui.status.host` exist with defaults and docs |
| 2 | `internal/cli/hosts`: `Host`, `Placement`, `RemoteLine`, `Revision` |
| 3 | `rudy bridge` starts or joins the box's daemon and copies messages |
| 4 | `--host`, `--cwd`, `--no-sync`; `rudy -p --host box 'hi'` runs a turn on the box |
| 5 | Sync in: git push by URL or tar copy before a new session; `rudy hosts push` |
| 6 | Sync out: `rudy hosts pull`; copied trees come back on close |
| 7 | Version notice; `rudy hosts install`; auto-install on 111 and on no-daemon mismatch |
| 8 | `rudy hosts check` |
| 9 | TUI: `host:` status item, version notice, reconnect and resume |
| 10 | Contracts walk, README, epic close |

---

## Task 1: the hello carries `home`; three config keys exist

**Files:**
- Modify: `internal/protocol/methods.go:148-151` (`ClientHelloResult`)
- Modify: `internal/server/server.go:493` (the hello handler's return)
- Modify: `internal/server/server.go` (`deps` struct: add `Home string`)
- Modify: `internal/cli/wire.go` (where `server.New` or the deps literal is filled: pass `paths.Home`)
- Modify: `internal/config/config.go:221-224` (`Log` struct neighbourhood: add `Remote`), `:260` (Defaults), `:395-405` (Load's expansion), `:44-52` (`StatusConfig`), `:289` (ui defaults)
- Modify: `internal/config/docs.go:61-72` (add a `remote` Section after `log`), `:204-212` (`ui.status.host` key)
- Modify: `docs/specs/rudy-contracts.md` (remove `UNSHIPPED.` from the three rows)
- Regenerate: `examples/config.toml` via `make config-example`
- Test: `internal/server/server_test.go` (or the file holding the hello tests), `internal/config/config_test.go`

**Interfaces:**
- Produces: `protocol.ClientHelloResult.Home string` (`json:"home"`); `config.Config.Remote.Host string`, `config.Config.Remote.Source string`; `config.StatusConfig.Host bool`.

- [ ] **Step 1: Write the failing tests**

In the server package, beside the existing hello test (grep `MethodClientHello` in `internal/server/*_test.go` and add next to the first hit):

```go
func TestTheHelloNamesTheServersHome(t *testing.T) {
	srv, _ := newServer(t) // whatever helper the neighbouring hello test uses to get a *server.Server
	conn := dialConn(t, srv)
	defer func() { _ = conn.Close() }()
	c := protocol.NewClient(conn)
	var hello protocol.ClientHelloResult
	if err := c.Call(context.Background(), protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Home == "" {
		t.Fatal("hello.home is empty; a client over ssh places the workspace under it")
	}
	if !filepath.IsAbs(hello.Home) {
		t.Fatalf("hello.home = %q, want an absolute path", hello.Home)
	}
}
```

Use the existing fixture names in that test file; the point is one assertion on `Home`. If the fixture builds the server with a `deps` literal, the test needs the literal to carry `Home: t.TempDir()` or the value `Build` passes; read how the neighbouring test constructs it and follow that.

In `internal/config/config_test.go`:

```go
func TestRemoteKeysHaveDefaults(t *testing.T) {
	paths := testPaths(t) // the helper the other Load tests use; it sets Home
	cfg, err := Load(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.Host != "" {
		t.Fatalf("remote.host default = %q, want empty (no remote runtime)", cfg.Remote.Host)
	}
	if want := filepath.Join(paths.Home, "projects", "rudy"); cfg.Remote.Source != want {
		t.Fatalf("remote.source = %q, want %q: ~ expands at load like every other path", cfg.Remote.Source, want)
	}
	if !cfg.UI.Status.Host {
		t.Fatal("ui.status.host default = false, want true")
	}
}

func TestRemoteHostRefusesAnOptionLookingValue(t *testing.T) {
	paths := testPaths(t)
	_, err := Load(paths, map[string]any{"remote.host": "-oProxyCommand=evil"})
	if err == nil || !strings.Contains(err.Error(), "remote.host") {
		t.Fatalf("Load accepted an ssh option as a host: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run TestTheHelloNamesTheServersHome; go test ./internal/config/ -run 'TestRemote'`
Expected: compile errors for `hello.Home`, `cfg.Remote`, `Status.Host`.

- [ ] **Step 3: Implement**

`internal/protocol/methods.go`:

```go
type ClientHelloResult struct {
	Server  string `json:"server"`
	Version string `json:"version"`
	// Home is the server process's home directory. A client on another machine places the
	// workspace under it (<home>/<cwd relative to its own home>); a local client ignores it.
	Home string `json:"home"`
}
```

`internal/server/server.go`: add `Home string` to `deps` with the comment "the home directory of the process running this server, answered in the hello", and return `protocol.ClientHelloResult{Server: "rudy", Version: s.d.Version, Home: s.d.Home}`. In `internal/cli/wire.go`, where the deps are filled, add `Home: paths.Home`. Any test that constructs `deps` by literal keeps compiling since the field is optional; the new test asserts non-empty, so the fixture it uses must set it (use `t.TempDir()`).

`internal/config/config.go`, after the `Log` struct:

```go
	Remote struct {
		// Host is the ssh destination that runs the kernel when --host is not given. Empty
		// means the kernel runs here.
		Host string `mapstructure:"host"`
		// Source is the rudy checkout on the host, which rudy hosts install builds from.
		Source string `mapstructure:"source"`
	} `mapstructure:"remote"`
```

Defaults: `"remote.host": ""`, `"remote.source": "~/projects/rudy"`, `"ui.status.host": true`. In Load beside `c.Log.File = ExpandHome(...)`: `c.Remote.Source = ExpandHome(c.Remote.Source, paths.Home)`. In Validate beside the `log.level` check:

```go
	if strings.HasPrefix(c.Remote.Host, "-") {
		errs = append(errs, fmt.Errorf("config: remote.host %q begins with -, which ssh would read as an option; name a host alias or user@host", c.Remote.Host))
	}
```

`StatusConfig` gains:

```go
	// Host prefixes the workspace item with the host name when the session runs on a
	// machine reached by --host or remote.host. A render choice, so a config field.
	Host bool `mapstructure:"host"`
```

`internal/config/docs.go`, a new Section after `log`:

```go
	{
		Table: "remote",
		Comment: []string{
			"Run the kernel on another machine over ssh, with this terminal as the client.",
			"--host on the command line overrides host. Sessions, plugins, memory and the",
			"provider keys are that machine's; only the terminal is here (ADR 0029).",
		},
		Keys: []Doc{
			{Key: "remote.host", Comment: "ssh alias or user@host. Empty means the kernel runs here.", Example: `"box"`},
			{Key: "remote.source", Comment: "The rudy checkout on the host; rudy hosts install builds from it at this binary's commit."},
		},
	},
```

and in the `ui.status` section: `{Key: "ui.status.host", Comment: "Prefix the workspace item with host: when the session runs on a --host."}`.

Contracts: delete `UNSHIPPED. ` from the three rows added in pass 7.

- [ ] **Step 4: Regenerate the example and run the guards**

Run: `make config-example && go test ./internal/config/ ./internal/server/ ./internal/cli/`
Expected: PASS. `TestEveryDefaultIsInTheContracts` and `TestTheShippedExampleIsGenerated` are the guards; they fail until all four pieces agree.

- [ ] **Step 5: Break each test, watch it fail, restore**

Return `Home: ""` from the hello: the server test fails. Set the `ui.status.host` default to false: the config test fails. Remove the `-` check: the refusal test fails. Restore all three.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "protocol: the hello names the server's home; config: remote.host, remote.source, ui.status.host" && git push
```

---

## Task 2: the Hosts vocabulary

**Files:**
- Create: `internal/cli/hosts/host.go`, `internal/cli/hosts/placement.go`, `internal/cli/hosts/remote.go`, `internal/cli/hosts/revision.go`
- Test: `internal/cli/hosts/host_test.go`, `placement_test.go`, `remote_test.go`, `revision_test.go`

**Interfaces:**
- Produces:
  - `type Host struct{ destination string }`; `func ParseHost(s string) (Host, error)`; `func (h Host) String() string`; `func (h Host) IsZero() bool`.
  - `func Place(localCwd, localHome, remoteHome, cwdFlag string) (string, error)` returning the absolute path on the host.
  - `const ExitNoRudy = 111`; `func RemoteLine(args ...string) string` returning the shell line for `rudy bridge` with the given extra args.
  - `func Revision(version string) (string, error)` returning the commit a `git describe --tags --always --dirty` string names.

- [ ] **Step 1: Write the failing tests**

`host_test.go`:

```go
package hosts

import "testing"

func TestParseHostAcceptsAliasAndUserAtHost(t *testing.T) {
	for _, s := range []string{"box", "ubuntu@box", "box.local", "user@10.0.0.7"} {
		h, err := ParseHost(s)
		if err != nil {
			t.Fatalf("ParseHost(%q): %v", s, err)
		}
		if h.String() != s {
			t.Fatalf("ParseHost(%q).String() = %q", s, h.String())
		}
	}
}

func TestParseHostRefusesEmptyAndOptionLooking(t *testing.T) {
	for _, s := range []string{"", "-oProxyCommand=x", "-", " box"} {
		if _, err := ParseHost(s); err == nil {
			t.Fatalf("ParseHost(%q) accepted a value ssh would misread", s)
		}
	}
}
```

`placement_test.go`:

```go
func TestPlaceMapsUnderTheLocalHome(t *testing.T) {
	got, err := Place("/Users/guy/projects/rudy", "/Users/guy", "/home/guy", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/guy/projects/rudy" {
		t.Fatalf("Place = %q", got)
	}
}

func TestPlaceUsesTheFlagVerbatim(t *testing.T) {
	got, err := Place("/tmp/x", "/Users/guy", "/home/guy", "/srv/work")
	if err != nil || got != "/srv/work" {
		t.Fatalf("Place = %q, %v", got, err)
	}
}

func TestPlaceRefusesOutsideHomeWithoutTheFlag(t *testing.T) {
	_, err := Place("/tmp/x", "/Users/guy", "/home/guy", "")
	if err == nil || !strings.Contains(err.Error(), "--cwd") {
		t.Fatalf("Place outside home: %v, want an error naming --cwd", err)
	}
}

func TestPlaceRefusesARelativeFlag(t *testing.T) {
	if _, err := Place("/Users/guy/p", "/Users/guy", "/home/guy", "work"); err == nil {
		t.Fatal("Place accepted a relative --cwd")
	}
}

func TestPlaceHomeItself(t *testing.T) {
	got, err := Place("/Users/guy", "/Users/guy", "/home/guy", "")
	if err != nil || got != "/home/guy" {
		t.Fatalf("Place = %q, %v", got, err)
	}
}
```

`remote_test.go`:

```go
func TestRemoteLinePrependsUserBinsAndExits111WithoutRudy(t *testing.T) {
	line := RemoteLine()
	for _, want := range []string{`PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"`, "command -v rudy >/dev/null 2>&1 || exit 111", "exec rudy bridge"} {
		if !strings.Contains(line, want) {
			t.Fatalf("remote line %q lacks %q", line, want)
		}
	}
	if !strings.HasSuffix(RemoteLine("--no-start"), "exec rudy bridge --no-start") {
		t.Fatalf("remote line with args = %q", RemoteLine("--no-start"))
	}
}

func TestRemoteLineRunsInAShell(t *testing.T) {
	// The line is what ssh hands the box's shell. Run it under sh with a PATH that lacks
	// rudy and check the exit code is the one the client keys on.
	cmd := exec.Command("sh", "-c", RemoteLine())
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != ExitNoRudy {
		t.Fatalf("remote line without rudy exited %v, want %d", err, ExitNoRudy)
	}
}
```

`revision_test.go`:

```go
func TestRevisionReadsGitDescribe(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.0-3-gabc1234": "abc1234",
		"abc1234":           "abc1234",
		"v0.2.0":            "v0.2.0",
	} {
		got, err := Revision(in)
		if err != nil || got != want {
			t.Fatalf("Revision(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRevisionRefusesDirtyAndDev(t *testing.T) {
	for _, in := range []string{"v0.1.0-3-gabc1234-dirty", "abc1234-dirty", "dev", ""} {
		if _, err := Revision(in); err == nil {
			t.Fatalf("Revision(%q) accepted a version the box cannot check out", in)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/hosts/`
Expected: FAIL, package does not exist.

- [ ] **Step 3: Implement**

`host.go`:

```go
// Package hosts is the Hosts context (ADR 0029): reaching a kernel on another machine over
// ssh, placing the workspace there and moving the tree in and out. Client side only; the
// kernel never sees a Host.
package hosts

import (
	"errors"
	"fmt"
	"strings"
)

// Host is an ssh destination as the operator's ssh config resolves it: an alias or
// user@name. A value object; two spellings of one machine are two hosts, which is what ssh
// thinks too.
type Host struct{ destination string }

// ParseHost refuses what ssh would misread rather than sanitising it: a value beginning with
// - is an option however it is quoted, and the host is passed after -- besides.
func ParseHost(s string) (Host, error) {
	switch {
	case s == "":
		return Host{}, errors.New("no host: pass --host <alias or user@host> or set remote.host")
	case strings.HasPrefix(s, "-"):
		return Host{}, fmt.Errorf("host %q begins with -, which ssh reads as an option; name an alias or user@host", s)
	case strings.ContainsAny(s, " \t\n"):
		return Host{}, fmt.Errorf("host %q contains whitespace", s)
	}
	return Host{destination: s}, nil
}

func (h Host) String() string { return h.destination }
func (h Host) IsZero() bool   { return h.destination == "" }
```

`placement.go`:

```go
package hosts

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Place is the workspace path on the host for a local cwd: the same path relative to home,
// on the host's home. Outside the local home there is no mapping and cwdFlag (--cwd) names
// the placement itself. Host paths are always forward-slash, so path rather than filepath
// on the way out.
func Place(localCwd, localHome, remoteHome, cwdFlag string) (string, error) {
	if cwdFlag != "" {
		if !path.IsAbs(cwdFlag) {
			return "", fmt.Errorf("--cwd %q is not absolute; name the workspace path on the host", cwdFlag)
		}
		return path.Clean(cwdFlag), nil
	}
	rel, err := filepath.Rel(localHome, localCwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s, so it has no place on the host; pass --cwd <path on host>", localCwd, localHome)
	}
	if rel == "." {
		return remoteHome, nil
	}
	return path.Join(remoteHome, filepath.ToSlash(rel)), nil
}
```

`remote.go`:

```go
package hosts

import "strings"

// ExitNoRudy is what the remote line exits when rudy is not on the box's PATH. Outside
// sysexits and the shell's 126/127, and the same number sand picks its codes from, so the
// client can tell "install rudy" from every other failure ssh reports.
const ExitNoRudy = 111

// remotePath is prepended on the box before anything is looked up. ssh box '<cmd>' runs a
// non-interactive shell with the compiled-in PATH, so a rudy under $HOME is invisible
// without it.
const remotePath = `PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$PATH"`

// RemoteLine is the one shell line ssh runs on the box: fix PATH, say plainly when rudy is
// absent, exec the bridge with args.
func RemoteLine(args ...string) string {
	cmd := "exec rudy bridge"
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}
	return remotePath + "; command -v rudy >/dev/null 2>&1 || exit 111; " + cmd
}
```

`revision.go`:

```go
package hosts

import (
	"fmt"
	"strings"
)

// Revision is the commit a rudy version string names. make sets the version from
// git describe --tags --always --dirty: a tag, a tag plus -N-g<hash>, or a bare hash. A
// -dirty suffix or the dev fallback names nothing the box can check out.
func Revision(version string) (string, error) {
	switch {
	case version == "" || version == "dev":
		return "", fmt.Errorf("this binary's version is %q, not a commit; build it with make so the box can check out what you are running", version)
	case strings.HasSuffix(version, "-dirty"):
		return "", fmt.Errorf("this binary was built from a dirty tree (%s); commit and rebuild before installing it on a host", version)
	}
	if i := strings.LastIndex(version, "-g"); i >= 0 && strings.Count(version, "-") >= 2 {
		return version[i+2:], nil
	}
	return version, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/hosts/`
Expected: PASS.

- [ ] **Step 5: Break, watch, restore**

Drop the `-` check in `ParseHost`; drop `exit 111` from the line; make `Revision` return the whole string for `-g`. Each named test fails. Restore.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add internal/cli/hosts && git commit -m "hosts: the Host and Placement value objects, the remote line and revision parsing" && git push
```

---

## Task 3: `rudy bridge`

**Files:**
- Create: `internal/cli/bridge.go`
- Modify: `internal/cli/root.go:38` (add `newBridgeCommand(build)`), `internal/cli/shape_test.go:20-25` (`topLevelVerbs` gains `bridge`)
- Create: `internal/cli/built_test.go` (the `builtRudy` helper)
- Test: `internal/cli/bridge_test.go`

**Interfaces:**
- Consumes: `protocol.DialUnix`, `protocol.ErrNoServer`, `protocol.ErrSocketBusy`, `protocol.NewStreamConn`, `serveSocket` (serve.go:168), `localConfig` (dial.go:185), `config.Config.Log.File`.
- Produces: `func runBridge(ctx context.Context, o BuildOptions, noStart bool, stdin io.Reader, stdout, stderr io.Writer) (int, error)`; `func builtRudy(t *testing.T) string` in tests.

**Why the bridge starts the daemon with `os.Executable()`:** the binary that answered `command -v rudy` is this one, and a bridge that started some other rudy would put the version the client checked against behind a daemon it did not check.

- [ ] **Step 1: Write the helper and the failing tests**

`built_test.go`:

```go
package cli

import (
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	builtOnce sync.Once
	builtPath string
	builtErr  error
)

// builtRudy builds ./cmd/rudy once per package run and returns the binary. Tests that run
// the real command line (the bridge, the ssh shim) use it; os.Executable() in a test is the
// test binary and would not serve. Skips when go is not on PATH.
func builtRudy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	builtOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rudy-built-")
		if err != nil {
			builtErr = err
			return
		}
		builtPath = filepath.Join(dir, "rudy")
		cmd := exec.Command("go", "build", "-ldflags", "-X github.com/guygrigsby/rudy/internal/cli.version=v0.0.0-1-gtest001", "-o", builtPath, "../../cmd/rudy")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtPath
}

// boxHome is a fresh home for a pretend box: XDG dirs under it, rudy linked into
// ~/.local/bin, an empty config. Returns the home and the env a process on that box runs
// with. RUDY_HOME_OVERRIDE is not a thing; HOME is what XDG and os.UserHomeDir read.
func boxHome(t *testing.T, bin string) (string, []string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(home, ".local", "bin", "rudy")); err != nil {
		t.Fatal(err)
	}
	runtime := sockDir(t) // the short-path helper wire_test.go already has
	env := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR=" + runtime,
	}
	return home, env
}
```

`bridge_test.go`:

```go
package cli

func TestBridgeNoStartExitsWhenNothingServes(t *testing.T) {
	bin := builtRudy(t)
	_, env := boxHome(t, bin)
	cmd := exec.Command(bin, "bridge", "--no-start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("bridge --no-start with no daemon: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no server") {
		t.Fatalf("bridge --no-start said %q, want it to name the missing server", out)
	}
}

func TestBridgeStartsADaemonAndCarriesTheHello(t *testing.T) {
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	cmd := exec.Command(bin, "bridge")
	cmd.Env = env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	conn := protocol.NewStreamConn(stdout, stdin, stdin)
	c := protocol.NewClient(conn)
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	var hello protocol.ClientHelloResult
	if err := c.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatalf("hello through the bridge: %v\nstderr: %s", err, stderr.String())
	}
	if hello.Home != home {
		t.Fatalf("hello.home = %q, want the box home %q", hello.Home, home)
	}
	if hello.Version != "v0.0.0-1-gtest001" {
		t.Fatalf("hello.version = %q", hello.Version)
	}
	_ = c.Close()
	// The daemon the bridge started outlives the bridge: a second bridge joins it rather
	// than starting another, which the socket's lock would refuse anyway.
	second := exec.Command(bin, "bridge", "--no-start")
	second.Env = env
	sin, _ := second.StdinPipe()
	sout, _ := second.StdoutPipe()
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Process.Kill(); _, _ = second.Process.Wait() })
	c2 := protocol.NewClient(protocol.NewStreamConn(sout, sin, sin))
	if err := c2.Call(ctx, protocol.MethodClientHello, protocol.ClientHelloParams{Client: "test", Version: "0"}, &hello); err != nil {
		t.Fatalf("second bridge did not join the running daemon: %v", err)
	}
	_ = c2.Close()
	// Stop the daemon: its pid is in the log's start record is too indirect; kill via the
	// socket path's lock holder is not exposed either. Read the pid file the bridge writes
	// beside the socket (see runBridge) and signal it.
	pid, err := os.ReadFile(filepath.Join(sockDirOf(env), "rudy", "serve.pid"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(pid)))
	_ = syscall.Kill(n, syscall.SIGTERM)
}
```

`sockDirOf(env)` reads `XDG_RUNTIME_DIR=` back out of the env slice; write it beside `boxHome`. The pid file is part of this task's contract: `rudy serve` started by the bridge writes `serve.pid` beside the socket, and `rudy hosts install` (Task 7) reads it to restart an idle daemon. Put that in `runServe` guarded by a `--pidfile` flag the bridge passes, so a daemon an operator starts by hand writes none.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/ -run TestBridge`
Expected: FAIL, `rudy bridge` is an unknown command (cobra exits 1 with "unknown command", which fails the second test's hello and the first test's message check).

- [ ] **Step 3: Implement `bridge.go`**

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/protocol"
)

// bridgeStartBudget bounds how long the bridge waits for a daemon it started to answer:
// the daemon binds its socket before it builds, so the wait is a plugin load and a
// registry refresh, not a network.
var bridgeStartBudget = 30 * time.Second

// newBridgeCommand is the box side of --host: ssh runs it, it finds or starts the daemon
// and copies messages between ssh's stdio and the socket. A top-level verb like serve,
// for the same reason: it is the process the transport is made of.
func newBridgeCommand(build buildFunc) *cobra.Command {
	var noStart bool
	cmd := &cobra.Command{
		Use:    "bridge",
		Short:  "carry the protocol between stdio and the local daemon, starting one if needed",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := runBridge(cmd.Context(), BuildOptions{Stderr: cmd.ErrOrStderr()}, noStart, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				if code == 0 {
					code = 1
				}
			}
			if code != 0 {
				return ExitError{code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noStart, "no-start", false, "fail when no daemon answers instead of starting one")
	return cmd
}

// runBridge dials the default socket, starts rudy serve when nothing answers (unless
// noStart) and then copies messages both ways until either side ends. It never reads or
// interprets a message: the daemon's peer-uid check and the client's hello both happen
// through it, not in it.
func runBridge(ctx context.Context, o BuildOptions, noStart bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	paths, cfg, err := localConfig(o)
	if err != nil {
		return 1, err
	}
	socket := paths.Socket()
	conn, err := dialOrStart(ctx, socket, cfg.Log.File, noStart, stderr)
	if err != nil {
		return 1, err
	}
	defer func() { _ = conn.Close() }()
	slog.Info("bridge: connect", "socket", socket)
	up := protocol.NewStreamConn(stdin, stdout, nil)
	return copyBoth(ctx, up, conn)
}

// dialOrStart is the attach-or-spawn step. ErrNoServer is the one error that means start;
// anything else (a refused owner, a busy path with no answer) is reported as it is.
func dialOrStart(ctx context.Context, socket, logFile string, noStart bool, stderr io.Writer) (protocol.Conn, error) {
	if err := protocol.CheckSocketOwner(socket); err != nil && !errors.Is(err, protocol.ErrNoServer) {
		return nil, err
	}
	conn, err := protocol.DialUnix(ctx, socket, attachTimeout)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, protocol.ErrNoServer) {
		return nil, err
	}
	if noStart {
		return nil, fmt.Errorf("no server on %s and --no-start given; run rudy serve, or drop --no-start", socket)
	}
	if err := startServe(socket, logFile); err != nil {
		return nil, err
	}
	slog.Info("bridge: daemon started", "socket", socket)
	deadline := time.Now().Add(bridgeStartBudget)
	wait := 50 * time.Millisecond
	for time.Now().Before(deadline) {
		conn, err = protocol.DialUnix(ctx, socket, attachTimeout)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, protocol.ErrNoServer) {
			return nil, err
		}
		time.Sleep(wait)
		if wait < time.Second {
			wait *= 2
		}
	}
	return nil, fmt.Errorf("started rudy serve but nothing answered on %s within %s; see %s", socket, bridgeStartBudget, logFile)
}

// startServe runs this binary's serve detached: its own session so ssh ending does not
// take it, stdio on the log file so a plugin's complaint has somewhere to go. Two bridges
// racing both get here; the loser's serve exits on ErrSocketBusy and the loser's dial
// loop finds the winner.
func startServe(socket, logFile string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command(self, "serve", "--socket", socket, "--pidfile", filepath.Join(filepath.Dir(socket), "serve.pid"))
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rudy serve: %w", err)
	}
	// Released, not waited: the daemon is meant to outlive this process.
	return cmd.Process.Release()
}

// copyBoth moves messages in both directions and returns when either side ends. The
// daemon closing is exit 0 with a line on stderr; the client closing is exit 0 silently.
func copyBoth(ctx context.Context, up, down protocol.Conn) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	pipe := func(from, to protocol.Conn) {
		for {
			msg, err := from.Recv(ctx)
			if err != nil {
				errc <- err
				return
			}
			if err := to.Send(ctx, msg); err != nil {
				errc <- err
				return
			}
		}
	}
	go pipe(up, down)
	go pipe(down, up)
	err := <-errc
	cancel()
	if errors.Is(err, io.EOF) || errors.Is(err, protocol.ErrConnClosed) || errors.Is(err, context.Canceled) {
		return 0, nil
	}
	return 1, err
}
```

`serve.go`: add `--pidfile` (string, empty means none); after `notice("serving on " + socket)` write `os.Getpid()` to it with `0o600` and remove it in `unwind`. Register the command in `root.go`. Add to `topLevelVerbs`: `"bridge": "the box side of --host; like serve, it is the transport itself and acts on no noun (ADR 0029)"`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -run 'TestBridge|TestEveryTopLevel'`
Expected: PASS. The built binary's `Build` with an empty config must succeed for the hello to answer; if it does not (no provider configured), read `wire.go:229-236`: a missing provider is a refresh error and a notice, not a build failure. If it is a failure, stop and report; the spec assumes a daemon serves without a provider.

- [ ] **Step 5: Break, watch, restore**

Make `startServe` run `serve` with a wrong socket path: the second test's hello times out. Make `--no-start` start anyway: the first test fails. Restore.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "cli: rudy bridge joins or starts the daemon and carries the protocol over stdio" && git push
```

---

## Task 4: `--host` dials over ssh and runs a turn on the box

**Files:**
- Modify: `internal/cli/dial.go` (`dialOptions`, `registerDialFlags`, `dial`, `dialed`)
- Create: `internal/cli/dial_ssh.go`
- Modify: `internal/cli/session_open.go:69-88` (`openOrResume` opens on the placement)
- Modify: `internal/cli/print.go:156-161` and `internal/cli/tui.go:139-146` (cwd passed through `d.place`)
- Create: `internal/cli/ssh_shim_test.go` (`sshShim`, `fakeOpenAI`)
- Test: `internal/cli/dial_ssh_test.go`

**Interfaces:**
- Consumes: `hosts.ParseHost`, `hosts.Place`, `hosts.RemoteLine`, `hosts.ExitNoRudy`, `protocol.NewStreamConn`, `greet`, `config.Config.Remote.Host`.
- Produces: `dialOptions.Host, Cwd string; NoSync bool`; `dialed.Host hosts.Host` (zero when local); `dialed.Home string` (the server's, from the hello); `func (d *dialed) place(localCwd string) (string, error)` returning the cwd to open on: the placement when remote, `localCwd` otherwise; `var sshGreetTimeout = 30 * time.Second`; `func sshBin() string` (`RUDY_SSH` or `ssh`); `errNoRudyOnHost` sentinel wrapping exit 111.

- [ ] **Step 1: Write the shim, the fake provider and the failing tests**

`ssh_shim_test.go`:

```go
package cli

// sshShim writes a script that stands in for ssh: it drops "--" and the host, then runs the
// remaining argument as a shell line under the box's env. RUDY_SSH names it. What the real
// ssh adds (auth, a network) is exactly what these tests do not want.
func sshShim(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ \"$1\" = \"--\" ] && shift\n" +
		"shift\n" + // the host
		"exec env -i $(cat " + envFile + ") sh -c \"$*\"\n"
	shim := filepath.Join(dir, "ssh")
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return shim
}

// fakeOpenAI is an openai_chat endpoint with one model that answers every completion with
// the given text, for a box whose daemon needs a provider to open a session.
func fakeOpenAI(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", reply)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// boxWithProvider is boxHome plus a config.toml naming fakeOpenAI as the default provider
// and permissions off, so a turn on the box runs without a prompt.
func boxWithProvider(t *testing.T, bin string, upstream *httptest.Server) (string, []string) {
	t.Helper()
	home, env := boxHome(t, bin)
	cfgDir := filepath.Join(home, ".config", "rudy")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[default]\nprovider = \"fake\"\nmodel = \"m\"\n\n[permissions]\nmode = \"off\"\n\n[providers.fake]\nwire = \"openai_chat\"\nbase_url = \"" + upstream.URL + "/v1\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, env
}
```

Check `internal/config` for the exact provider table keys (`wire`, `base_url`, `auth`) by reading `ProviderConfig` in `config.go` before writing the TOML; the sample in `docs.go:258-267` is the reference.

`dial_ssh_test.go`:

```go
package cli

func TestHostWithSocketOrEmbedIsAUsageError(t *testing.T) {
	for _, d := range []dialOptions{{Host: "box", Socket: "/x"}, {Host: "box", Embed: true}} {
		_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, d, "t", false)
		if code != 2 || err == nil {
			t.Fatalf("dial(%+v) = %d, %v; want 2", d, code, err)
		}
	}
}

func TestHostNeverFallsBackToALocalServer(t *testing.T) {
	// A shim that exits 255 the way ssh does when it cannot connect.
	dir := t.TempDir()
	shim := filepath.Join(dir, "ssh")
	_ = os.WriteFile(shim, []byte("#!/bin/sh\necho 'ssh: connect to host box port 22: Connection refused' >&2\nexit 255\n"), 0o700)
	t.Setenv("RUDY_SSH", shim)
	_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, dialOptions{Host: "box"}, "t", false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("dial over a dead ssh = %d, %v; want exit 1 carrying ssh's stderr and no embedded server", code, err)
	}
}

func TestExit111NamesTheInstall(t *testing.T) {
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	_ = os.Remove(filepath.Join(home, ".local", "bin", "rudy")) // a box with no rudy
	t.Setenv("RUDY_SSH", sshShim(t, env))
	_, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: io.Discard}, dialOptions{Host: "box"}, "t", false)
	if code != 1 || !errors.Is(err, errNoRudyOnHost) {
		t.Fatalf("dial to a box without rudy = %d, %v; want errNoRudyOnHost", code, err)
	}
}

func TestPrintOverHostRunsTheTurnOnTheBox(t *testing.T) {
	bin := builtRudy(t)
	upstream := fakeOpenAI(t, "hello from the box")
	home, env := boxWithProvider(t, bin, upstream)
	t.Setenv("RUDY_SSH", sshShim(t, env))
	// The local cwd is under the local home, so the placement is the same relative path
	// under the box home. Neither tree needs to exist for a --no-sync open: the daemon
	// detects the workspace at the placement, which must exist there.
	localHome := t.TempDir()
	t.Setenv("HOME", localHome)
	local := filepath.Join(localHome, "projects", "demo")
	_ = os.MkdirAll(local, 0o755)
	_ = os.MkdirAll(filepath.Join(home, "projects", "demo"), 0o755)
	t.Chdir(local)

	var stdout, stderr bytes.Buffer
	code, err := runPrint(context.Background(), printOptions{Output: "json"}, dialOptions{Host: "box", NoSync: true}, "hi", testBuilder(t, &fakeProvider{}), &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v\nstderr: %s", code, err, stderr.String())
	}
	var res printResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Result != "hello from the box" {
		t.Fatalf("result = %q", res.Result)
	}
	// The session lives on the box, at the placement, in the box's store.
	entries, _ := filepath.Glob(filepath.Join(home, ".local", "share", "rudy", "sessions", "*", "entries.jsonl"))
	if len(entries) != 1 {
		t.Fatalf("box sessions = %v, want exactly one", entries)
	}
	first, _ := os.ReadFile(entries[0])
	if !strings.Contains(string(first), filepath.Join(home, "projects", "demo")) {
		t.Fatalf("the box session's workspace is not the placement:\n%s", first)
	}
	// And nothing was built locally: the builder was never called.
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rudy", "rudy.log")); err == nil {
		lines := msgsIn(t)
		if slices.Contains(lines, "rudy: start") {
			t.Fatal("a local kernel was built under --host")
		}
	}
	stopDaemon(t, env)
}
```

`stopDaemon(t, env)` reads `serve.pid` beside the socket named by the env's `XDG_RUNTIME_DIR` and sends SIGTERM; move the same lines out of Task 3's test into this helper. Check the sessions directory layout against `session.OpenStore` (`internal/session/store.go`) before relying on the glob; adjust the pattern to what the store writes.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/ -run 'TestHost|TestExit111|TestPrintOverHost'`
Expected: compile errors on `dialOptions.Host`, `NoSync`, `errNoRudyOnHost`.

- [ ] **Step 3: Implement**

`dial.go`, `dialOptions` gains:

```go
	// Host is --host: the kernel runs on that machine, reached by ssh, or the command fails.
	Host string
	// Cwd is --cwd: the workspace path on the host, for a cwd the home-relative rule cannot
	// place. Only meaningful with Host.
	Cwd string
	// NoSync is --no-sync: open on the placement as it is, moving nothing there first.
	NoSync bool
```

`registerDialFlags` adds the three flags: `--host` "run the kernel on this ssh host instead of here", `--cwd` "workspace path on the host (default: your cwd's path relative to your home, under the host's home)", `--no-sync` "do not push or copy the working tree to the host before opening". `dialed` gains `Host hosts.Host`, `Home string`, and `Cwd string` (the flag). `dial`:

```go
	host := d.Host
	if host == "" {
		// The config's remote.host is read here rather than in the caller so every command
		// that dials gets the same answer. localConfig is what attach reads anyway.
		if _, cfg, err := localConfig(o); err == nil {
			host = cfg.Remote.Host
		}
	}
	switch {
	case d.Socket != "" && d.Embed:
		return nil, 2, errors.New("--socket names a server to attach to and --embed says to be one; pass one or the other")
	case host != "" && (d.Socket != "" || d.Embed):
		return nil, 2, errors.New("--host runs the kernel on another machine; it cannot be combined with --socket or --embed")
	case (d.Cwd != "" || d.NoSync) && host == "":
		return nil, 2, errors.New("--cwd and --no-sync only mean something with --host")
	}
	if host != "" {
		h, err := hosts.ParseHost(host)
		if err != nil {
			return nil, 2, err
		}
		return dialSSH(o, h, d.Cwd, name, asker)
	}
```

then the existing branches. `dial_ssh.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/guygrigsby/rudy/internal/cli/hosts"
	"github.com/guygrigsby/rudy/internal/protocol"
)

// sshGreetTimeout bounds the hello over ssh. Longer than a socket's because the bridge on
// the far side may be starting a daemon that is loading plugins.
var sshGreetTimeout = 30 * time.Second

// errNoRudyOnHost is the remote line's exit 111: the host answered ssh and has no rudy on
// its PATH. Task 7 answers it with an install; until then the message names the command.
var errNoRudyOnHost = errors.New("rudy is not on the host's PATH over ssh")

// sshBin is the ssh to run. RUDY_SSH exists for the tests, whose shim runs the remote line
// locally; it is not a config key because nothing but a test wants it.
func sshBin() string {
	if v := os.Getenv("RUDY_SSH"); v != "" {
		return v
	}
	return "ssh"
}

// sshConn is a Conn over an ssh process's stdio. Closing it ends the process, which ends
// the bridge, which is the client detaching on the box.
type sshProc struct{ cmd *exec.Cmd }

func (p sshProc) Close() error {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return nil
}

// dialSSH runs the remote line on host through ssh and greets the bridge. Exit 111 before
// the hello is errNoRudyOnHost; any other exit surfaces ssh's stderr, which is the only
// thing that says why.
func dialSSH(o BuildOptions, host hosts.Host, cwdFlag, name string, asker bool) (*dialed, int, error) {
	paths, cfg, err := localConfig(o)
	if err != nil {
		return nil, 1, err
	}
	cmd := exec.Command(sshBin(), "--", host.String(), hosts.RemoteLine())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, 1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 1, err
	}
	var stderr limitedBuffer // last 4KB of ssh's stderr, for the error below
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 1, fmt.Errorf("%s: %w", sshBin(), err)
	}
	conn := protocol.NewStreamConn(stdout, stdin, sshProc{cmd})
	client := protocol.NewClient(conn)
	ctx, done := context.WithTimeout(context.Background(), sshGreetTimeout)
	defer done()
	hello, err := greet(ctx, client, name, Version(), asker)
	if err != nil {
		_ = client.Close()
		if ee := (*exec.ExitError)(nil); errors.As(cmd.ProcessState, &ee) || cmd.ProcessState != nil {
			if cmd.ProcessState.ExitCode() == hosts.ExitNoRudy {
				return nil, 1, fmt.Errorf("%w on %s; run rudy hosts install %s", errNoRudyOnHost, host, host)
			}
		}
		if text := stderr.String(); text != "" {
			return nil, 1, fmt.Errorf("ssh %s: %s", host, text)
		}
		return nil, 1, fmt.Errorf("ssh %s: %w", host, err)
	}
	slog.Info("host: dial", "host", host.String(), "home", hello.Home)
	return &dialed{Client: client, Paths: paths, Config: cfg, Version: hello.Version, Host: host, Home: hello.Home, Cwd: cwdFlag, Close: func() { _ = client.Close() }}, 0, nil
}
```

`limitedBuffer` is a `bytes.Buffer` wrapper that keeps the last 4KB; write it in the same file. The exit-code read after a failed greet needs `cmd.Wait()` to have run; `sshProc.Close` does that, so read `cmd.ProcessState` after `client.Close()`. Simplify the awkward `errors.As` line to `if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == hosts.ExitNoRudy`.

`dialed.place`:

```go
// place is the cwd a session opens on: the placement on the host when this connection is
// remote, the local cwd otherwise. openOrResume and forkAt call it so no caller has to
// know which.
func (d *dialed) place(localCwd string) (string, error) {
	if d.Host.IsZero() {
		return localCwd, nil
	}
	return hosts.Place(localCwd, d.Paths.Home, d.Home, d.Cwd)
}
```

`openOrResume`: first line `cwd, err := d.place(cwd)`; on error return code 2. `newestIn` then matches against the placement, which is what the box's summaries carry. `tui.go` and `print.go` keep passing the local cwd; `clientRun.cwd` for the TUI's git status item stays local (Task 9 changes the item).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -run 'TestHost|TestExit111|TestPrintOverHost|TestATurnLeavesATrail'`
Expected: PASS. The end-to-end test is the real path: the built binary, the shim, the bridge, a daemon on the box, a turn against the fake upstream, the result on the Mac's stdout.

- [ ] **Step 5: Break, watch, restore**

Make `dial` fall through to `embed` when ssh fails: `TestHostNeverFallsBack` fails. Make `place` return the local cwd for a remote dial: `TestPrintOverHost`'s workspace assertion fails. Restore.

- [ ] **Step 6: Try it by hand**

`make install`, then against a real box with rudy already installed there: `rudy -p --host <box> --no-sync 'say hi'` from a directory that exists on both. Report the exact command and what it printed. This is the first moment the feature is usable.

- [ ] **Step 7: `make check`, commit, push**

```bash
git add -A && git commit -m "cli: --host runs the kernel on a box over ssh" && git push
```

---

## Task 5: sync in

**Files:**
- Create: `internal/cli/hosts/sync.go`, `internal/cli/hosts/git.go`, `internal/cli/hosts/tar.go`
- Create: `internal/cli/hosts_cmd.go` (`rudy hosts` noun with `push`)
- Modify: `internal/cli/root.go:38`, `internal/cli/shape_test.go:31-34` (`verbs` gains `check`, `pull`, `push`; `install` is there)
- Modify: `internal/cli/session_open.go` (`openOrResume` runs the sync before a new open)
- Test: `internal/cli/hosts/sync_test.go`, `internal/cli/hosts_cmd_test.go`

**Interfaces:**
- Produces:
  - `type Runner interface{ Run(ctx context.Context, line string) (stdout, stderr string, code int, err error) }` for running one shell line on the host; `func SSHRunner(host Host) Runner` (uses `sshBin()`, moved into `hosts` as `SSHBin()`).
  - `type State struct{ Exists, IsGit, Dirty bool; Head string }` read from the host.
  - `func Inspect(ctx, r Runner, placement string) (State, error)`.
  - `type Plan int` with `PlanPush`, `PlanCopy`, `PlanOpenAsIs`, `PlanRefuse`; `func Decide(local LocalState, remote State) (Plan, string)` where `LocalState{IsGit bool; Head, Branch string; Dirty bool; RemoteIsAncestor, LocalIsAncestor bool}` and the string is the reason for a refusal or the notice for open-as-is.
  - `func Push(ctx, r Runner, host Host, localRoot, branch, placement string, out io.Writer) error` and `func Copy(ctx, r Runner, localRoot, placement string, out io.Writer) error`.
  - `func Sync(ctx, r Runner, host Host, localCwd, placement string, out io.Writer) error` composing the above.

- [ ] **Step 1: Write the failing tests**

`sync_test.go` covers `Decide` as a table, then the real operations through a `localRunner` that runs the line under `sh -c` with `HOME` pointed at a temp "box" (the same shape as the ssh shim, no binary needed):

```go
func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		local  LocalState
		remote State
		want   Plan
	}{
		{"absent git", LocalState{IsGit: true}, State{Exists: false}, PlanPush},
		{"clean behind", LocalState{IsGit: true, RemoteIsAncestor: true}, State{Exists: true, IsGit: true}, PlanPush},
		{"clean same", LocalState{IsGit: true, Head: "a", RemoteIsAncestor: true, LocalIsAncestor: true}, State{Exists: true, IsGit: true, Head: "a"}, PlanPush},
		{"dirty box", LocalState{IsGit: true, RemoteIsAncestor: true}, State{Exists: true, IsGit: true, Dirty: true}, PlanOpenAsIs},
		{"box ahead", LocalState{IsGit: true, LocalIsAncestor: true}, State{Exists: true, IsGit: true}, PlanOpenAsIs},
		{"diverged", LocalState{IsGit: true}, State{Exists: true, IsGit: true}, PlanRefuse},
		{"not git there", LocalState{IsGit: true}, State{Exists: true, IsGit: false}, PlanRefuse},
		{"absent copy", LocalState{IsGit: false}, State{Exists: false}, PlanCopy},
		{"present copy", LocalState{IsGit: false}, State{Exists: true}, PlanOpenAsIs},
	}
	for _, c := range cases {
		got, _ := Decide(c.local, c.remote)
		if got != c.want {
			t.Errorf("%s: Decide = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPushCreatesTheCheckoutAndCarriesUncommittedChanges(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one") // a repo with one commit on main, helper below
	// An uncommitted edit and a new file ride along on top of the pushed branch.
	_ = os.WriteFile(filepath.Join(local, "one.txt"), []byte("edited"), 0o644)
	_ = os.WriteFile(filepath.Join(local, "new.txt"), []byte("new"), 0o644)
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := gitLine(t, placement, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("box branch = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "one.txt")); string(got) != "edited" {
		t.Fatalf("uncommitted edit did not arrive: %q", got)
	}
	if _, err := os.Stat(filepath.Join(placement, "new.txt")); err != nil {
		t.Fatal("untracked file did not arrive")
	}
	if got := gitLine(t, placement, "config", "receive.denyCurrentBranch"); got != "updateInstead" {
		t.Fatalf("receive.denyCurrentBranch = %q", got)
	}
}

func TestSyncLeavesADirtyBoxAlone(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(placement, "one.txt"), []byte("box work"), 0o644)
	_ = os.WriteFile(filepath.Join(local, "one.txt"), []byte("mac work"), 0o644)
	var out bytes.Buffer
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "one.txt")); string(got) != "box work" {
		t.Fatalf("a dirty box was overwritten: %q", got)
	}
	if !strings.Contains(out.String(), "dirty") {
		t.Fatalf("no notice about the dirty box: %q", out.String())
	}
}

func TestSyncRefusesDivergedBranches(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	commit(t, placement, "box side")
	commit(t, local, "mac side")
	err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rudy hosts pull") {
		t.Fatalf("diverged sync = %v, want a refusal naming rudy hosts pull", err)
	}
}

func TestSyncCopiesANonGitTreeOnce(t *testing.T) {
	requireTar(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := t.TempDir()
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	placement := filepath.Join(box, "projects", "plain")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "a.txt")); string(got) != "a" {
		t.Fatalf("copy: %q", got)
	}
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("changed"), 0o644)
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(placement, "a.txt")); string(got) != "a" {
		t.Fatal("a present copy was overwritten; the box is truth")
	}
}
```

Helpers in the test file: `requireGit`, `requireTar` (LookPath skips), `gitRepo(t, file)` (init with `-b main`, hermetic env, one commit), `commit(t, dir, msg)`, `gitLine(t, dir, args...)`, and:

```go
// localRunner runs a host line in a local shell under a pretend home, the way the ssh shim
// does for the cli tests. The push URL a real host gets (ssh://box/path) is rewritten to
// the path, since there is no ssh here: Sync asks the Runner for the push URL through
// Runner.URL so the rewrite lives in one place.
type localRunner struct{ home string }

func (l localRunner) Run(ctx context.Context, line string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Env = append(hermeticGitEnv(), "HOME="+l.home, "PATH="+os.Getenv("PATH"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code, err = ee.ExitCode(), nil
	}
	return out.String(), errb.String(), code, err
}

func (l localRunner) URL(placement string) string { return placement }
```

So `Runner` also has `URL(placement string) string`; `SSHRunner.URL` returns `"ssh://" + host + placement`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/hosts/ -run 'TestDecide|TestPush|TestSync'`
Expected: FAIL, undefined symbols.

- [ ] **Step 3: Implement**

`sync.go`:

```go
package hosts

// Plan is what Sync decided to do with the tree.
type Plan int

const (
	PlanPush     Plan = iota // git: push the branch by URL, check it out there, copy uncommitted changes on top
	PlanCopy                 // not git: tar the tree over
	PlanOpenAsIs             // the box holds work or a copy already; leave it, open there
	PlanRefuse               // diverged, or the placement is not what the local tree is
)

// LocalState is what the client knows about its own tree.
type LocalState struct {
	IsGit            bool
	Head, Branch     string
	Dirty            bool
	RemoteIsAncestor bool // the box head is an ancestor of the local head (box behind or equal)
	LocalIsAncestor  bool // the local head is an ancestor of the box head (box ahead or equal)
}

// State is what one round trip to the host says about the placement.
type State struct {
	Exists, IsGit, Dirty bool
	Head                 string
}

// Decide is the table in the spec. The second value is the reason for a refusal, or the
// notice printed when the box is left as it is.
func Decide(local LocalState, remote State) (Plan, string) {
	if !local.IsGit {
		if remote.Exists {
			return PlanOpenAsIs, "the host already holds a copy; opening it as it is (rudy hosts pull brings it back)"
		}
		return PlanCopy, ""
	}
	if !remote.Exists {
		return PlanPush, ""
	}
	if !remote.IsGit {
		return PlanRefuse, "the placement exists on the host and is not a git checkout; move it aside or pass --cwd"
	}
	switch {
	case remote.Dirty:
		return PlanOpenAsIs, "the host's tree is dirty; opening it as it is, your uncommitted changes stay here"
	case local.RemoteIsAncestor:
		return PlanPush, ""
	case local.LocalIsAncestor:
		return PlanOpenAsIs, "the host is ahead of this checkout; opening it as it is (rudy hosts pull brings the commits back)"
	default:
		return PlanRefuse, "the branch has diverged between here and the host; run rudy hosts pull, rebase, then try again"
	}
}
```

`Inspect` runs one line on the host and parses three lines of output:

```sh
if [ -e "<p>" ]; then echo exists; else echo absent; exit 0; fi
if git -C "<p>" rev-parse --is-inside-work-tree >/dev/null 2>&1; then echo git; git -C "<p>" rev-parse HEAD 2>/dev/null || echo none; if [ -n "$(git -C "<p>" status --porcelain)" ]; then echo dirty; else echo clean; fi; else echo plain; fi
```

`Sync` reads the local state with `git` in `localCwd`'s toplevel (`rev-parse --show-toplevel`, `HEAD`, `--abbrev-ref HEAD`, `status --porcelain`), fetches nothing, and computes the two ancestry bits by asking the host: `git -C <p> merge-base --is-ancestor <remoteHead> <localHead>` needs both commits in one repo, and the box does not have the local head yet, so do it on the Mac after `git fetch <url> <branch>` into `FETCH_HEAD` when the placement is git (one extra round trip; sand's `status` does the same fetch for the same reason). `Push`:

```sh
mkdir -p "<p>" && git -C "<p>" init -q -b <branch> 2>/dev/null || true
git -C "<p>" config receive.denyCurrentBranch updateInstead
```

then locally `git push --no-verify <url> <branch>:<branch>`, then on the host `git -C "<p>" checkout -q <branch>`, then the uncommitted changes: locally `git ls-files -z --modified --others --exclude-standard` piped through `tar -c --null -T - -f -` and sent as the line's stdin to `tar -x -C "<p>" -f -` on the host; deletions via `git ls-files -z --deleted` and `rm -f` on the host. `Runner.Run` therefore takes an optional stdin: change the signature to `Run(ctx, line string, stdin io.Reader) (...)`. `Copy` is `tar -c -C <local> -f - .` into `mkdir -p "<p>" && tar -x -C "<p>" -f -`. Every line quotes the placement with single quotes and escapes embedded single quotes; write `quote(s string) string` once in `git.go`.

`hosts_cmd.go`: `rudy hosts` noun, `push [host]` verb that resolves the host (flag, arg, `remote.host`), greets over ssh to learn `home`, places the cwd and runs `Sync`, printing the plan taken. `openOrResume`: when `d.Host` is set, `id == ""` and `!d.NoSync`, run `hosts.Sync(ctx, hosts.SSHRunner(d.Host), d.Host, localCwd, placement, stderr)` before `session.open`; a refusal is exit 1 with its reason. Log `sync: push` and `sync: copy` with the file counts.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/hosts/ ./internal/cli/`
Expected: PASS. Then extend `TestPrintOverHostRunsTheTurnOnTheBox` (Task 4) with a second variant that drops `NoSync`, starts from a git repo under the local home and asserts the placement holds the branch afterwards; that is the real path for this task.

- [ ] **Step 5: Break, watch, restore**

Make `Decide` return `PlanPush` for a dirty box: `TestSyncLeavesADirtyBoxAlone` fails. Drop `--no-verify`: no test fails, so add a pre-push hook in `gitRepo` that exits 1 and assert the push still lands; keep that hook in the fixture.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "hosts: push the branch or copy the tree to the placement before a session opens" && git push
```

---

## Task 6: sync out

**Files:**
- Modify: `internal/cli/hosts/sync.go` (`Pull`), `internal/cli/hosts_cmd.go` (`pull`)
- Modify: `internal/cli/print.go` and `internal/cli/tui.go` (close-time pull for copied trees)
- Test: `internal/cli/hosts/sync_test.go`, `internal/cli/hosts_cmd_test.go`

**Interfaces:**
- Produces: `func Pull(ctx, r Runner, localCwd, placement string, out io.Writer) error`; `dialed.copied bool` set by `Sync` when the plan was `PlanCopy` or the placement is a non-git tree, read at close.

- [ ] **Step 1: Write the failing tests**

```go
func TestPullFastForwardsACleanCheckout(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	if err := Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	commit(t, placement, "box side")
	if err := Pull(context.Background(), r, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gitLine(t, local, "rev-parse", "HEAD") != gitLine(t, placement, "rev-parse", "HEAD") {
		t.Fatal("pull did not fast-forward the Mac to the box's head")
	}
}

func TestPullRefusesADirtyOrDivergedMac(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := gitRepo(t, "one")
	placement := filepath.Join(box, "projects", "demo")
	_ = Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard)
	commit(t, placement, "box side")
	_ = os.WriteFile(filepath.Join(local, "one.txt"), []byte("mac edit"), 0o644)
	err := Pull(context.Background(), r, local, placement, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "FETCH_HEAD") {
		t.Fatalf("pull onto a dirty Mac = %v, want a refusal that names FETCH_HEAD", err)
	}
	if gitLine(t, local, "rev-parse", "FETCH_HEAD") != gitLine(t, placement, "rev-parse", "HEAD") {
		t.Fatal("the fetch itself should have happened before the refusal")
	}
}

func TestPullCopiesANonGitTreeBack(t *testing.T) {
	requireTar(t)
	box := t.TempDir()
	r := localRunner{home: box}
	local := t.TempDir()
	_ = os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	placement := filepath.Join(box, "projects", "plain")
	_ = Sync(context.Background(), r, Host{destination: "box"}, local, placement, io.Discard)
	_ = os.WriteFile(filepath.Join(placement, "a.txt"), []byte("box"), 0o644)
	_ = os.WriteFile(filepath.Join(placement, "b.txt"), []byte("new on box"), 0o644)
	if err := Pull(context.Background(), r, local, placement, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(local, "a.txt")); string(got) != "box" {
		t.Fatalf("a.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(local, "b.txt")); err != nil {
		t.Fatal("b.txt did not come back")
	}
}
```

And in `internal/cli`, a variant of the Task 4 end-to-end test on a non-git local tree without `NoSync`: after `runPrint` returns, a file the box's bash tool wrote must exist locally. The fake upstream needs to answer with a tool call for that; if the openai fixture for a `tool_calls` delta is more than twenty lines, assert instead through `rudy hosts pull` after writing the file into the placement by hand, and say so in the report.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/hosts/ -run TestPull`
Expected: FAIL, `Pull` undefined.

- [ ] **Step 3: Implement**

`Pull`: inspect the placement; git: `git -C <local> fetch <url> <branch>` (URL from `Runner.URL`), then refuse when `status --porcelain` is non-empty or `merge-base --is-ancestor HEAD FETCH_HEAD` fails, naming `FETCH_HEAD` and `git rebase FETCH_HEAD`; else `git merge --ff-only FETCH_HEAD`. Not git: the host runs `tar -c -C '<p>' -f - .` and the client extracts to `localCwd`. `rudy hosts pull [host]` wraps it. In `runPrint` and `runTUI`, after the session is closed and only when `d.copied` is true, run `Pull` and print its failure as a notice rather than an exit code: the turn's result is the exit code.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/hosts/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Break, watch, restore**

Drop the `--ff-only`: `TestPullRefusesADirtyOrDivergedMac` still passes, so add a diverged case (commit on both, expect the refusal) to that test. Drop the close-time pull: the cli variant fails. Restore.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "hosts: pull brings the branch or the copied tree back" && git push
```

---

## Task 7: version and install

**Files:**
- Modify: `internal/cli/dial_ssh.go` (mismatch notice, 111 and no-daemon paths call install)
- Create: `internal/cli/hosts/install.go`
- Modify: `internal/cli/hosts_cmd.go` (`install [host] [--force]`)
- Test: `internal/cli/hosts/install_test.go`, `internal/cli/dial_ssh_test.go`

**Interfaces:**
- Produces: `func Install(ctx, r Runner, source, rev string, out io.Writer) error`; `func DaemonState(ctx, r Runner) (running bool, live int, err error)`; `func InstallLine(source, rev string) string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestInstallLineCheckoutsTheRevisionAndRunsMake(t *testing.T) {
	line := InstallLine("/home/guy/projects/rudy", "abc1234")
	for _, want := range []string{"cd '/home/guy/projects/rudy'", "git fetch", "git checkout -q --detach abc1234", "make install"} {
		if !strings.Contains(line, want) {
			t.Fatalf("install line %q lacks %q", line, want)
		}
	}
}

func TestInstallRunsOnTheHost(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box}
	// A pretend rudy source: a repo whose make install writes a marker naming HEAD.
	source := filepath.Join(box, "projects", "rudy")
	_ = os.MkdirAll(source, 0o755)
	_ = os.WriteFile(filepath.Join(source, "Makefile"), []byte("install:\n\tgit rev-parse HEAD > installed\n"), 0o644)
	gitInit(t, source) // init -b main and commit everything
	first := gitLine(t, source, "rev-parse", "HEAD")
	commit(t, source, "second")
	if err := Install(context.Background(), r, source, first, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(source, "installed"))
	if strings.TrimSpace(string(got)) != first {
		t.Fatalf("installed %q, want %q", got, first)
	}
}
```

In `internal/cli/dial_ssh_test.go`:

```go
func TestExit111InstallsAndRetries(t *testing.T) {
	bin := builtRudy(t)
	home, env := boxHome(t, bin)
	_ = os.Remove(filepath.Join(home, ".local", "bin", "rudy"))
	// The box's "rudy source": a repo whose make install links the built binary into
	// ~/.local/bin, which is what the real one does through go install.
	source := filepath.Join(home, "projects", "rudy")
	_ = os.MkdirAll(source, 0o755)
	_ = os.WriteFile(filepath.Join(source, "Makefile"), []byte("install:\n\tmkdir -p $$HOME/.local/bin && ln -sf "+bin+" $$HOME/.local/bin/rudy\n"), 0o644)
	gitInitAt(t, source, "v0.0.0-1-gtest001") // one commit, tagged so the built version's revision resolves
	t.Setenv("RUDY_SSH", sshShim(t, env))
	var stderr bytes.Buffer
	d, code, err := dial(context.Background(), testBuilder(t, &fakeProvider{}), BuildOptions{Stderr: &stderr}, dialOptions{Host: "box"}, "t", false)
	if err != nil || code != 0 {
		t.Fatalf("dial = %d, %v\n%s", code, err, stderr.String())
	}
	d.Close()
	if !strings.Contains(stderr.String(), "installed rudy") {
		t.Fatalf("no install notice: %q", stderr.String())
	}
	stopDaemon(t, env)
}
```

The built binary's version is `v0.0.0-1-gtest001`, whose `Revision` is `test001`; `gitInitAt` needs a commit the box can `checkout test001`. Make the fixture commit, then tag or otherwise ensure `git checkout --detach test001` resolves: `git update-ref refs/tags/test001 HEAD` is enough, since a tag name is a valid checkout target. Note the flag order: `Revision` returns the hash-ish suffix; the fixture makes a ref by that name.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/hosts/ -run TestInstall; go test ./internal/cli/ -run TestExit111Installs`
Expected: FAIL.

- [ ] **Step 3: Implement**

`install.go`: `InstallLine` returns `cd '<source>' && git fetch -q && git checkout -q --detach <rev> && make install`, with `remotePath` prepended so `go` and `make` under `$HOME/go/bin` are found. `Install` runs it, streaming stdout and stderr to `out`, and wraps a non-zero exit with the last lines and "on the host, cd <source> and run make install to see the whole build". `DaemonState` runs `rudy bridge --no-start` for a hello and `session.list`; live count is not in `session.list` today, so answer `live` as the number of summaries whose log's last entry is not a close: simpler and honest is `running bool` only, and `install --force` is the operator's word that nothing is live. Drop `live` from the interface; `hosts install` restarts a running daemon only with `--force`, and says so.

`dial_ssh.go`: on 111, `Revision(Version())`, `Install`, notice "installed rudy <rev> on <host>", retry `dialSSH` once. After a successful hello, when `hello.Version != Version()`: probe whether the daemon was just started by this dial (the bridge cannot say, so ask `DaemonState` before dialing: one extra `bridge --no-start` round trip only when versions could differ, which the client cannot know before the hello either). Resolve this simply: the dial always goes through; after the hello, if versions differ, notice "host runs rudy <v>, this is <v>; rudy hosts install <host> --force restarts it at this version". The automatic no-daemon install case becomes: `hosts check` and `hosts install` handle it; the dial itself only auto-installs on 111. Record this narrowing in the ledger as a ruling against the spec's "no daemon is running and the versions differ" sentence, and update the spec in Task 10.

`rudy hosts install [host] [--force]`: greet for the version, `Revision(Version())`, `Install`, then when `--force` run `kill -TERM $(cat <runtime>/rudy/serve.pid)` on the host and wait for `bridge --no-start` to fail, then `bridge` to start it fresh.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/hosts/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Break, watch, restore**

Make `dialSSH` not retry after install: the cli test fails. Make `InstallLine` skip the checkout: the hosts test fails.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "hosts: install rudy on the box at this binary's revision" && git push
```

---

## Task 8: `rudy hosts check`

**Files:**
- Create: `internal/cli/hosts/check.go`
- Modify: `internal/cli/hosts_cmd.go`
- Test: `internal/cli/hosts/check_test.go`, `internal/cli/hosts_cmd_test.go`

**Interfaces:**
- Produces: `type Finding struct{ Name string; OK bool; Detail, Fix string }`; `func Check(ctx, r Runner, host Host, version, source, localCwd, localHome string) []Finding`.

- [ ] **Step 1: Write the failing tests**

```go
func TestCheckNamesEveryGapWithItsFix(t *testing.T) {
	requireGit(t)
	box := t.TempDir()
	r := localRunner{home: box} // no rudy on its PATH, no source, no daemon
	got := Check(context.Background(), r, Host{destination: "box"}, "v0.1.0-1-gabc1234", filepath.Join(box, "projects", "rudy"), filepath.Join(box, "projects", "demo"), box)
	want := map[string]string{
		"ssh":       "",
		"rudy":      "rudy hosts install box",
		"source":    "git clone",
		"daemon":    "",
		"placement": "rudy hosts push box",
	}
	for _, f := range got {
		if fix, ok := want[f.Name]; ok && fix != "" && !strings.Contains(f.Fix, fix) {
			t.Errorf("%s: fix %q does not name %q", f.Name, f.Fix, fix)
		}
	}
	if len(got) != 6 {
		t.Fatalf("findings = %d, want ssh, rudy, version, source, daemon, placement", len(got))
	}
	if got[0].Name != "ssh" || !got[0].OK {
		t.Fatalf("ssh should answer through the local runner: %+v", got[0])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/hosts/ -run TestCheck`
Expected: FAIL.

- [ ] **Step 3: Implement**

Six findings, computed concurrently with an `errgroup` or plain goroutines and a `sync.WaitGroup`, returned in a fixed order: `ssh` (`true` when `Run(ctx, "true")` exits 0), `rudy` (`command -v rudy` under `remotePath`), `version` (`rudy --version` there against ours, fix `rudy hosts install <host>`), `source` (`git -C <source> rev-parse` and `test -d <source>/../memory`, fix `git clone github.com/guygrigsby/rudy <source>` plus the memory sibling), `daemon` (`rudy bridge --no-start` exits 0 within 5s, fix "starts on first --host"), `placement` (`Inspect` says exists, and is git when the local tree is; fix `rudy hosts push <host>`). `rudy hosts check [host]` prints one line per finding, `ok` or `gap` with the fix, exits 1 on any gap.

- [ ] **Step 4: Run, break, restore, `make check`, commit, push**

```bash
git add -A && git commit -m "hosts: check names every gap and the command that fixes it" && git push
```

---

## Task 9: the TUI over a host

**Files:**
- Modify: `internal/tui/app/model.go` (`Options.Host string`, `Options.Redial func() (*protocol.Client, error)`, `disconnected` handling), `internal/tui/app/status.go` (workspace item prefix), `internal/tui/app/client.go` (reconnect command)
- Modify: `internal/cli/tui.go` (pass `Host`, `Redial`, version notice)
- Test: `internal/tui/app/status_test.go`, a new `internal/tui/app/reconnect_test.go`

**Interfaces:**
- Consumes: `config.StatusConfig.Host`, `dialed.Host`, `dialSSH`.
- Produces: `app.Options.Host string` (empty local), `app.Options.Redial func() (*protocol.Client, error)`; `ReconnectedMsg{Client *protocol.Client; Info protocol.SessionInfo}`.

- [ ] **Step 1: Write the failing tests**

`status_test.go`:

```go
func TestWorkspaceItemCarriesTheHost(t *testing.T) {
	h := newHarness(t, map[string]any{"ui.status.items": []string{"workspace"}, "ui.status.host": true})
	h.opts.Host = "box"
	h.opts.Workspace = "demo main"
	if got := h.statusLine(); !strings.Contains(got, "box:demo main") {
		t.Fatalf("status = %q, want the host before the workspace", got)
	}
	h2 := newHarness(t, map[string]any{"ui.status.items": []string{"workspace"}, "ui.status.host": false})
	h2.opts.Host = "box"
	h2.opts.Workspace = "demo main"
	if got := h2.statusLine(); strings.Contains(got, "box:") {
		t.Fatalf("ui.status.host=false still drew the host: %q", got)
	}
}
```

Adapt to the harness `status_test.go` already has (`newHarness` and however it renders the status line). `reconnect_test.go`, using the in-memory server the app tests build:

```go
func TestADroppedConnectionRedialsAndResumes(t *testing.T) {
	h := newHarness(t, nil)
	redials := 0
	h.opts.Redial = func() (*protocol.Client, error) {
		redials++
		return h.newClient(), nil // a fresh client to the same server
	}
	m := h.model()
	// Drop the first client: the pump reports it, the model redials and resumes.
	_ = h.client.Close()
	h.drain(m, 2*time.Second) // run Update on messages until quiet
	if redials != 1 {
		t.Fatalf("redials = %d", redials)
	}
	if m.disconnected {
		t.Fatal("still disconnected after a successful redial")
	}
	if !h.sawNotification("entry.appended") {
		t.Fatal("no replay arrived: the resume did not happen")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

- [ ] **Step 3: Implement**

`Options.Host` and `Redial`. In `update`'s `DisconnectedMsg` case: when `m.redial != nil` and the operator has not quit, set a notice "connection lost; reconnecting" and return `m.reconnect(delay)` which sleeps `delay` (1s, doubling to 30s across attempts), calls `Redial`, on success calls `session.resume` with `m.session.SessionID` and returns `ReconnectedMsg`; on failure returns `DisconnectedMsg` again with the next delay in the model. `ReconnectedMsg` swaps `m.cl`, re-arms `pump`, clears `disconnected`, notes "reconnected". The transcript is rebuilt from the replay the same way a fresh attach does; read how the model handles the initial replay and reuse it (clear rows, then fold `entry.appended`). `status.go`: when `Options.Host != ""` and `cfg.UI.Status.Host`, the workspace item is `host + ":" + item`. `tui.go`: `Host: r.dial.Host.String()`, `Redial` set only when `d.Host` is non-zero, calling `dialSSH` again with the same args; the version notice `host runs rudy X, this is Y` via the app's notice path when `d.Version != Version()`.

- [ ] **Step 4: Run, break (drop the re-arm of `pump` after reconnect), watch, restore**

- [ ] **Step 5: Try it by hand**

`rudy --host <box>` from a checkout under home; kill the ssh process from another terminal; the TUI should say reconnecting and come back with the transcript. Report what happened.

- [ ] **Step 6: `make check`, commit, push**

```bash
git add -A && git commit -m "tui: the host in the status bar, a version notice and reconnect over ssh" && git push
```

---

## Task 10: the contracts walk and the epic close

**Files:**
- Modify: `docs/specs/rudy-contracts.md` (walk pass 7's rows against the code; correct any row the code contradicts, the way passes 3 and 4 were walked)
- Modify: `docs/specs/2026-09-13-remote-runtime-design.md` (record Task 7's narrowing of automatic install to the 111 case; anything else the implementation changed)
- Modify: `README.md` (a "Remote runtime" section: `--host`, `remote.host`, `rudy hosts check` first, the sync rules in four lines)
- Modify: `docs/adr/0014-the-serve-wave.md` (append a line under Status: "Amended by ADR 0029: the dial order gains --host before the local probe")

- [ ] **Step 1: Walk every pass 7 row** against the code with `grep`, fix rows, note each fix in the pass 7 sentence at the top the way pass 4 did.
- [ ] **Step 2: README and spec updates.**
- [ ] **Step 3: `bd close` every task bead and the epic; commit the export.**
- [ ] **Step 4: `make check`, commit, push**

```bash
git add -A && git commit -m "docs: walk contracts pass 7 against the code and close the remote runtime epic" && git push
```

---

## Self-review

- Spec coverage: Host, Placement (T2), Bridge (T3), dial order and no fallback (T4), Sync in (T5), Sync out and close-time pull (T6), version notice and install (T7, with one narrowing ruled and recorded in T10), doctor (T8), status item, version notice in the TUI, reconnect (T9), config keys and hello (T1), logging records (T3, T4, T5 name them), errors table (each row lands in the task that owns the situation), testing through the shim (T4 onward).
- Placeholders: none; every code step carries code. Two steps say "adapt to the harness that exists" for the tui tests because the harness names are in `status_test.go`, which the implementer reads first.
- Types: `Runner.Run(ctx, line, stdin)` is the signature from T5 onward; T3's `runBridge` predates it and does not use it. `dialed.Host`, `Home`, `Cwd`, `copied` are introduced in T4 and T6 and read in T5, T6 and T9. `hosts.Revision` (T2) feeds T7. `ExitNoRudy` (T2) is read in T4.
