package pluginstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tarEntry is one entry tarballWithEntries writes, in the order given: order matters for the
// path-escape tests, which need a deterministic archive rather than a map's randomized one.
type tarEntry struct {
	name     string
	content  string
	typeflag byte // zero means tar.TypeReg
	linkname string
}

// tarballWithEntries builds a gzip tarball holding exactly entries, in order.
func tarballWithEntries(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}
		if e.typeflag != 0 {
			hdr.Typeflag = e.typeflag
			hdr.Linkname = e.linkname
		} else {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("Write(%s): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// tarballOf builds a gzip tarball whose root holds one regular-file entry per files, sorted by
// name for a deterministic archive; the tests that use this one don't care about entry order,
// unlike the escape tests, which build their tarball with tarballWithEntries directly.
func tarballOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]tarEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, tarEntry{name: name, content: files[name]})
	}
	return tarballWithEntries(t, entries)
}

// staticTarballServer answers every request with whatever body currently points to, letting a
// test swap the served tarball between requests (TestUpdateReplacesOnlyWhenTheDigestChanges).
func staticTarballServer(t *testing.T, body *[]byte, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(*body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInstallsATarballOverHTTPS covers the contracts row's https kind end to end: kind, digest
// (sha256 of the exact downloaded bytes) and the checkout's contents.
func TestInstallsATarballOverHTTPS(t *testing.T) {
	tgz := tarballOf(t, map[string]string{"plugin.toml": helloManifest, "run.sh": "#!/bin/sh\n"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	inst, m, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m.Name != "hello" {
		t.Fatalf("Name = %s, want hello", m.Name)
	}
	locked := s.Lock().Plugins["hello"]
	if locked.Kind != KindHTTPS {
		t.Fatalf("Kind = %s, want %s", locked.Kind, KindHTTPS)
	}
	sum := sha256.Sum256(tgz)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if locked.Digest != want {
		t.Fatalf("Digest = %q, want %q", locked.Digest, want)
	}
	if inst.Digest != want {
		t.Fatalf("Install's returned Digest = %q, want %q", inst.Digest, want)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "plugin.toml")); err != nil {
		t.Fatalf("plugin.toml missing from the installed checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "run.sh")); err != nil {
		t.Fatalf("run.sh missing from the installed checkout: %v", err)
	}
	// No stray download artifact should survive into the checkout.
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), ".download.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("stat .download.tar.gz = %v, want it removed before the checkout was renamed in", err)
	}
}

// TestATarballEscapingTheStageIsRefused covers the security property of an https: install: an
// entry whose cleaned name climbs out of the stage must refuse the whole install rather than
// write anywhere outside it.
func TestATarballEscapingTheStageIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{{name: "../../evil", content: "haha"}})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil {
		t.Fatal("Install with a ../../evil entry: want an error")
	}
	if !strings.Contains(err.Error(), "escapes the stage") {
		t.Fatalf("err = %v, want it to name the escape", err)
	}
	if _, statErr := os.Stat(filepath.Join(s.Root, "..", "evil")); !os.IsNotExist(statErr) {
		t.Fatalf("the escaping entry landed outside the store root: stat = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(s.Root), "evil")); !os.IsNotExist(statErr) {
		t.Fatalf("the escaping entry landed outside the store root: stat = %v", statErr)
	}
}

// TestATarballWithAnAbsolutePathIsRefused covers the other half of the traversal guard: an
// absolute entry name, which "../.." alone does not exercise.
func TestATarballWithAnAbsolutePathIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{{name: "/etc/evil", content: "haha"}})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil || !strings.Contains(err.Error(), "escapes the stage") {
		t.Fatalf("err = %v, want it to refuse the absolute entry name", err)
	}
	if _, statErr := os.Stat("/etc/evil"); !os.IsNotExist(statErr) {
		t.Fatalf("the absolute entry landed on disk: stat = %v", statErr)
	}
}

// TestATarballWithASymlinkEscapeIsRefused covers the third traversal shape: a symlink entry.
// unpackTarball refuses every symlink and hard link outright (see its comment), which is a
// strict superset of refusing only the ones that escape, so this also proves an escaping one is
// caught.
func TestATarballWithASymlinkEscapeIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "../../../../etc/passwd"}})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil || !strings.Contains(err.Error(), "not a regular file or a directory") {
		t.Fatalf("err = %v, want the symlink entry refused", err)
	}
}

