package pluginstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
//
// The tarball also carries a legitimate plugin.toml at its root: without one, an unguarded
// build would still fail (checkManifestAtRoot has nothing to find), so the earlier version of
// this test passed for the wrong reason regardless of whether the traversal guard did anything
// at all. The containment check runs before the error checks, and against the one path an
// unguarded "../../evil" actually reaches: the stage sits two directories below s.Root
// (Root/plugins/.install-*), so climbing out two levels lands exactly at Root/evil.
func TestATarballEscapingTheStageIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{
		{name: "plugin.toml", content: helloManifest},
		{name: "../../evil", content: "haha"},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if _, statErr := os.Stat(filepath.Join(s.Root, "evil")); !os.IsNotExist(statErr) {
		t.Fatalf("the escaping entry landed at %s: stat = %v", filepath.Join(s.Root, "evil"), statErr)
	}
	if err == nil {
		t.Fatal("Install with a plugin.toml plus a ../../evil entry: want an error")
	}
	if !strings.Contains(err.Error(), "escapes the stage") {
		t.Fatalf("err = %v, want it to name the escape", err)
	}
}

// TestATarballWithAnAbsolutePathIsRefused covers the other half of the traversal guard: an
// absolute entry name, which "../.." alone does not exercise.
//
// filepath.Join never actually resolves a later absolute-looking element back to the real
// filesystem root (Join("/stage", "/etc/evil") is "/stage/etc/evil", not "/etc/evil"), so an
// unguarded absolute name lands inside the stage rather than escaping it outright; checking the
// real /etc/evil would pass whether or not the guard exists. stageHTTPS is called directly, with
// a stage this test owns, rather than through Install, since Install removes the whole stage on
// any error and would otherwise erase the evidence before this test could look at it.
func TestATarballWithAnAbsolutePathIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{
		{name: "plugin.toml", content: helloManifest},
		{name: "/etc/evil", content: "haha"},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	stage := t.TempDir()
	src := Source{Kind: KindHTTPS, Location: srv.URL + "/p.tar.gz"}
	_, err := stageHTTPS(context.Background(), src, stage, srv.Client())

	if _, statErr := os.Stat(filepath.Join(stage, "etc", "evil")); !os.IsNotExist(statErr) {
		t.Fatalf("the absolute-path entry landed inside the stage: stat = %v", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), "escapes the stage") {
		t.Fatalf("err = %v, want it to refuse the absolute entry name", err)
	}
}

