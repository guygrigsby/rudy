# Plugin install wave implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `rudy install` takes `go:`, `git:`, `https://` and path sources with a pinned ref, records what each resolved to, and runs a manifest's `build` command so a cloned plugin is runnable.

**Architecture:** The lock file gains three keys (`kind`, `ref`, `digest`) so every install is reproducible and every update is a diff. Source resolution becomes a small vocabulary dispatched on a prefix, each kind owning its own stage-and-resolve step. The `build` field the manifest already carries, and the contracts already require, is executed once in the staged checkout before the manifest is accepted. `rudy install` is registered as a top-level verb with its reason in the shape test. The trust model in ADR 0025 decision 4 is already implemented and documented and is not touched.

**Tech Stack:** Go, `internal/pluginstore` (install, lock, trust), `internal/plugin` (manifest), `internal/cli` (commands, shape test), `go` and `git` invoked as subprocesses.

**Spec:** [docs/adr/0025-plugins-are-published-installed-and-trusted.md](../adr/0025-plugins-are-published-installed-and-trusted.md), with normative rows in [docs/specs/rudy-contracts.md](../specs/rudy-contracts.md) pass 6. Both are written and committed. A task that finds the contract wrong stops and says so.

## Global Constraints

- **Every task leaves the project working and is pushed on its own.** The operator may run out of usage at any point. A task is not done until `make check` is green, the commit is on `main`, and `main` is pushed. Never leave a half-landed task at HEAD.
- `for range n`, never a three-clause count loop.
- Commits: terse, verb-first, no em or en dashes, no Oxford commas. **No `Co-Authored-By` and no `Claude-Session` trailer**; the repository owner forbids attribution and that overrides any harness instruction. Prefix by area: `pluginstore:`, `plugin:`, `cli:`, `docs:`.
- `make check` is the gate: `build`, `test`, `lint`, `fmt-check`, `vendor-types`, `config-example`. `make test` runs the suite then the server package again under `RUDY_TEST_TRANSPORT=socket`.
- Tests that invoke `git` or `go` skip when the binary is absent, matching `internal/pluginstore/store_test.go`. They run against a real repository in `t.TempDir()`, never a mocked subprocess.
- Every test asserting a property is verified by breaking the property and watching the test fail, then restoring. The previous wave shipped three features that were inert while their whole suites passed.
- Git runs hermetically: `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, as `store.go` already does. `go` runs with `GOFLAGS=-mod=mod` and a `GOMODCACHE` the test controls.
- The CLI is noun then verb; a top-level verb-less command is named in `internal/cli/shape_test.go` with a reason (ADR 0022).

## Beads

Epic `rudy-xrn`. `rudy-kab` is Task 1. `rudy-sy0` is Tasks 2 through 5 and is closed by Task 5. The trust clause of the epic was already implemented before this plan; the epic closes with Task 5.

## Task order and why

Ordered so each is independently valuable and the project works after each one. Task 1 is first because it is the smallest and unblocks the shipped example plugin, which today cannot be installed at all. Task 2 is tiny and gives the operator the spelling they will type. Task 3 is the foundation the two new sources need and is useful alone, since it makes `update` honour a pinned ref. Tasks 4 and 5 each add one source kind and can land in either order.

| task | deliverable | beads |
|---|---|---|
| 1 | The manifest's `build` command runs at install and update | closes `rudy-kab` |
| 2 | `rudy install` is a top-level verb | `rudy-sy0` |
| 3 | The lock records `kind`, `ref` and `digest`; `git:` takes `@ref` and update honours it | `rudy-sy0` |
| 4 | `go:module/path@version` installs from the module proxy | `rudy-sy0` |
| 5 | `https://` installs a `.tar.gz` whose root holds `plugin.toml` | closes `rudy-sy0`, closes `rudy-xrn` |

---

## Task 1: `build` runs at install and update

