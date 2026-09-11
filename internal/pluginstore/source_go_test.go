package pluginstore

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	goMod := "module " + module + "\n\ngo 1.21\n"
	files := map[string]string{
		"go.mod": goMod,
		// helloManifest (store_test.go) is a valid plugin.toml naming "hello"; reused here
		// rather than duplicating another manifest literal.
		"plugin.toml": helloManifest,
		"main.go":     "package main\n\nfunc main() {}\n",
	}

	proxyDir := t.TempDir()
	verDir := filepath.Join(proxyDir, filepath.FromSlash(module), "@v")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "list"), []byte(version+"\n"), 0o644); err != nil {
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
	return module, version, "file://" + proxyDir
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
// invocation anywhere in source_go.go), and the lock records what it reported.
func TestInstallsAGoModuleFromTheProxy(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	inst, m, err := s.Install(context.Background(), "go:"+module+"@"+version, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.Kind != KindGo {
		t.Fatalf("Kind = %s, want %s", inst.Kind, KindGo)
	}
	if inst.Ref != version {
		t.Fatalf("Ref = %q, want %q", inst.Ref, version)
	}
	if !strings.HasPrefix(inst.Digest, version) || !strings.Contains(inst.Digest, " h1:") {
		t.Fatalf("Digest = %q, want it to start with %q and contain \" h1:\"", inst.Digest, version)
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

	inst, _, err := s.Install(context.Background(), "go:"+module, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.Ref != "" {
		t.Fatalf("Ref = %q, want empty (nothing was typed)", inst.Ref)
	}
	if !strings.HasPrefix(inst.Digest, version) {
		t.Fatalf("Digest = %q, want it to start with the resolved latest version %q", inst.Digest, version)
	}
}

// TestStageGoUpdateAcceptsAnUnchangedPin exercises stageGoUpdate directly, the way
// Store.Update calls it for a plugin pinned to a version: re-running the download for the same
// pinned ref must reproduce the same digest and must not error.
func TestStageGoUpdateAcceptsAnUnchangedPin(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}
	src := Source{Kind: KindGo, Location: module, Ref: version}

	first, err := s.stageGo(context.Background(), src, t.TempDir())
	if err != nil {
		t.Fatalf("stageGo: %v", err)
	}

	second, err := s.stageGoUpdate(context.Background(), src, t.TempDir(), first)
	if err != nil {
		t.Fatalf("stageGoUpdate on an unchanged pin: %v", err)
	}
	if second != first {
		t.Fatalf("digest changed on an unchanged pin: %q -> %q", first, second)
	}
}

// TestStageGoUpdateRefusesAChangedPin covers the other half: a pinned ref whose freshly
// resolved digest no longer matches the lock's recorded one must error rather than swap the
// checkout in, since a go module@version is supposed to be immutable once published.
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

// TestUpdateRunsAGoModuleThroughTheFullDispatch covers Store.Update's own wiring end to end,
// not just stageGoUpdate in isolation: install a plugin pinned to a version, update it, and
// confirm the lock still carries the same ref and digest, and the checkout still holds
// plugin.toml, the way a Update with nothing new to fetch should leave things.
func TestUpdateRunsAGoModuleThroughTheFullDispatch(t *testing.T) {
	requireGo(t)
	module, version, proxyURL := goModuleFixture(t)
	s := newGoTestStore(t)
	s.GoEnv = []string{"GOPROXY=" + proxyURL, "GONOSUMDB=*"}

	inst, _, err := s.Install(context.Background(), "go:"+module+"@"+version, time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Ref != version {
		t.Fatalf("Ref after Update = %q, want %q", updated.Ref, version)
	}
	if updated.Digest != inst.Digest {
		t.Fatalf("Digest after Update = %q, want it unchanged at %q", updated.Digest, inst.Digest)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "plugin.toml")); err != nil {
		t.Fatalf("plugin.toml missing from the updated checkout: %v", err)
	}
}