// TestATarballWithABackslashInTheNameIsRefused covers a name like "..\..\evil": on a unix build,
// filepath.Clean leaves a backslash alone (it is not a separator there), so the name survives
// every other check as one legal, if odd, filename with no actual traversal effect on this
// platform. It is refused anyway, since this package's path handling is the OS-provided
// filepath, not a unix-only one, and there is no legitimate reason for a plugin bundle to name
// a file this way.
func TestATarballWithABackslashInTheNameIsRefused(t *testing.T) {
	tgz := tarballWithEntries(t, []tarEntry{
		{name: "plugin.toml", content: helloManifest},
		{name: `..\..\evil`, content: "haha"},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil || !strings.Contains(err.Error(), "backslash") {
		t.Fatalf("err = %v, want it to refuse the backslash in the entry name", err)
	}
}

// sparseBombTarballB64 is a genuine archive, produced by macOS bsdtar (libarchive), of a single
// sparse file: `python3 -c "f=open('bomb.bin','wb'); f.seek(size-1); f.write(b'\0')"` (size =
// 257 MiB, one MiB over maxUnpackedBytes) followed by `tar --format=pax -czf bomb.tar.gz
// bomb.bin`. The archive itself is 402 bytes; its one entry declares Size == the full 257 MiB.
// Go's archive/tar reads that entry back as an ordinary TypeReg with Size 269484032 and, if
// asked to, will synthesize that many zero bytes on Read without consuming more than the 402
// archive bytes backing it: nothing about the Header says "sparse" to a caller that only
// inspects Typeflag and Size, which is exactly why unpackTarball budgets bytes actually written
// rather than trusting Size to reflect real archive content, still less bytes read off the
// compressed stream.
const sparseBombTarballB64 = `H4sIANJ+pGoAA+3UTW/TMBjA8cIx4s7VnyDY8UvSQw9lAlZpRdCKSTu6nVUyNWmVdKPdV+Ezcdg3wp
00aYytp1ZD7P+7WPFjW49fnnzx6+Pgz0PzbrKoJumkrDt7J6V0xohtmzt720Z3rZTGWqFsZpXMc51lQionpeqI9f5T+dtl
u/JNTGV2uZk15aydbB4f9+N7CPMd6/y5KXGIVA9BSzFdlVXoqbzoKmcz51IjTZ65QtskRv0jUZfrvKu30erpufEiP33+lrZ
L37QhrfzFoumph71lHXtlkhX3e2sf17x7jonW92NN8PO2vA69zHVNYaTOEpuLk8H7/ujoeHD6IV371apJp4sq9cvlPKTLZ
nEVal9PQ6//ddAf2qvR9Wg6DKfDxHTFOE46Ods16dXrjv558+vtm804ee7LOoB4tOPbk/1Yxn3Lg/wFdte/UduPB/VvnekI
uc8knvLC6z9WZCwklztTJMrpwvyPjxwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/kG/AfBCaUIATAAA`

// decodeSparseBombTarball decodes sparseBombTarballB64, joining the wrapped lines back into one
// base64 string first.
func decodeSparseBombTarball(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(sparseBombTarballB64), ""))
	if err != nil {
		t.Fatalf("decode sparseBombTarballB64: %v", err)
	}
	return b
}

// TestASparseEntryClaimingAHugeSizeIsRefused is the Critical: a sparse tar entry's declared Size
// can vastly exceed the archive bytes that actually back it, so a cap that counts bytes read off
// the (possibly compressed) input stream never sees the true expanded size — verified against
// this exact archive before the fix landed: it read as 402 archive bytes through the old
// gzip-wrapping cap yet copied the full 269,484,032 declared bytes to disk with no error at all.
//
// stageHTTPS is called directly, with a stage this test owns, so the write can be inspected
// before Install's own cleanup (which runs on any error) would erase the evidence.
func TestASparseEntryClaimingAHugeSizeIsRefused(t *testing.T) {
	tgz := decodeSparseBombTarball(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	stage := t.TempDir()
	src := Source{Kind: KindHTTPS, Location: srv.URL + "/p.tar.gz"}
	_, err := stageHTTPS(context.Background(), src, stage, srv.Client())

	var written int64
	if walkErr := filepath.WalkDir(stage, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr == nil {
			written += info.Size()
		}
		return nil
	}); walkErr != nil {
		t.Fatalf("walk stage: %v", walkErr)
	}
	// Nothing this store legitimately writes for a refused install is anywhere near the
	// entry's declared 257 MiB; 10 MiB is a generous margin above the tiny download artifact
	// alone, so this catches "wrote most of the bomb before failing" just as well as "wrote all
	// of it".
	const tooMuch = 10 << 20
	if written > tooMuch {
		t.Fatalf("stage holds %d bytes after the refusal, want nowhere near the declared 257 MiB", written)
	}
	if err == nil {
		t.Fatal("Install of a sparse entry declaring 257 MiB: want an error")
	}
	if !strings.Contains(err.Error(), "byte unpacked cap") {
		t.Fatalf("err = %v, want it to name the unpacked cap", err)
	}
}