**Files:**
- Modify: `internal/pluginstore/store.go` (`Install` around line 212, `Update` around 326, the stage helpers around 434)
- Modify: `internal/cli/plugin.go:66-84` (the install command's output)
- Modify: `examples/plugins/hello/plugin.toml` (add a `build` line)
- Test: `internal/pluginstore/store_test.go`, `internal/cli/plugin_test.go`

**Interfaces:**
- Consumes: `plugin.Manifest.Build` (`internal/plugin/spawned.go:42`), already parsed and already in the trust digest.
- Produces: `runBuild(ctx, dir, command string, out io.Writer) error`, called by `Install` and `Update` after the source lands and before `ReadManifest` is trusted.

**Why:** `build` is a normative row in the contracts and a field on the manifest, digested into the trust record and printed in the trust prompt, and never executed. `examples/plugins/hello` names `command = "hello"`, which must already be on `PATH`, so the shipped example cannot be installed. `rudy-kab`.

- [ ] **Step 1: Write the failing tests**

```go
func TestInstallRunsTheManifestsBuild(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, `name = "built"
version = "0.1.0"
protocol_version = 1
command = "./built"
build = "printf '#!/bin/sh\necho hi\n' > built && chmod +x built"
`)
	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir(inst.Name), "built")); err != nil {
		t.Fatalf("build did not produce the binary: %v", err)
	}
}

func TestAFailingBuildRefusesTheInstall(t *testing.T) {
	requireGit(t)
	src := newSourceRepo(t, `name = "broken"
version = "0.1.0"
protocol_version = 1
command = "./nothing"
build = "exit 3"
`)
	s := newTestStore(t)
	_, _, err := s.Install(context.Background(), src, time.Now())
	if err == nil {
		t.Fatal("a failing build was accepted")
	}
	if !strings.Contains(err.Error(), "build") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("error names neither the step nor the exit code: %v", err)
	}
	if _, ok := s.Lock().Plugins["broken"]; ok {
		t.Fatal("a plugin whose build failed was written to the lock")
	}
	if _, err := os.Stat(s.PluginDir("broken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed install left its checkout behind")
	}
}

func TestUpdateRunsTheBuildAgain(t *testing.T) {
	// Install with a build that writes a marker, commit a change to the source that
	// changes the marker, update, and assert the new marker is present. Proves update
	// re-runs build rather than keeping the stale artefact.
}
```

`newSourceRepo(t, manifest)` exists at `store_test.go:62`; extend it or add a sibling that takes manifest text. `s.PluginDir` and `s.Lock` may need small test accessors; add them if absent.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/pluginstore/ -run 'RunsTheManifestsBuild|FailingBuild|RunsTheBuildAgain' -v`
Expected: FAIL. The first cannot find the binary because nothing ran `build`.

- [ ] **Step 3: Implement `runBuild` and call it from `Install` and `Update`**

```go
// runBuild runs a manifest's build command once in the staged checkout, with the checkout as
// its working directory and the operator's environment. It executes arbitrary code by
// construction: that is what installing from a source means, and the install command says so
// before it happens rather than implying otherwise (ADR 0025).
func runBuild(ctx context.Context, dir, command string, out io.Writer) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %q: %w", command, err)
	}
	return nil
}
```

In `Install`, after the source is staged and the manifest read but before the stage is promoted to `plugins/<name>` and the lock written: `if err := runBuild(ctx, stage, m.Build, s.out); err != nil { remove the stage; return err }`. Same in `Update` after the fetch and reset. Thread an `io.Writer` into the store for build output; the CLI passes its stdout so the operator sees the build.

- [ ] **Step 4: Say what is about to happen**

In `internal/cli/plugin.go`'s install command, before calling `Install`, print one line naming the source and that its manifest may run a build command. When the manifest has a `build`, the store prints `building <name>: <command>` before running it.

- [ ] **Step 5: Give the example a build**

`examples/plugins/hello/plugin.toml`: change `command = "hello"` to `command = "./hello"` and add `build = "go build -o hello ."`. Then confirm the spawned tests in `internal/server/spawned_test.go` still compile it themselves and still pass, since they do not go through install.

- [ ] **Step 6: Run the tests to verify they pass, break each, restore**

Run: `go test ./internal/pluginstore/ ./internal/cli/ -run 'Build|Install' -v`
Then: comment out the `runBuild` call in `Install`, confirm the first test fails; make `runBuild` ignore the exit code, confirm the second fails; comment out the call in `Update`, confirm the third fails. Restore.

- [ ] **Step 7: `make check`, commit, push**

```bash
git add internal/pluginstore internal/cli examples/plugins/hello
git commit -m "pluginstore: install and update run the manifest's build"
bd close rudy-kab --reason "build runs in the staged checkout at install and update; the hello example builds itself"
git pull --rebase && git push
```

---

## Task 2: `rudy install` is a top-level verb

**Files:**
- Modify: `internal/cli/root.go:37` (register the command), `internal/cli/plugin.go` (expose the install command's constructor)
- Modify: `internal/cli/shape_test.go:20-24` (`topLevelVerbs`)
- Test: `internal/cli/shape_test.go`, `internal/cli/plugin_test.go`

**Interfaces:**
- Consumes: the existing `rudy plugins install` cobra command.
- Produces: `rudy install <source>`, the same command registered at the root.

- [ ] **Step 1: Add the shape-test exception first and watch it fail**

In `shape_test.go`, add to `topLevelVerbs`:

```go
	"install": "the one verb everybody types; the noun form rudy plugins install remains (ADR 0025)",
