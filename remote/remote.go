// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package remote is the one interface a playground uses to read a project from
// a git forge and to propose changes back to it.
//
// A proposal is always a NEW branch plus a pull (merge) request. Nothing in this
// package or its implementations writes to an existing branch: the branch name
// is validated, checked absent, and created without force. The default branch,
// and the branch the work was loaded from, are refused outright.
//
// Two families implement [Remote]: package forge talks to the GitHub, GitLab and
// Gitea/Forgejo REST APIs; [Git] speaks git smart HTTP through
// [github.com/go-browserhttp/browsergit], for forges reachable only that way.
package remote

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Files maps a slash-separated path, relative to the loaded directory, to its
// content.
type Files map[string][]byte

// Base records where a set of files came from: enough to propose changes on top
// of it and to ask whether it moved. It holds no credential and is safe to store
// with a saved version.
type Base struct {
	// Forge names the implementation ("github", "gitlab", "gitea", "git").
	Forge string `json:"forge"`
	// Repo is "owner/name" (GitLab: "group/sub/name").
	Repo string `json:"repo"`
	// Ref is the branch the files were loaded from; proposals target it.
	Ref string `json:"ref"`
	// Dir is the repository subdirectory the files are relative to ("" = root).
	Dir string `json:"dir,omitempty"`
	// Commit is the full commit id of Ref when loaded.
	Commit string `json:"commit"`
	// Blobs maps each loaded path (relative to Dir) to its git blob id.
	Blobs map[string]string `json:"blobs,omitempty"`
}

// Proposal describes the change to propose.
type Proposal struct {
	// Branch is the new branch to create. It must not exist.
	Branch string
	// Title is the commit subject and the pull request title.
	Title string
	// Body is the pull request description, also the commit body.
	Body string
}

// Result reports a successful proposal.
type Result struct {
	Branch string
	Commit string
	// URL is the pull request page, or for [Git] the forge's compare page when a
	// web base was configured (empty otherwise). Always http(s).
	URL string
}

// Remote loads files from a forge and proposes changes back.
type Remote interface {
	// Load reads every regular file under dir at the tip of ref (the default
	// branch when ref is empty).
	Load(ctx context.Context, repo, ref, dir string) (Files, Base, error)
	// Propose commits files on top of base.Commit as a new branch and opens a
	// pull request against base.Ref. Paths absent from files but present in
	// base.Blobs are deleted.
	Propose(ctx context.Context, base Base, files Files, p Proposal) (Result, error)
	// Refresh reports the files at the current tip of base.Ref. When the tip is
	// still base.Commit it returns nil files and base unchanged.
	Refresh(ctx context.Context, base Base) (Files, Base, error)
}

// Error classes shared by every implementation. Each failure wraps exactly one.
var (
	ErrAuth          = errors.New("remote: authentication failed")
	ErrForbidden     = errors.New("remote: not permitted")
	ErrNotFound      = errors.New("remote: not found")
	ErrBranchExists  = errors.New("remote: branch already exists")
	ErrBadBranch     = errors.New("remote: branch name refused")
	ErrBadPath       = errors.New("remote: path refused")
	ErrBadRepo       = errors.New("remote: repository name refused")
	ErrTooLarge      = errors.New("remote: over a size limit")
	ErrStale         = errors.New("remote: the branch moved since it was loaded")
	ErrNoChanges     = errors.New("remote: nothing to propose")
	ErrConflict      = errors.New("remote: the forge refused the change")
	ErrRateLimited   = errors.New("remote: rate limited")
	ErrTransport     = errors.New("remote: transport error")
	ErrBadResponse   = errors.New("remote: malformed response")
	ErrNotConfigured = errors.New("remote: not configured")
)

// Limits bounds what a Load may bring into memory and what a Propose may send.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// DefaultLimits suit a slide deck or a paper with its figures.
var DefaultLimits = Limits{MaxFiles: 2000, MaxFileBytes: 20 << 20, MaxTotalBytes: 100 << 20}

