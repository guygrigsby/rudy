package pluginstore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// maxDownloadBytes bounds how much of an https: tarball stageHTTPS will read at all. The
// bytes come from wherever the operator's URL happens to serve them from; refusing a body past
// this cap, rather than reading to exhaustion, is what keeps a misbehaving or hostile server
// from exhausting disk on an install nobody watches to completion. 256 MiB is generous next to
// any plugin bundle actually meant to be checked out (source plus maybe a small binary).
const maxDownloadBytes = 256 << 20

// maxUnpackedBytes bounds the total bytes unpackTarball writes to disk while decompressing,
// independent of maxDownloadBytes: gzip compresses well, so a small downloaded body can still
// expand into an enormous one (a "gzip bomb"). The same figure is used for both caps for the
// same reason: nothing this store installs is expected to be bigger than that either way.
const maxUnpackedBytes = 256 << 20

// httpRetryAttempts bounds the retry loop stageHTTPS runs against a 429 or 5xx response: enough
// to ride out a rate limit or a transient server hiccup, not enough to hang an install forever
// against a server that is simply down.
const httpRetryAttempts = 5

// maxBackoffWait caps the exponential fallback used when a 429 or 5xx response carries no
// Retry-After, so a flaky server cannot turn one install into a multi-minute hang.
const maxBackoffWait = 5 * time.Second

// errDigestUnchanged signals that a re-download's digest matched the lock's recorded one. It is
// not a failure: Store.Update treats it as "nothing to do" and returns the existing entry with
// the stage removed and no build or swap ever attempted, so an unchanged https: source never
// touches the live checkout, per the contracts row's update semantics for a non-git source.
var errDigestUnchanged = errors.New("pluginstore: digest unchanged")

// httpTimeout bounds one entire download from an https: source, request through response body
// fully read. http.DefaultClient has no timeout at all, so a server that accepts the connection
// and then dribbles bytes (or none) would otherwise hang rudy install forever: the bounded
// retry-attempt count in doWithRetry only bounds how many requests are tried, not how long any
// one of them is allowed to run. Ten minutes is generous next to maxDownloadBytes even on a slow
// link, and short enough that an install that has actually stalled eventually fails instead of
// hanging the terminal it was run in.
const httpTimeout = 10 * time.Minute

// defaultHTTPClient is what stageHTTPS/stageHTTPSUpdate use in production (a test hands in its
// own via Store.HTTPClient). It exists, rather than reusing http.DefaultClient, for two
// hardening properties DefaultClient lacks: httpTimeout above, and CheckRedirect refusing a
// redirect to anything but https. Without the latter, an https: URL that redirects to a plain
// http one would have the "downloaded bytes" the digest is taken over be whatever that
// unencrypted hop actually served, silently downgrading the one guarantee installing from
// https: is supposed to carry.
var defaultHTTPClient = &http.Client{
	Timeout: httpTimeout,
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("pluginstore: refusing to follow a redirect to %s", req.URL)
		}
		return nil
	},
}

// httpClient is the *http.Client a stageHTTPS/stageHTTPSUpdate call uses: Store.HTTPClient when
// a caller (a test, with an httptest server's own client) set one, defaultHTTPClient otherwise.
// Same shape as Store.GoEnv: production always gets the real default, and a test hands in a
// fixture client instead of touching anything package-global.
func (s *Store) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return defaultHTTPClient
}

// stageHTTPS fills dir with an https: source's contents for a fresh install: download
// src.Location (a .tar.gz), hash the bytes as they stream in, unpack the tarball into dir, and
// return "sha256:<hex>" of what was downloaded. No caller of this file ever shells out to git
// or the go tool; the archive's bytes are the entire source of truth.
func stageHTTPS(ctx context.Context, src Source, dir string, client *http.Client) (string, error) {
	digest, tmp, err := downloadTarball(ctx, client, src.Location, dir)
	if err != nil {
		return "", err
	}
	// The download artifact lives inside dir (so an abandoned one ages and is swept with the
	// rest of the stage), which means it must be gone again before checkManifestAtRoot looks
	// at dir's own top-level entries, or it would count as one itself.
	unpackErr := unpackTarball(tmp, dir)
	_ = os.Remove(tmp)
	if unpackErr != nil {
		return "", unpackErr
	}
	if err := checkManifestAtRoot(dir); err != nil {
		return "", err
	}
	return digest, nil
}