```

Run: `go test ./internal/cli/ -run Shape -v`
Expected: FAIL, because the test asserts each named exception actually exists at the root and `install` does not yet.

- [ ] **Step 2: Register it**

In `root.go`, alongside the other top-level registrations, add the same cobra command the `plugins` noun uses for `install`. Do not duplicate the command body: factor `newInstallCmd(s)` out of `plugin.go` and call it from both places.

- [ ] **Step 3: Prove both spellings do the same thing**

```go
func TestInstallIsTheSameCommandUnderBothSpellings(t *testing.T) {
	requireGit(t)
	src := newPluginSourceRepo(t)
	for _, argv := range [][]string{{"install", src}, {"plugins", "install", src}} {
		// fresh store per iteration; run root with argv; assert the lock names the plugin
	}
}
```

- [ ] **Step 4: `make check`, commit, push**

```bash
git commit -m "cli: rudy install is the top-level spelling"
git pull --rebase && git push
```

---

## Task 3: The lock records `kind`, `ref` and `digest`; `git:` takes `@ref`

**Files:**
- Modify: `internal/pluginstore/store.go:27-39` (`Installed`), `:402-451` (`isRemoteSource`, `resolveSource`, `stageSource`), `:326-364` (`Update`)
- Create: `internal/pluginstore/source.go` (the source vocabulary)
- Test: `internal/pluginstore/source_test.go`, `internal/pluginstore/store_test.go`

**Interfaces:**
- Produces:
  ```go
  type Kind string
  const (KindGit Kind = "git"; KindPath Kind = "path"; KindGo Kind = "go"; KindHTTPS Kind = "https")
  type Source struct { Kind Kind; Location string; Ref string; AsTyped string }
  func ParseSource(s string) (Source, error)
  ```
  and `Installed` gains `Kind Kind`, `Ref string`, `Digest string`. Tasks 4 and 5 add a stage function per kind behind one `stage(ctx, src Source, dir string) (resolved string, err error)` dispatch.

**Why:** today `Update` hard-resets to `FETCH_HEAD` of whatever `origin` tracks, so an install at a tag drifts to the branch tip on update. The contracts now say `ref` is re-resolved and never the tip.

- [ ] **Step 1: Write the failing parser tests**

```go
func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Source
		err  bool
	}{
		{"git:github.com/a/b@v1.2.0", Source{Kind: KindGit, Location: "https://github.com/a/b", Ref: "v1.2.0"}, false},
		{"git:github.com/a/b", Source{Kind: KindGit, Location: "https://github.com/a/b"}, false},
		{"git@github.com:a/b.git", Source{Kind: KindGit, Location: "git@github.com:a/b.git"}, false},
		{"https://github.com/a/b.git", Source{Kind: KindGit, Location: "https://github.com/a/b.git"}, false},
		{"go:example.com/m/plugin@v0.3.1", Source{Kind: KindGo, Location: "example.com/m/plugin", Ref: "v0.3.1"}, false},
		{"go:example.com/m/plugin", Source{Kind: KindGo, Location: "example.com/m/plugin", Ref: "latest"}, false},
		{"https://example.com/p.tar.gz", Source{Kind: KindHTTPS, Location: "https://example.com/p.tar.gz"}, false},
		{"./local", Source{Kind: KindPath}, false}, // Location is the absolute path; assert kind only
		{"go:", Source{}, true},
		{"git:@v1", Source{}, true},
	} {
		got, err := ParseSource(tc.in)
		// ...
	}
}
```

Note the ambiguity to resolve deliberately: an `https://` URL ending in `.git`, or one a `git ls-remote` would answer, is `git`; one ending in `.tar.gz` is `https`. Write the rule down in `ParseSource`'s comment and test both sides.

