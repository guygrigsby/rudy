package pluginstore

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Kind is the vocabulary of where an installed plugin's source lives (ADR 0025 decision 1):
// go modules, git, a raw https download, or a local filesystem path. There is no rudy
// package registry; these four are what git, the Go module proxy and a plain download
// already answer.
type Kind string

const (
	KindGit   Kind = "git"
	KindPath  Kind = "path"
	KindGo    Kind = "go"
	KindHTTPS Kind = "https"
)

// Source is one rudy install argument, classified. Location is what a stager actually
// clones, fetches or copies: an https:// URL, git's scp-like shorthand, a Go module path, or
// an absolute filesystem path. Ref is the part after "@", exactly as typed, empty when the
// operator gave none; a stager resolves it to a commit or a digest but never rewrites Ref
// itself. AsTyped is what the lock's source column records: the original argument verbatim
// for every kind except path, which has always been stored as an absolute path (never the
// relative string typed) since Update has no reason to run from the working directory
// Install did.
type Source struct {
	Kind     Kind
	Location string
	Ref      string
	AsTyped  string
}

// ParseSource classifies one rudy install argument into a Source.
//
// "go:module/path@version" and "git:host/user/repo@ref" carry an explicit kind. git:'s
// remainder is rewritten to an https:// URL only when it looks like a bare host/path
// shorthand: no "://" already in it, and not itself a filesystem path (leading "/", "."  or
// "~"). That keeps a local checkout usable as "git:/abs/path@ref" in a test or a bind mount
// without git: turning it into "https:///abs/path".
//
// With no explicit prefix: git's own scp-like shorthand (user@host:path, or host:path with
// no "user@") and any explicit scheme (https://, git://, ssh://, ...) are both remote,
// mirroring git's own transport detection. The one deliberate ambiguity is https://: a URL
// meant a git remote before this vocabulary existed, so that stays the default. Only a URL
// ending in ".tar.gz" or ".tgz" reads as the https kind (a tarball download); one ending in
// ".git", or anything else, reads as git. Neither form takes an "@ref" suffix: pinning by
// ref is a git: prefix feature.
//
// Everything left is a filesystem path, made absolute since a relative one is meaningless
// once Update runs from a different cwd.
func ParseSource(s string) (Source, error) {
	switch {
	case strings.HasPrefix(s, "go:"):
		return parseGoSource(s)
	case strings.HasPrefix(s, "git:"):
		return parseGitPrefixed(s)
	}
	if sourceSchemeRe.MatchString(s) {
		return Source{Kind: classifyScheme(s), Location: s, AsTyped: s}, nil
	}
	if isRemoteSource(s) {
		return Source{Kind: KindGit, Location: s, AsTyped: s}, nil
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return Source{}, fmt.Errorf("pluginstore: %s: %w", s, err)
	}
	return Source{Kind: KindPath, Location: abs, AsTyped: abs}, nil
}

// classifyScheme is the https:// ambiguity call, for a source that already carries an
// explicit scheme. Every scheme but https names a git transport and nothing else; https
// names git unless the path ends in the one or two extensions a tarball download uses.
func classifyScheme(s string) Kind {
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "https://") {
		return KindGit
	}
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		return KindHTTPS
	}
	return KindGit
}

// parseGoSource handles "go:module/path@version". A missing version resolves to "latest",
// mirroring go install's own default.
func parseGoSource(s string) (Source, error) {
	rest := strings.TrimPrefix(s, "go:")
	loc, ref := splitRef(rest)
	if loc == "" {
		return Source{}, fmt.Errorf("pluginstore: %s: empty go module path", s)
	}
	if ref == "" {
		ref = "latest"
	}
	return Source{Kind: KindGo, Location: loc, Ref: ref, AsTyped: s}, nil
}

// parseGitPrefixed handles "git:host/user/repo@ref" and the local-path spelling
// "git:/abs/or/./rel/path@ref" a test fixture or a bind-mounted checkout wants.
func parseGitPrefixed(s string) (Source, error) {
	rest := strings.TrimPrefix(s, "git:")
	loc, ref := splitRef(rest)
	if loc == "" {
		return Source{}, fmt.Errorf("pluginstore: %s: empty git source", s)
	}
	if !strings.Contains(loc, "://") &&
		!strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, ".") && !strings.HasPrefix(loc, "~") {
		loc = "https://" + loc
	}
	return Source{Kind: KindGit, Location: loc, Ref: ref, AsTyped: s}, nil
}

// splitRef splits "location@ref" on the last "@", the one a version, tag or commit follows.
// A string with no "@" has no ref.
func splitRef(s string) (loc, ref string) {
	i := strings.LastIndexByte(s, '@')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}