// stageHTTPSUpdate is stageHTTPS for rudy plugins update: it re-downloads unconditionally (the
// contracts row is explicit that an update re-downloads rather than trusting anything cached),
// but when the fresh digest matches priorDigest it returns errDigestUnchanged without unpacking
// anything into dir, so Store.Update can leave the live checkout exactly as it was rather than
// stage, build and swap in a byte-identical copy.
func stageHTTPSUpdate(ctx context.Context, src Source, dir string, client *http.Client, priorDigest string) (string, error) {
	digest, tmp, err := downloadTarball(ctx, client, src.Location, dir)
	if err != nil {
		return "", err
	}
	if priorDigest != "" && digest == priorDigest {
		_ = os.Remove(tmp)
		return "", errDigestUnchanged
	}
	unpackErr := unpackTarball(tmp, dir)
	_ = os.Remove(tmp)
	if unpackErr != nil {
		return "", unpackErr
	}
	if err := checkManifestAtRoot(dir); err != nil {
		return "", err
	}
	return digest, nil
}

// downloadTarball fetches url (retrying a 429 or 5xx per doWithRetry) into a file inside dir
// (the caller's own stage, so an orphaned download left by a crash mid-transfer ages and gets
// swept the same way the rest of an abandoned stage does, rather than littering the OS temp
// directory), hashing the bytes with sha256 as they stream through. The digest is derived from
// h.Sum only after io.Copy has returned with no error: a short or corrupted transfer must never
// be recorded as though it were the digest of the file the operator asked for.
func downloadTarball(ctx context.Context, client *http.Client, url, dir string) (digest, tmpPath string, err error) {
	resp, err := doWithRetry(ctx, client, url)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// A random suffix, not a fixed name: a tar entry literally named ".download.tar.gz" would
	// otherwise get extracted (writeTarFile opens its target O_TRUNC) onto this very file while
	// unpackTarball is still reading it, corrupting the read mid-stream. The server sends its
	// response body before this file is created, so it cannot know the suffix os.CreateTemp
	// picks and cannot craft an entry to collide with it.
	f, err := os.CreateTemp(dir, ".download-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("pluginstore: %w", err)
	}
	tmpPath = f.Name()
	defer func() { _ = f.Close() }()

	h := sha256.New()
	// The limit is set one byte past the cap so a body landing exactly on it is not mistaken
	// for one that exceeds it, while a body that does exceed it is caught below rather than
	// silently truncated and hashed as if that were the whole download.
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownloadBytes+1))
	if copyErr != nil {
		return "", "", fmt.Errorf("pluginstore: download %s: %w", url, copyErr)
	}
	if n > maxDownloadBytes {
		return "", "", fmt.Errorf("pluginstore: download %s: exceeds the %d byte cap", url, maxDownloadBytes)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), tmpPath, nil
}

// doWithRetry issues one GET, retrying a 429 or 5xx response up to httpRetryAttempts times: the
// project's own rule on being a good client of an external service. Retry-After is honoured in
// both forms the HTTP spec allows (parsed by retryAfter); its absence falls back to an
// exponential backoff. A 4xx other than 429 is not retried at all and fails immediately, since
// retrying a request the server has already rejected as malformed or unauthorized wastes its
// time and the operator's for no chance of a different answer.
func doWithRetry(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	var lastErr error
	for attempt := range httpRetryAttempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("pluginstore: %w", err)
		}
		req.Header.Set("User-Agent", userAgent())
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("pluginstore: download %s: %w", url, err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("pluginstore: download %s: unexpected status %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
		}
		lastErr = fmt.Errorf("status %s", resp.Status)
		wait, ok := retryAfter(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()
		if attempt == httpRetryAttempts-1 {
			break
		}
		if !ok {
			wait = backoff(attempt)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("pluginstore: download %s: giving up after %d attempts: %s", url, httpRetryAttempts, lastErr)
}

// retryAfter parses a Retry-After header value in either form the HTTP spec allows: an integer
// count of seconds, or an HTTP date naming when to try again. ok is false when the header was
// absent or unparseable, telling the caller to fall back to its own exponential backoff instead
// of treating "couldn't parse it" the same as "server said wait zero seconds".
func retryAfter(v string) (wait time.Duration, ok bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// backoff is the exponential fallback doWithRetry uses when a 429 or 5xx response carries no
// Retry-After, capped at maxBackoffWait.
func backoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << attempt
	if d > maxBackoffWait {
		return maxBackoffWait
	}
	return d
}

// userAgent names rudy and its version, per the project's rule on being a good client of a
// service it does not run. It reads runtime/debug's build info rather than internal/cli's own
// version variable to avoid an import cycle: internal/cli already imports internal/pluginstore.
func userAgent() string {
	v := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		v = info.Main.Version
	}
	return "rudy/" + v
}

// tarballReader is one open gzip tarball: the file, the decompressor over it and the tar
// reader over that, closed together. unpackTarball opens the archive twice, once to survey it
// and once to write it out (see soleTopLevelDir), so this exists to keep the three-step open
// and the two closes in one place rather than twice over.
type tarballReader struct {
	f  *os.File
	gz *gzip.Reader
	tr *tar.Reader
}

func openTarball(path string) (*tarballReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pluginstore: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("pluginstore: %s: %w", path, err)
	}
	return &tarballReader{f: f, gz: gz, tr: tar.NewReader(gz)}, nil
}