- [ ] **Step 2: Write the failing pin tests**

```go
func TestInstallAtARefStaysThereOnUpdate(t *testing.T) {
	requireGit(t)
	src, tagged := newTaggedSourceRepo(t, "v1") // returns the repo path and the commit v1 points at
	s := newTestStore(t)
	inst, _, err := s.Install(context.Background(), "git:"+src+"@v1", time.Now())
	// commit more to src's default branch after the tag
	// s.Update(...)
	// assert the lock's commit is still `tagged` and the lock's ref is "v1"
}

func TestLockRecordsKindAndRef(t *testing.T) {
	// install a git source with a ref; read the lock file bytes; assert kind = "git", ref = "v1",
	// and that a lock written without `kind` reads back as git when commit is set and path otherwise
}
```

- [ ] **Step 3: Implement**

`ParseSource` in `source.go`. `Installed` gains the three fields with `toml:"kind"`, `toml:"ref"`, `toml:"digest"`. Reading a lock without `kind` derives it (`commit != "" → git`, else `path`) so old locks keep meaning. `stageSource` becomes a dispatch on `src.Kind`; for `git` with a ref: `git clone --depth 1 --branch <ref>` when the ref is a branch or tag, falling back to a full clone plus `git checkout <ref>` for a bare commit. `Update` for `git`: `git fetch --depth 1 origin <ref>` then `reset --hard FETCH_HEAD` when ref is set; the existing default-branch behaviour when it is empty.

- [ ] **Step 4: Verify, break, restore, `make check`, commit, push**

Break: make `Update` ignore `ref`; confirm the pin test fails. Break: drop `kind` from the struct tag; confirm the lock test fails.

```bash
git commit -m "pluginstore: the lock records kind, ref and digest and git honours a pinned ref"
git pull --rebase && git push
```

---

## Task 4: `go:module/path@version`

**Files:**
- Create: `internal/pluginstore/source_go.go`
- Modify: `internal/pluginstore/store.go` (the stage dispatch)
- Test: `internal/pluginstore/source_go_test.go`

**Interfaces:**
- Consumes: Task 3's `Source` and dispatch.
- Produces: `stageGo(ctx, src Source, dir string) (digest string, err error)`.

**Design, stated so the implementer does not have to guess:** "installed the way `go install` does" means the module proxy resolves it, not a git host. Run `go mod download -json <module>@<ref>` with `GOFLAGS=-mod=mod`; parse the JSON for `Dir`, `Version` and `Sum`; copy `Dir` into the stage; record `Version + " " + Sum` as the digest. The manifest's `build` (Task 1) then builds the binary. `Update` re-runs the download with the recorded `ref` (`latest` re-resolves; a pinned version is a no-op unless the proxy's sum changed, which is an error worth surfacing).

- [ ] **Step 1: Write the failing test against a real local module**

Build a throwaway module in `t.TempDir()` with a `go.mod`, a `plugin.toml` and a `main.go`, serve it through a file-based `GOPROXY` (`GOPROXY=file://<dir>` with the proxy directory layout, or `GOFLAGS=-mod=mod GONOSUMDB=* GOPROXY=direct` against a local git repo tagged `v0.1.0`). Whichever is less code; say which in the report. Skip when `go` is absent.