// TestATarballNotRootedRefusesWithAHelpfulError covers the shape `tar czf plugin.tar.gz dir/`
// produces: every entry sits under one top-level directory, a likely operator mistake refused
// with an error naming what was found, rather than a bare "no such file" from ReadManifest.
func TestATarballNotRootedRefusesWithAHelpfulError(t *testing.T) {
	tgz := tarballOf(t, map[string]string{"myplugin/plugin.toml": helloManifest})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil {
		t.Fatal("Install with plugin.toml under a top-level directory: want an error")
	}
	if !strings.Contains(err.Error(), "myplugin") || !strings.Contains(err.Error(), "tarball root") {
		t.Fatalf("err = %v, want it to name the found directory and the expected root", err)
	}
}

// TestUpdateReplacesOnlyWhenTheDigestChanges covers the contracts row's update semantics for a
// non-git source: re-download; unchanged digest means do nothing (no build, no swap), changed
// digest means stage, build and swap.
//
// Each https: install or update stages from scratch (download, unpack), so a marker written
// inside the checkout itself cannot accumulate across separate stages the way a git checkout's
// own working tree could: a broken short-circuit that always restages and rebuilds would
// recreate the exact same in-checkout marker every time, making that shape of test pass whether
// or not the short-circuit exists. buildLog instead lives outside any stage, at a fixed path
// under the store's own root the manifest's build command is given directly, so its line count
// is the number of times build has actually run, full stop, regardless of which stage directory
// it ran in.
func TestUpdateReplacesOnlyWhenTheDigestChanges(t *testing.T) {
	s := newTestStore(t)
	buildLog := filepath.Join(s.Root, "buildlog")
	manifest := fmt.Sprintf(`name = "hello"
version = "0.1.0"
protocol_version = 1
command = "hello"
build = "printf x >> %s"
`, buildLog)

	tgzA := tarballOf(t, map[string]string{"plugin.toml": manifest})
	tgzB := tarballOf(t, map[string]string{"plugin.toml": manifest, "extra": "b"})

	var mu sync.Mutex
	current := tgzA
	srv := staticTarballServer(t, &current, &mu)
	s.HTTPClient = srv.Client()

	inst, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFileContent(t, buildLog, "x")
	firstDigest := inst.Digest

	// Upstream publishes a new tarball: digest changes, build reruns.
	mu.Lock()
	current = tgzB
	mu.Unlock()
	updated, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update to a changed tarball: %v", err)
	}
	if updated.Digest == firstDigest {
		t.Fatal("Digest did not change after the upstream tarball changed")
	}
	assertFileContent(t, buildLog, "xx")
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "extra")); err != nil {
		t.Fatalf("the new tarball's extra file is missing from the swapped-in checkout: %v", err)
	}

	// Nothing new upstream: the same bytes are re-downloaded, the digest matches what's already
	// recorded, and neither the build nor the swap runs again.
	again, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update with an unchanged tarball: %v", err)
	}
	if again.Digest != updated.Digest {
		t.Fatalf("Digest = %q, want it unchanged at %q", again.Digest, updated.Digest)
	}
	assertFileContent(t, buildLog, "xx")
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}

// TestDownloadRetriesOn429WithRetryAfterSeconds covers being a good client of an external
// service: a 429 with a Retry-After in the seconds form is honoured, and the install still
// succeeds once the server recovers, in exactly two requests.
func TestDownloadRetriesOn429WithRetryAfterSeconds(t *testing.T) {
	tgz := tarballOf(t, map[string]string{"plugin.toml": helloManifest})
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	if _, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want exactly 2", got)
	}
}

// TestDownloadFailsImmediatelyOnA4xx covers the other half of being a good client: a 4xx other
// than 429 is not the kind of failure retrying fixes, so it fails on the first request.
func TestDownloadFailsImmediatelyOnA4xx(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(http.StatusNotFound)) {
		t.Fatalf("err = %v, want it to name the 404 status", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (no retry on a plain 4xx)", got)
	}
}