func (t *tarballReader) Close() {
	_ = t.gz.Close()
	_ = t.f.Close()
}

// soleTopLevelDir reports the one top-level directory every entry of the tarball at path sits
// under, or "" when the entries already sit at the root, sit under more than one directory, or
// include a name the unpacking pass is going to refuse anyway. `git archive` and every GitHub
// release tarball wrap their contents in exactly one such directory, so stripping it is what
// makes the likeliest https: URL there is installable at all; any other nesting is still
// refused, by checkManifestAtRoot, naming what it found (contracts, plugins.lock.toml).
//
// This is a whole extra pass over the archive, decompression included, before a byte is
// written. Deciding while writing instead would mean either buffering entries until the answer
// is known or moving files up a level afterwards, and the rule is that nothing is written under
// the extra directory in the first place.
func soleTopLevelDir(path string) (string, error) {
	tb, err := openTarball(path)
	if err != nil {
		return "", err
	}
	defer tb.Close()

	prefix, nested := "", false
	for {
		hdr, err := tb.tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		clean, err := cleanTarName(hdr.Name)
		if err != nil {
			// An entry no name check will accept: say nothing about a prefix and let the
			// unpacking pass refuse it, so a refusal is worded in exactly one place.
			return "", nil
		}
		if clean == "." {
			continue
		}
		first, rest, _ := strings.Cut(clean, string(filepath.Separator))
		switch {
		case prefix == "":
			prefix = first
		case first != prefix:
			return "", nil
		}
		switch {
		case rest != "":
			nested = true
		case hdr.Typeflag != tar.TypeDir:
			// The one top-level entry is a file: there is nothing to strip, and stripping its
			// own name would drop the file itself.
			return "", nil
		}
	}
	if !nested {
		return "", nil
	}
	return prefix, nil
}

// stripTopLevel removes prefix, the archive's sole top-level directory, from one cleaned entry
// name. The prefix's own directory entry becomes ".", the stage itself, which the caller skips.
func stripTopLevel(rel, prefix string) string {
	if prefix == "" {
		return rel
	}
	if rel == prefix {
		return "."
	}
	return strings.TrimPrefix(rel, prefix+string(filepath.Separator))
}

// unpackTarball unpacks the gzip tarball at path into stage, which the caller has already
// created empty. Every entry is refused, and the whole install refused with it, unless its
// cleaned name resolves inside stage (cleanTarName: no "..", no absolute path, no backslash) and
// it is a regular file or a directory. Anything else — a symlink or a hard link most of all,
// since either one's target can point anywhere on disk the process can reach, escaping or not —
// is refused outright rather than validated case by case: a plugin bundle has no legitimate
// need for either, and refusing the whole type removes a class of link-resolution bugs from
// ever having to be gotten right against bytes an arbitrary https URL served. A PAX global
// header (typeflag 'g'), the entry git archive and GitHub's release tarballs emit ahead of the
// real content, names no file of the archive's own and is skipped rather than falling into the
// refusal for entry types this store does not extract.
//
// The total bytes actually written to disk are capped at maxUnpackedBytes, tracked as a
// shrinking budget rather than by counting bytes read off the gzip stream: a GNU or PAX sparse
// entry can declare a Size far larger than the archive bytes that back it (the reader fills the
// declared holes with zeros without consuming any archive bytes for them), so counting the
// compressed or even the raw tar-format bytes read never sees the true, expanded size at all. A
// 402 byte crafted archive can carry a sparse entry whose declared Size alone already exceeds
// the cap; io.Copy(w, tr) would otherwise still happily write all of it. Refusing whenever a
// single entry's declared Size exceeds what remains catches that up front, and wrapping the
// copy itself in io.LimitReader(tr, remaining) is the actual enforcement: it is what bounds
// bytes landing on disk regardless of what any entry's header claims or a reader's internal
// zero-fill synthesizes, and remaining is decremented by what writeTarFile reports was actually
// copied, not by hdr.Size.
func unpackTarball(path, stage string) error {
	prefix, err := soleTopLevelDir(path)
	if err != nil {
		return err
	}
	tb, err := openTarball(path)
	if err != nil {
		return err
	}
	defer tb.Close()

	remaining := int64(maxUnpackedBytes)
	for {
		hdr, err := tb.tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if hdr.Size < 0 || hdr.Size > remaining {
			return fmt.Errorf("pluginstore: %s: entry %q would exceed the %d byte unpacked cap", path, hdr.Name, maxUnpackedBytes)
		}
		rel, err := cleanTarName(hdr.Name)
		if err != nil {
			return fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		rel = stripTopLevel(rel, prefix)
		if rel == "." {
			continue
		}
		target, err := stageTarget(stage, rel)
		if err != nil {
			return fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("pluginstore: %w", err)
			}
		case tar.TypeReg:
			n, err := writeTarFile(target, io.LimitReader(tb.tr, remaining), hdr)
			remaining -= n
			if err != nil {
				return fmt.Errorf("pluginstore: %s: %w", path, err)
			}
		default:
			return fmt.Errorf("pluginstore: %s: entry %q is not a regular file or a directory; plugin tarballs may hold only those", path, hdr.Name)
		}
	}
	return nil
}