// TestAGitArchiveInstallsWithItsTopLevelDirectoryStripped covers the shape behind the single
// most likely https: URL an operator types,
// https://github.com/o/r/archive/refs/tags/v1.tar.gz: a pax_global_header entry, typeflag 'g',
// ahead of content that all sits under one directory named for the repository and its ref.
// That directory is stripped, per the contracts row, so the manifest lands at the root of the
// checkout and nothing is written under the extra level. The global header must still be
// skipped rather than refused as an entry type this store does not extract, which the install
// succeeding at all proves.
func TestAGitArchiveInstallsWithItsTopLevelDirectoryStripped(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": "git archive"},
	}); err != nil {
		t.Fatalf("WriteHeader(global): %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "myrepo-abc1234/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatalf("WriteHeader(dir): %v", err)
	}
	for _, e := range []struct{ name, content string }{
		{"myrepo-abc1234/plugin.toml", helloManifest},
		{"myrepo-abc1234/lib/run.sh", "#!/bin/sh\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Size:     int64(len(e.content)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatalf("Write(%s): %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	tgz := buf.Bytes()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	if _, m, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now()); err != nil {
		t.Fatalf("Install of a GitHub-shaped archive: %v", err)
	} else if m.Name != "hello" {
		t.Fatalf("Name = %s, want hello", m.Name)
	}
	dir := s.PluginDir("hello")
	if _, err := os.Stat(filepath.Join(dir, "plugin.toml")); err != nil {
		t.Fatalf("plugin.toml is not at the checkout root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "run.sh")); err != nil {
		t.Fatalf("lib/run.sh missing from the checkout: %v", err)
	}
	// The stripping happens while unpacking, so the extra level is never written at all.
	if _, err := os.Stat(filepath.Join(dir, "myrepo-abc1234")); !os.IsNotExist(err) {
		t.Fatalf("stat myrepo-abc1234 = %v, want the top-level directory never written", err)
	}
}

// TestDefaultHTTPClientRefusesARedirectToPlainHTTP covers the Important on the default client:
// without a CheckRedirect of its own, an https: URL that redirects to a plain http one would
// have the digest taken over whatever that unencrypted hop actually served. Tested directly
// against the func rather than through a live redirecting server: a real end-to-end redirect
// test would need a second, differently-trusted server for the http hop and adds nothing this
// unit test of the policy itself doesn't already prove.
func TestDefaultHTTPClientRefusesARedirectToPlainHTTP(t *testing.T) {
	toHTTP, err := http.NewRequest(http.MethodGet, "http://example.com/evil", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := defaultHTTPClient.CheckRedirect(toHTTP, nil); err == nil {
		t.Fatal("CheckRedirect: want an error for a redirect to a plain http URL")
	}
	toHTTPS, err := http.NewRequest(http.MethodGet, "https://example.com/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := defaultHTTPClient.CheckRedirect(toHTTPS, nil); err != nil {
		t.Fatalf("CheckRedirect: want no error for an https redirect target, got %v", err)
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

// TestATarballNestedUnderTwoDirectoriesIsRefused covers the other half of the contracts row:
// one sole top-level directory is stripped, and any other nesting is refused naming what was
// found. Two top-level directories is that "any other": there is no single prefix to strip, so
// the manifest is genuinely not at the root and the operator is told which entries were there
// instead of being left with a bare "no such file" from ReadManifest.
func TestATarballNestedUnderTwoDirectoriesIsRefused(t *testing.T) {
	tgz := tarballOf(t, map[string]string{
		"myplugin/plugin.toml": helloManifest,
		"docs/README.md":       "# docs\n",
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil {
		t.Fatal("Install with two top-level directories: want an error")
	}
	if !strings.Contains(err.Error(), "tarball root") ||
		!strings.Contains(err.Error(), "myplugin") || !strings.Contains(err.Error(), "docs") {
		t.Fatalf("err = %v, want it to name both top-level directories and the expected root", err)
	}
}

// TestATarballNestedTwoLevelsDeepIsRefused covers what survives the strip: an archive wrapping
// a wrapper. The sole top-level directory goes, and what is left still has no manifest at its
// root, so the refusal names the directory the manifest actually sits in rather than claiming
// the archive was empty of one.
func TestATarballNestedTwoLevelsDeepIsRefused(t *testing.T) {
	tgz := tarballOf(t, map[string]string{"r-1/myplugin/plugin.toml": helloManifest})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	s := newTestStore(t)
	s.HTTPClient = srv.Client()

	_, _, err := s.Install(context.Background(), srv.URL+"/p.tar.gz", time.Now())
	if err == nil {
		t.Fatal("Install with plugin.toml two directories deep: want an error")
	}
	if !strings.Contains(err.Error(), "myplugin") || !strings.Contains(err.Error(), "tarball root") {
		t.Fatalf("err = %v, want it to name the directory the manifest was found under", err)
	}
	if strings.Contains(err.Error(), "r-1") {
		t.Fatalf("err = %v, the sole top-level directory should have been stripped before this check", err)
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
	updated, changed, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update to a changed tarball: %v", err)
	}
	if updated.Digest == firstDigest {
		t.Fatal("Digest did not change after the upstream tarball changed")
	}
	if !changed {
		t.Fatal("changed = false after the upstream tarball changed")
	}
	assertFileContent(t, buildLog, "xx")
	if _, err := os.Stat(filepath.Join(s.PluginDir("hello"), "extra")); err != nil {
		t.Fatalf("the new tarball's extra file is missing from the swapped-in checkout: %v", err)
	}

	// Nothing new upstream: the same bytes are re-downloaded, the digest matches what's already
	// recorded, and neither the build nor the swap runs again.
	again, changedAgain, err := s.Update(context.Background(), "hello", time.Now())
	if err != nil {
		t.Fatalf("Update with an unchanged tarball: %v", err)
	}
	if again.Digest != updated.Digest {
		t.Fatalf("Digest = %q, want it unchanged at %q", again.Digest, updated.Digest)
	}
	// What the CLI prints "unchanged" from: nothing was rebuilt and nothing was swapped.
	if changedAgain {
		t.Fatal("changed = true for a re-download whose digest matched the lock")
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

// TestATarballWithTooManyEntriesIsRefused covers rudy-79q: moving the unpacked-size cap from
// the gzip stream to bytes actually written left the entry count bounded by nothing but the
// download cap, and an empty directory writes no bytes at all while gzipping to almost nothing.
// A million of them fit in 4.7 MiB, well inside maxDownloadBytes, and cost a MkdirAll and an
// inode each. The refusal happens in the scanning pass, before the first entry is written, so
// the stage is still empty afterwards.
func TestATarballWithTooManyEntriesIsRefused(t *testing.T) {
	entries := make([]tarEntry, 0, maxTarEntries+1)
	for i := range maxTarEntries + 1 {
		entries = append(entries, tarEntry{name: fmt.Sprintf("d%06d/", i), typeflag: tar.TypeDir})
	}
	tgz := tarballWithEntries(t, entries)
	// The point of the cap: the download cap does not come close to catching this.
	if len(tgz) > maxDownloadBytes {
		t.Fatalf("fixture is %d bytes, want it well inside the %d byte download cap", len(tgz), maxDownloadBytes)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	// stageHTTPS directly, with a stage this test owns, so what did or did not get written can be
	// looked at: Install removes the whole stage on any error.
	stage := t.TempDir()
	src := Source{Kind: KindHTTPS, Location: srv.URL + "/p.tar.gz"}
	_, err := stageHTTPS(context.Background(), src, stage, srv.Client())
	if err == nil {
		t.Fatalf("stageHTTPS of %d entries: want an error", len(entries))
	}
	if !strings.Contains(err.Error(), "entries") {
		t.Fatalf("err = %v, want it to name the entry cap", err)
	}
	left, readErr := os.ReadDir(stage)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(left) != 0 {
		t.Fatalf("stage holds %d entries after the refusal, want nothing written at all", len(left))
	}
}
