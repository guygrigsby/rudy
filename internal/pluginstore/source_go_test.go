// SPDX-License-Identifier: AGPL-3.0-or-later

package pluginstore

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// requireGo skips a test when go is not on PATH: these tests run the real `go mod download`,
// and cannot fake that out.
func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found on PATH")
	}
}

// newGoTestStore is newTestStore plus a cleanup that undoes the module cache's own read-only
// permissions before t.TempDir() tries to remove the store's root: go mod download deliberately
// leaves GOMODCACHE's directories unwritable (dr-xr-xr-x), which os.RemoveAll cannot unlink
// through no matter who owns them, so t.TempDir()'s automatic cleanup fails the test unless
// something chmods the tree writable first. Registered after t.TempDir()'s own cleanup (inside
// New, via newTestStore), so LIFO order runs this one first.
func newGoTestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	t.Cleanup(func() { chmodWritable(s.goModCacheDir()) })
	return s
}

// chmodWritable adds the owner-write bit to every file and directory under dir. Best effort:
// dir may not exist at all (a test that never ran stageGo), which is not a cleanup failure.
func chmodWritable(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			_ = os.Chmod(path, info.Mode()|0o200)
		}
		return nil
	})
}

// goModuleFixture builds a throwaway module (a go.mod, plugin.toml and main.go, per the task
// brief) and serves it through a file-based GOPROXY laid out the way a real module proxy
// answers: <module>/@v/list, <version>.info, <version>.mod and <version>.zip. This is the
// cheaper of the two fixture shapes the brief allows: the alternative, GOPROXY=direct against a
// local git repository tagged v0.1.0, still needs go's own go-import HTTP discovery to resolve
// an arbitrary module domain to a repository, which a local checkout does not remove; the file
// proxy has no such step; `go mod download` reads it directly, offline, with no VCS resolution
// and no git host involved at all.
func goModuleFixture(t *testing.T) (module, version, proxyURL string) {
	t.Helper()
	module = "example.com/rudytest/hello"
	version = "v0.1.0"
	proxyDir := t.TempDir()
	writeProxyVersion(t, proxyDir, module, version)
	return module, version, "file://" + proxyDir
}