// writeTarFile extracts one regular-file entry to target, creating its parent directory (a tar
// stream is not required to list a directory entry before a file inside it) and preserving the
// entry's own permission bits so an executable like a build script stays executable. It reports
// how many bytes it actually wrote, which is what the caller's remaining unpacked-size budget is
// decremented by, rather than hdr.Size.
func writeTarFile(target string, r io.Reader, hdr *tar.Header) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	mode := os.FileMode(hdr.Mode & 0o777) //nolint:gosec // the tar header's own mode bits, masked to permissions only
	if mode == 0 {
		mode = 0o644
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(w, r)
	closeErr := w.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// cleanTarName resolves a tar entry's name to a stage-relative path, refusing anything that
// could land outside the stage. This is the security property of installing from an https:
// tarball: the bytes come from wherever the operator's URL happened to serve them from, and a
// hostile or merely careless archive can name an entry "../../evil" or "/etc/cron.d/whatever"
// to write outside the extraction directory. It runs before any top-level prefix is stripped,
// so a prefix can never be derived from a name that would have been refused.
func cleanTarName(name string) (string, error) {
	// "..\..\x" is one legal, backslash-containing filename to filepath.Clean on a unix build
	// (backslash is not a separator there), so it survives every check below unchanged; refuse
	// it outright rather than rely on this running only on unix, since this package's path
	// handling is the OS-provided filepath, not a unix-only one.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("entry %q contains a backslash", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", name)
	}
	return clean, nil
}

// stageTarget joins a cleaned, stage-relative entry name onto stage.
func stageTarget(stage, rel string) (string, error) {
	target := filepath.Join(stage, rel)
	// Defense in depth: cleanTarName should already make this impossible, but a stager touching
	// arbitrary paths on disk on attacker-controlled input is exactly the place to double-check
	// rather than trust one code path to have gotten it right.
	if target != stage && !strings.HasPrefix(target, stage+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", rel)
	}
	return target, nil
}

// checkManifestAtRoot refuses a tarball whose plugin.toml is not at the root of what was
// unpacked, naming what was found instead of letting the generic plugin.ReadManifest fail later
// with a bare "no such file" that does not say why. An archive whose every entry sits under one
// top-level directory has already had it stripped by then (soleTopLevelDir), so what reaches
// here is real nesting: two levels deep, or a bundle of several directories with no manifest
// among them.
func checkManifestAtRoot(stage string) error {
	if _, err := os.Stat(filepath.Join(stage, "plugin.toml")); err == nil {
		return nil
	}
	ents, err := os.ReadDir(stage)
	if err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	if len(ents) == 1 && ents[0].IsDir() {
		nested := filepath.Join(stage, ents[0].Name(), "plugin.toml")
		if _, err := os.Stat(nested); err == nil {
			return fmt.Errorf("pluginstore: found plugin.toml under top-level directory %q, want it at the tarball root", ents[0].Name())
		}
	}
	return fmt.Errorf("pluginstore: plugin.toml not found at the tarball root; found %s", entryList(ents))
}

// entryList names a stage's top-level entries for that error, directories marked with a
// trailing separator, capped so a large archive does not empty its whole root into one string.
func entryList(ents []os.DirEntry) string {
	if len(ents) == 0 {
		return "an empty archive"
	}
	const maxNamed = 8
	names := make([]string, 0, maxNamed)
	for _, e := range ents[:min(len(ents), maxNamed)] {
		name := e.Name()
		if e.IsDir() {
			name += string(filepath.Separator)
		}
		names = append(names, strconv.Quote(name))
	}
	out := strings.Join(names, ", ")
	if len(ents) > maxNamed {
		out += fmt.Sprintf(" and %d more", len(ents)-maxNamed)
	}
	return out
}
