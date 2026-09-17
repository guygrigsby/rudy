// SPDX-License-Identifier: AGPL-3.0-or-later

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

	"github.com/guygrigsby/rudy/internal/tarx"
)

// maxDownloadBytes bounds how much of an https: tarball stageHTTPS will read at all. The
// bytes come from wherever the operator's URL happens to serve them from; refusing a body past
// this cap, rather than reading to exhaustion, is what keeps a misbehaving or hostile server
// from exhausting disk on an install nobody watches to completion. 256 MiB is generous next to
// any plugin bundle actually meant to be checked out (source plus maybe a small binary).
const maxDownloadBytes = 256 << 20

// maxTarEntries bounds how many entries scanTarball will walk before refusing the archive,
// the same figure tarx.Unpack caps the unpacking pass at: an empty directory or file costs
// nothing against a budget of bytes written and gzips to almost nothing, so the download cap
// alone leaves the entry count bounded only at roughly a million per 5 MiB, each one a MkdirAll
// or an OpenFile and an inode (rudy-79q). Twenty thousand is far more than a plugin bundle
// needs (rudy's own repository is under four hundred files) and refuses the pathological
// archive here, before a byte of it is written.
const maxTarEntries = tarx.MaxEntries

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
// and once to write it out (see scanTarball), so this exists to keep the three-step open
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

// scanTarball reads the whole archive at path once before anything is written, and answers the
// two questions that have to be settled before the first write: is there a sole top-level
// directory to strip, and are there more entries than this store will extract at all.
//
// The prefix is "" when the entries already sit at the root, sit under more than one directory,
// or include a name the unpacking pass is going to refuse anyway. `git archive` and every
// GitHub release tarball wrap their contents in exactly one such directory, so stripping it is
// what makes the likeliest https: URL there is installable at all; any other nesting is still
// refused, by checkManifestAtRoot, naming what it found (contracts, plugins.lock.toml).
//
// This is a whole extra pass over the archive, decompression included. Deciding while writing
// instead would mean either buffering entries until the answer is known or moving files up a
// level afterwards, and the rule is that nothing is written under the extra directory in the
// first place; the entry cap gets the same benefit, refusing before the first MkdirAll rather
// than partway through a million of them.
func scanTarball(path string) (string, error) {
	tb, err := openTarball(path)
	if err != nil {
		return "", err
	}
	defer tb.Close()

	// rooted is set by anything that rules stripping out, and the loop then runs on rather than
	// returning: the entry count is only trustworthy if every entry is actually reached.
	prefix, nested, rooted, entries := "", false, false, 0
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
		entries++
		if entries > maxTarEntries {
			return "", fmt.Errorf("pluginstore: %s: more than %d entries; a plugin bundle is smaller than that", path, maxTarEntries)
		}
		clean, err := tarx.Clean(hdr.Name)
		if err != nil {
			// An entry no name check will accept: say nothing about a prefix and let the
			// unpacking pass refuse it, so a refusal is worded in exactly one place.
			rooted = true
			continue
		}
		if clean == "." {
			continue
		}
		first, rest, _ := strings.Cut(clean, string(filepath.Separator))
		switch {
		case prefix == "":
			prefix = first
		case first != prefix:
			rooted = true
		}
		if rest == "" && hdr.Typeflag != tar.TypeDir {
			// A top-level entry that is a file: stripping its own name would drop the file.
			rooted = true
		}
		if rest != "" {
			nested = true
		}
	}
	if rooted || !nested {
		return "", nil
	}
	return prefix, nil
}

// unpackTarball unpacks the gzip tarball at path into stage, which the caller has already
// created empty. The rules are tarx's, shared with the sync that unpacks a tar a host wrote:
// a name that could land outside stage, an entry type that names anything on disk (a symlink
// or a hard link most of all), more entries than the cap or more bytes than the cap is a
// refusal, and the whole install is refused with it. What is this package's own is the prefix:
// `git archive` and every GitHub release tarball wrap their contents in exactly one top-level
// directory, and scanTarball is the pass that decides whether there is one to strip.
func unpackTarball(path, stage string) error {
	prefix, err := scanTarball(path)
	if err != nil {
		return err
	}
	tb, err := openTarball(path)
	if err != nil {
		return err
	}
	defer tb.Close()
	if err := tarx.Unpack(tb.tr, stage, tarx.Options{Strip: prefix}); err != nil {
		return fmt.Errorf("pluginstore: %s: %w", path, err)
	}
	return nil
}

// checkManifestAtRoot refuses a tarball whose plugin.toml is not at the root of what was
// unpacked, naming what was found instead of letting the generic plugin.ReadManifest fail later
// with a bare "no such file" that does not say why. An archive whose every entry sits under one
// top-level directory has already had it stripped by then (scanTarball), so what reaches
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