// OrDefault fills zero fields from DefaultLimits.
func (l Limits) OrDefault() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultLimits.MaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultLimits.MaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultLimits.MaxTotalBytes
	}
	return l
}

// CheckFiles applies l to a whole set and every path in it.
func (l Limits) CheckFiles(files Files) error {
	l = l.OrDefault()
	if len(files) > l.MaxFiles {
		return fmt.Errorf("%w: %d files, limit %d", ErrTooLarge, len(files), l.MaxFiles)
	}
	var total int64
	for p, b := range files {
		if err := CheckPath(p); err != nil {
			return err
		}
		if int64(len(b)) > l.MaxFileBytes {
			return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrTooLarge, p, len(b), l.MaxFileBytes)
		}
		total += int64(len(b))
		if total > l.MaxTotalBytes {
			return fmt.Errorf("%w: more than %d bytes in all", ErrTooLarge, l.MaxTotalBytes)
		}
	}
	return nil
}

// MaxPathLen bounds one path.
const MaxPathLen = 1024

// CheckPath accepts a clean, relative, slash-separated path that cannot leave
// its directory or touch git's control directory. Paths come from a server we
// do not trust (Load) and from a deck we do not trust (Propose).
func CheckPath(p string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q: %s", ErrBadPath, p, why) }
	switch {
	case p == "":
		return bad("empty")
	case len(p) > MaxPathLen:
		return bad("too long")
	case !utf8.ValidString(p):
		return bad("not UTF-8")
	case strings.HasPrefix(p, "/"):
		return bad("absolute")
	case strings.ContainsRune(p, '\\'):
		return bad("backslash")
	case path.Clean(p) != p:
		return bad("not clean")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return bad("control character")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return bad("dot segment")
		}
		if strings.EqualFold(strings.TrimRight(seg, ". "), ".git") {
			return bad("git control directory")
		}
	}
	return nil
}

// CheckDir accepts "" (the root) or a path CheckPath accepts.
func CheckDir(dir string) error {
	if dir == "" {
		return nil
	}
	return CheckPath(dir)
}

var repoSeg = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// CheckRepo accepts "owner/name", or with nested=true "group/sub/.../name" (at
// most 20 segments). Segments are the characters forges allow; nothing in a
// repository name can then change the URL it is placed into.
func CheckRepo(repo string, nested bool) error {
	segs := strings.Split(repo, "/")
	if len(segs) < 2 || (!nested && len(segs) != 2) || len(segs) > 20 {
		return fmt.Errorf("%w: %q", ErrBadRepo, repo)
	}
	for _, s := range segs {
		if !repoSeg.MatchString(s) || strings.Contains(s, "..") {
			return fmt.Errorf("%w: %q", ErrBadRepo, repo)
		}
	}
	return nil
}

// MaxBranchLen bounds a branch name (Gitea refuses more than 100).
const MaxBranchLen = 100

// CheckBranch applies git-check-ref-format to refs/heads/<name>, plus the
// --branch rule (no leading '-'), "HEAD", and a length bound; then refuses the
// branches a proposal must never write to.
func CheckBranch(name string, refused ...string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q: %s", ErrBadBranch, name, why) }
	if name == "" || len(name) > MaxBranchLen {
		return bad("empty or too long")
	}
	if name == "HEAD" || strings.HasPrefix(name, "-") {
		return bad("reserved")
	}
	// Rule 6: no leading/trailing slash, no "//".
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return bad("slash")
	}
	// Rules 3, 7, 8, 9, 10.
	if strings.Contains(name, "..") || strings.HasSuffix(name, ".") || strings.Contains(name, "@{") || name == "@" || strings.ContainsRune(name, '\\') {
		return bad("forbidden sequence")
	}
	// Rules 4 and 5, and nothing outside printable ASCII: a branch name ends up
	// in URLs and on screens, where look-alike characters deceive.
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("~^:?*[", r) {
			return bad("forbidden character")
		}
	}
	// Rule 1.
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return bad("component")
		}
	}
	for _, r := range refused {
		if r != "" && strings.EqualFold(name, r) {
			return bad("protected target")
		}
	}
	return nil
}

