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

// httpClient is the *http.Client a stageHTTPS/stageHTTPSUpdate call uses: Store.HTTPClient when
// a caller (a test, with an httptest server's own client) set one, http.DefaultClient
// otherwise. Same shape as Store.GoEnv: production always gets the real default, and a test
// hands in a fixture client instead of touching anything package-global.
func (s *Store) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
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

	tmpPath = filepath.Join(dir, ".download.tar.gz")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("pluginstore: %w", err)
	}
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

// unpackTarball unpacks the gzip tarball at path into stage, which the caller has already
// created empty. Every entry is refused, and the whole install refused with it, unless its
// cleaned name resolves inside stage (safeTarPath: no "..", no absolute path) and it is a
// regular file or a directory. Anything else — a symlink or a hard link most of all, since
// either one's target can point anywhere on disk the process can reach, escaping or not — is
// refused outright rather than validated case by case: a plugin bundle has no legitimate need
// for either, and refusing the whole type removes a class of link-resolution bugs from ever
// having to be gotten right against bytes an arbitrary https URL served. The decompressed
// stream is capped independently of the download size (maxUnpackedBytes) against a gzip bomb.
func unpackTarball(path, stage string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("pluginstore: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("pluginstore: %s: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(&cappedReader{r: gz, limit: maxUnpackedBytes})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		target, err := safeTarPath(stage, hdr.Name)
		if err != nil {
			return fmt.Errorf("pluginstore: %s: %w", path, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("pluginstore: %w", err)
			}
		case tar.TypeReg:
			if err := writeTarFile(target, tr, hdr); err != nil {
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
// entry's own permission bits so an executable like a build script stays executable.
func writeTarFile(target string, r io.Reader, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(hdr.Mode & 0o777) //nolint:gosec // the tar header's own mode bits, masked to permissions only
	if mode == 0 {
		mode = 0o644
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(w, r)
	closeErr := w.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// cappedReader wraps r, refusing to read past limit total bytes with a clear error rather than
// truncating silently: truncation would surface later as a confusing tar-format error instead
// of naming the actual reason, an unpacked size that exceeded the cap.
type cappedReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, fmt.Errorf("unpacked size exceeds the %d byte cap", c.limit)
	}
	return n, err
}

// safeTarPath resolves a tar entry's name to a path inside stage, refusing anything that could
// land outside it. This is the security property of installing from an https: tarball: the
// bytes come from wherever the operator's URL happened to serve them from, and a hostile or
// merely careless archive can name an entry "../../evil" or "/etc/cron.d/whatever" to write
// outside the extraction directory.
func safeTarPath(stage, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", name)
	}
	target := filepath.Join(stage, clean)
	// Defense in depth: the checks above should already make this impossible, but a stager
	// touching arbitrary paths on disk on attacker-controlled input is exactly the place to
	// double-check rather than trust one code path to have gotten it right.
	if target != stage && !strings.HasPrefix(target, stage+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the stage", name)
	}
	return target, nil
}

// checkManifestAtRoot refuses a tarball whose plugin.toml is not directly at its root, naming
// what was found instead of letting the generic plugin.ReadManifest fail later with a bare "no
// such file" that does not say why. The likely cause is `tar czf plugin.tar.gz plugin-dir/`,
// whose every entry sits under one top-level directory: an easy mistake for a plugin author to
// make, and one worth naming rather than leaving them to guess at.
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
	return fmt.Errorf("pluginstore: plugin.toml not found at the tarball root")
}