```go
func TestInstallsAGoModuleFromTheProxy(t *testing.T) {
	requireGo(t)
	mod, ver := newLocalModule(t) // module path, version tag
	s := newTestStore(t)
	inst, _, err := s.Install(withProxy(t, context.Background()), "go:"+mod+"@"+ver, time.Now())
	// assert the lock's kind is go, ref is ver, digest starts with ver and contains " h1:"
	// assert plugin.toml is present in the plugin dir
}
```

- [ ] **Step 2: Implement `stageGo`, wire the dispatch, verify, break, restore**

Break: make `stageGo` record an empty digest; confirm the test fails.

- [ ] **Step 3: `make check`, commit, push**

```bash
git commit -m "pluginstore: go: installs a module through the proxy"
git pull --rebase && git push
```

---

## Task 5: `https://` installs a `.tar.gz`

**Files:**
- Create: `internal/pluginstore/source_https.go`
- Modify: `internal/pluginstore/store.go` (the stage dispatch), `internal/cli/plugin.go` (the warning line mentions the download)
- Test: `internal/pluginstore/source_https_test.go`

**Interfaces:**
- Consumes: Task 3's `Source` and dispatch.
- Produces: `stageHTTPS(ctx, src Source, dir string, client *http.Client) (digest string, err error)`.

**Design:** download the URL, hash the bytes with sha256 as they stream, unpack the gzip tarball into the stage refusing any entry whose cleaned path escapes the stage, and require `plugin.toml` at the root. Record `sha256:<hex>` as the digest. `Update` re-downloads and, when the digest matches, does nothing; when it differs, replaces the checkout and reruns `build`. Honour `Retry-After` and back off on 429 and 5xx, per the project's rule on being a good client of external services; set a `User-Agent` naming rudy and its version.

- [ ] **Step 1: Write the failing tests against `httptest`**

```go
func TestInstallsATarballOverHTTPS(t *testing.T) {
	tgz := tarballOf(t, map[string]string{"plugin.toml": minimalManifest, "run.sh": "#!/bin/sh\n"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) }))
	// install srv.URL + "/p.tar.gz" with srv.Client(); assert kind https and digest == "sha256:" + hex of tgz
}

func TestATarballEscapingTheStageIsRefused(t *testing.T) {
	// an entry named "../../evil" must fail the install and leave nothing outside the stage
}

func TestUpdateReplacesOnlyWhenTheDigestChanges(t *testing.T) {
	// serve tgz A, install, serve tgz B, update: digest changes and build reruns;
	// serve A again unchanged, update: nothing rewritten (check the checkout's mtime or a marker)
}
```

- [ ] **Step 2: Implement, verify, break, restore**

Break: skip the path-escape check; confirm the second test fails. Break: record the digest before hashing finishes; confirm the first fails.

- [ ] **Step 3: `make check`, commit, push, close the epic**

```bash
git commit -m "pluginstore: https installs a tarball and records its digest"
bd close rudy-sy0 --reason "go, git, https and path sources with a pinned ref, each recorded in the lock with what it resolved to; rudy install is the top-level spelling"
bd close rudy-xrn --reason "install sources, build step and the top-level spelling landed; the trust model had already shipped"
git pull --rebase && git push
```

---

## Self-review

**Spec coverage.** ADR 0025 decision 1 (sources vocabulary) is Tasks 3, 4 and 5. Decision 2 (`rudy install` top-level) is Task 2. Decision 3 (`build`) is Task 1. Decision 4 (trust) was already implemented and is deliberately untouched. Contracts pass 6 rows: `kind`, `ref`, `digest` are Task 3; the four-kinds paragraph is Tasks 3 through 5; the `build` sentence is Task 1; the `rudy install` sentence is Task 2.

**Type consistency.** `Source{Kind, Location, Ref, AsTyped}` and `ParseSource` are defined in Task 3 and consumed by 4 and 5. `runBuild` is Task 1 and reused by 3 through 5 through `Install`/`Update` unchanged. `Installed.Digest` is Task 3 and written by 4 and 5.

**Known soft spots.** Task 4's test fixture, a local module served to `go mod download`, is the fiddliest piece in the plan and the implementer is told to pick the cheaper of two shapes and say which. Task 3's `https://` ambiguity (`.git` versus `.tar.gz`) is a rule the implementer writes down; the test pins both sides.