// writeProxyVersion adds one version of module to a file-based GOPROXY tree already rooted at
// proxyDir: its @v/list entry (appended, never replacing what is already there, since go
// resolves "@latest" by picking the highest semver the list names), .info, .mod and .zip. A
// second call against the same proxyDir is how a test simulates a new release landing after an
// install already happened.
func writeProxyVersion(t *testing.T, proxyDir, module, version string) {
	t.Helper()
	goMod := "module " + module + "\n\ngo 1.21\n"
	files := map[string]string{
		"go.mod": goMod,
		// helloManifest (store_test.go) is a valid plugin.toml naming "hello"; reused here
		// rather than duplicating another manifest literal.
		"plugin.toml": helloManifest,
		"main.go":     "package main\n\nfunc main() {}\n",
	}

	verDir := filepath.Join(proxyDir, filepath.FromSlash(module), "@v")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(verDir, "list")
	existing, _ := os.ReadFile(listPath) // absent on the first version; fine either way
	if err := os.WriteFile(listPath, append(existing, []byte(version+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	info := fmt.Sprintf(`{"Version":%q,"Time":"2024-01-01T00:00:00Z"}`, version)
	if err := os.WriteFile(filepath.Join(verDir, version+".info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, version+".mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeModuleZip(filepath.Join(verDir, version+".zip"), module, version, files); err != nil {
		t.Fatal(err)
	}
}

// writeModuleZip builds a module proxy .zip: every file rooted under one top-level
// module@version/ directory, the layout `go mod download` requires of it.
func writeModuleZip(path, module, version string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	prefix := module + "@" + version + "/"
	for name, content := range files {
		w, err := zw.Create(prefix + name)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// TestInstallsAGoModuleFromTheProxy covers ADR 0025 decision 1's go: kind end to end: the
// module proxy resolves the source (go mod download is the only thing stageGo runs, no git
// invocation anywhere in source_go.go), and the lock records what it reported. Assertions read
// the lock back off disk (s.Lock(), the file's own convention: see store_test.go's
// TestInstallClonesGitSourceAndRecordsLock) rather than the Installed value Install happens to
// return in memory, so a TOML round-trip bug would actually be caught.
func TestInstallsAGoModuleFromTheProxy(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	_, m, err := s.Install(context.Background(), "go:"+module+"@"+version, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	locked := s.Lock().Plugins["hello"]
	if locked.Kind != KindGo {
		t.Fatalf("Kind = %s, want %s", locked.Kind, KindGo)
	}
	if locked.Ref != version {
		t.Fatalf("Ref = %q, want %q", locked.Ref, version)
	}
	if !strings.HasPrefix(locked.Digest, version) || !strings.Contains(locked.Digest, " h1:") {
		t.Fatalf("Digest = %q, want it to start with %q and contain \" h1:\"", locked.Digest, version)
	}
	if m.Name != "hello" {
		t.Fatalf("Name = %s, want hello", m.Name)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "plugin.toml")); err != nil {
		t.Fatalf("plugin.toml missing from the installed checkout: %v", err)
	}
}

// TestInstallsAGoModuleAtLatestWhenNoVersionGiven covers the empty-ref half of the contracts
// row: "go:module" with no "@version" means latest, resolved by stageGo, not rewritten to the
// literal string "latest" by ParseSource (task 3's ruling) or left for the operator to spell
// out themselves.
func TestInstallsAGoModuleAtLatestWhenNoVersionGiven(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	if _, _, err := s.Install(context.Background(), "go:"+module, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	locked := s.Lock().Plugins["hello"]
	if locked.Ref != "" {
		t.Fatalf("Ref = %q, want empty (nothing was typed)", locked.Ref)
	}
	if !strings.HasPrefix(locked.Digest, version) {
		t.Fatalf("Digest = %q, want it to start with the resolved latest version %q", locked.Digest, version)
	}
}

// TestStageGoUpdateRefusesAChangedPin covers the tamper guard itself, called directly rather
// than through Store.Update: a real "the proxy served different content for the same pinned
// version" scenario cannot be constructed cheaply through the full Install/Update path, since
// go's own module cache never re-verifies a version it has already downloaded (confirmed
// empirically: re-querying a cached module@version returns the cached digest regardless of
// what the proxy now serves). There is also no lock entry to assert here, since the whole point
// is that Update must refuse before anything is staged, built or swapped in, let alone written
// to the lock.
func TestStageGoUpdateRefusesAChangedPin(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}
	src := Source{Kind: KindGo, Location: module, Ref: version}

	staleDigest := version + " h1:not-the-real-sum-at-all-any-longer="
	if _, err := s.stageGoUpdate(context.Background(), src, t.TempDir(), staleDigest); err == nil {
		t.Fatal("stageGoUpdate: want an error when the proxy's digest no longer matches the pinned prior one")
	}
}

// TestUpdateAtLatestAdvancesAcrossANewRelease covers fix round 1's Important 1: an explicit or
// implicit "latest" is a moving target, not a pin, so a new release landing between an install
// and an update must advance the digest rather than be refused as tampering. ParseSource leaves
// "latest" in Ref exactly as typed (never collapsed with empty), so the lock's ref column
// stays "latest"; only stageGoUpdate's tamper check needs to treat the two alike.
func TestUpdateAtLatestAdvancesAcrossANewRelease(t *testing.T) {
	requireGo(t)
	module, first, proxyURL := goModuleFixture(t)
	proxyDir := strings.TrimPrefix(proxyURL, "file://")
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	if _, _, err := s.Install(context.Background(), "go:"+module+"@latest", time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	before := s.Lock().Plugins["hello"]
	if before.Ref != "latest" {
		t.Fatalf("Ref = %q, want the literal \"latest\" as typed", before.Ref)
	}
	if !strings.HasPrefix(before.Digest, first) {
		t.Fatalf("Digest = %q, want it to start with %q", before.Digest, first)
	}

	second := "v0.2.0"
	writeProxyVersion(t, proxyDir, module, second)

	if _, _, err := s.Update(context.Background(), "hello", time.Now()); err != nil {
		t.Fatalf("Update at latest across a new release: %v", err)
	}
	after := s.Lock().Plugins["hello"]
	if after.Ref != "latest" {
		t.Fatalf("Ref after Update = %q, want it to stay the literal \"latest\"", after.Ref)
	}
	if !strings.HasPrefix(after.Digest, second) {
		t.Fatalf("Digest after Update = %q, want it to advance to %q", after.Digest, second)
	}
	if after.Digest == before.Digest {
		t.Fatal("digest did not advance across the new release")
	}
}

// TestUpdateRunsAGoModuleThroughTheFullDispatch covers Store.Update's own wiring end to end:
// install a plugin pinned to a version, update it, and confirm the lock still carries the same
// ref and digest, and the checkout still holds plugin.toml, the way an update with nothing new
// to fetch should leave things.
func TestUpdateRunsAGoModuleThroughTheFullDispatch(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	if _, _, err := s.Install(context.Background(), "go:"+module+"@"+version, time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	before := s.Lock().Plugins["hello"]

	if _, _, err := s.Update(context.Background(), "hello", time.Now()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after := s.Lock().Plugins["hello"]
	if after.Ref != version {
		t.Fatalf("Ref after Update = %q, want %q", after.Ref, version)
	}
	if after.Digest != before.Digest {
		t.Fatalf("Digest after Update = %q, want it unchanged at %q", after.Digest, before.Digest)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "plugin.toml")); err != nil {
		t.Fatalf("plugin.toml missing from the updated checkout: %v", err)
	}
}

// TestAFailedGoDownloadNamesTheReason covers what every go: failure used to report: nothing.
// `go mod download -json` writes the cause into its JSON object's Error field on stdout and
// leaves stderr empty, so formatting stderr produced "exit status 1: " and stopped, which reads
// the same for a typo, a tag that does not exist, a private module with no credentials and a
// proxy that is down. The assertion is on the text after the exit status, not on the module
// name: this package's own error prefix already names the module, so an assertion on that would
// pass with an empty reason exactly as before.
func TestAFailedGoDownloadNamesTheReason(t *testing.T) {
	requireGo(t)
	_, _, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	_, _, err := s.Install(context.Background(), "go:example.com/rudytest/absent@v9.9.9", time.Now())
	if err == nil {
		t.Fatal("Install: want an error for a module the proxy does not serve")
	}
	if !regexp.MustCompile(`exit status \d+: \S`).MatchString(err.Error()) {
		t.Fatalf("err = %q, want a reason after the exit status", err)
	}
	// From the go tool's own Error field: the proxy file it could not read. Nothing but that
	// field carries it, so this is what proves the reason was parsed off stdout.
	if !strings.Contains(err.Error(), "v9.9.9.info") {
		t.Fatalf("err = %q, want the proxy path the go tool reported", err)
	}
}