// RequirePrefix refuses a branch outside prefix ("slides/" or "slides/<user>/").
func RequirePrefix(name, prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return fmt.Errorf("%w: %q is outside %q", ErrBadBranch, name, prefix)
	}
	return nil
}

// BranchName builds "<prefix><slug>-<yyyymmdd-hhmmss>" from a free-text title.
// The result always passes CheckBranch.
func BranchName(prefix, title string, now time.Time) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		ok := r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "change"
	}
	return prefix + slug + "-" + now.UTC().Format("20060102-150405")
}

// BlobID is git's object id for a blob: SHA-1 over "blob <len>\x00<data>".
func BlobID(data []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Op is what a Change does.
type Op int

const (
	// Create adds a path absent from the base.
	Create Op = iota + 1
	// Update replaces a path present in the base.
	Update
	// Delete removes a path present in the base.
	Delete
)

func (o Op) String() string {
	switch o {
	case Create:
		return "create"
	case Update:
		return "update"
	case Delete:
		return "delete"
	}
	return "op(" + strconv.Itoa(int(o)) + ")"
}

// Change is one file-level difference from the base.
type Change struct {
	Path    string // relative to the repository root (Dir already joined)
	Op      Op
	Data    []byte // nil for Delete
	OldBlob string // blob id in the base, "" for Create
}

// Changes compares files with base.Blobs and returns the differences in path
// order, with repository-root paths. It validates every path and the limits.
func Changes(base Base, files Files, lim Limits) ([]Change, error) {
	if err := lim.CheckFiles(files); err != nil {
		return nil, err
	}
	if err := CheckDir(base.Dir); err != nil {
		return nil, err
	}
	var out []Change
	for p, data := range files {
		old, had := base.Blobs[p]
		if had && old == BlobID(data) {
			continue
		}
		c := Change{Path: Join(base.Dir, p), Data: data, OldBlob: old, Op: Create}
		if had {
			c.Op = Update
		}
		out = append(out, c)
	}
	for p, old := range base.Blobs {
		if _, keep := files[p]; keep {
			continue
		}
		if err := CheckPath(p); err != nil {
			return nil, err
		}
		out = append(out, Change{Path: Join(base.Dir, p), Op: Delete, OldBlob: old})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Join joins a directory and a relative path.
func Join(dir, p string) string {
	if dir == "" {
		return p
	}
	return dir + "/" + p
}

// Rel returns p relative to dir and whether p lies under it.
func Rel(dir, p string) (string, bool) {
	if dir == "" {
		return p, true
	}
	if strings.HasPrefix(p, dir+"/") {
		return p[len(dir)+1:], true
	}
	return "", false
}

// CheckProposal validates a proposal against its base before any network call.
// refused lists further branch names to refuse (the default branch).
func CheckProposal(base Base, p Proposal, refused ...string) error {
	if err := CheckBranch(p.Branch, append([]string{base.Ref}, refused...)...); err != nil {
		return err
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("%w: empty title", ErrConflict)
	}
	if !isCommitID(base.Commit) {
		return fmt.Errorf("%w: base commit %q", ErrBadResponse, base.Commit)
	}
	return nil
}

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

func isCommitID(s string) bool { return commitID.MatchString(s) }

// IsCommitID reports whether s is a full SHA-1 or SHA-256 object id.
func IsCommitID(s string) bool { return isCommitID(s) }

// Message is the commit message for a proposal.
func Message(p Proposal) string {
	if strings.TrimSpace(p.Body) == "" {
		return p.Title
	}
	return p.Title + "\n\n" + p.Body
}
