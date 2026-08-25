// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package browsergit is an in-browser (Go/wasm) remote-git client for the
// go-tex playground. It wraps github.com/go-git/go-git/v5 driven entirely
// from memory: a memory object store plus a memfs working tree, talking
// smart-HTTP to a CORS-enabled origin over the browser Fetch API.
//
// The operation set + config mirror the loom server's on-disk git surface
// (internal/server/api_git.go): RemoteName "origin", default branch
// "main", PAT via HTTP BasicAuth, tolerate "already up-to-date", and a
// signed commit signature. The difference is storage — loom uses
// PlainClone against osfs; the browser client uses memory + memfs so no
// host filesystem is touched.
//
// # js/wasm transport constraint (proven by the feasibility study)
//
// Under GOOS=js GOARCH=wasm, net/http's RoundTripper is the WHATWG Fetch
// implementation, which is NOT an *http.Transport. go-git falls back to
// reaching into Transport.(*http.Transport) whenever a CloneOptions /
// PushOptions field forces a custom transport (InsecureSkipTLS, Proxy,
// CABundle, ClientCert, ClientKey). That type assertion fails in the
// browser. This package therefore keeps the go-git option structs
// MINIMAL — only URL, Auth, Depth, SingleBranch, ReferenceName — so
// go-git uses http.DefaultClient and the request rides the Fetch
// RoundTripper. Do not add transport-shaping fields here.
//
// SSRF validation (loom's validateRemoteURL) deliberately does NOT live
// here: in the browser topology it belongs in the sovereign proxy the
// client talks through, not in client-side code the user controls.
package browsergit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// RemoteName is the single remote every clone wires up, matching loom.
const RemoteName = "origin"

// DefaultBranch is used whenever the caller passes an empty branch.
const DefaultBranch = "main"

// Sentinel error classes. Every operation wraps its underlying go-git
// failure in exactly one of these so the UI (later phase) can branch on
// errors.Is without string-matching go-git internals.
var (
	// ErrAuth is a credential failure: the origin rejected the token
	// (HTTP 401/403). Distinct from ErrTransport so the UI can prompt
	// for a fresh PAT rather than a "check your network" banner.
	ErrAuth = errors.New("browsergit: authentication failed")

	// ErrNonFastForward is a rejected push: the remote branch moved on
	// and the local history is not a fast-forward. The UI should offer
	// pull-then-retry, never a silent force.
	ErrNonFastForward = errors.New("browsergit: non-fast-forward push rejected")

	// ErrRepoNotFound is a missing / unexported repository at the origin.
	ErrRepoNotFound = errors.New("browsergit: repository not found")

	// ErrTransport is any other origin/network/CORS failure. In the
	// browser a blocked CORS preflight, a refused connection, or a TLS
	// error all land here.
	ErrTransport = errors.New("browsergit: transport error")

	// ErrNotExist is returned by ReadFile when the path is absent from
	// the in-memory working tree.
	ErrNotExist = errors.New("browsergit: file does not exist")
)

// Options configures a Client. BaseURL is either a Forgejo base
// ("https://forge.example.org") or a sovereign-proxy prefix that already
// carries the upstream host as a path segment
// ("https://proxy.example.org/github.com"). Clone appends the caller's
// repoPath to it.
type Options struct {
	BaseURL  string
	Token    string
	Author   string
	Email    string
	Provider string // "github" | "gitlab" | "forgejo" | "generic"
}

// Client is a stateless factory bound to one origin + identity. It holds
// no mutable state, so a single Client can mint many Repos.
type Client struct {
	opts Options
}

// New returns a Client for the given options. BaseURL is normalised once
// (trailing slashes trimmed) so repoURL joins are clean.
func New(opts Options) *Client {
	opts.BaseURL = strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	return &Client{opts: opts}
}

// authUsername is the BasicAuth username for the configured provider.
// Every provider accepts PAT-as-password; only the username differs.
// GitLab is picky and wants "oauth2" for api-scoped PATs; everyone else
// takes the conventional "git" placeholder.
func (c *Client) authUsername() string {
	if strings.EqualFold(c.opts.Provider, "gitlab") {
		return "oauth2"
	}
	return "git"
}

// auth builds the go-git AuthMethod, or nil when no token is set (an
// anonymous clone of a public repo).
func (c *Client) auth() transport.AuthMethod {
	if c.opts.Token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: c.authUsername(), Password: c.opts.Token}
}

// repoURL joins BaseURL and repoPath into the smart-HTTP endpoint URL.
// repoPath is taken as-is apart from trimming surrounding slashes, so a
// caller may pass "owner/repo" or "owner/repo.git".
func (c *Client) repoURL(repoPath string) string {
	rp := strings.Trim(strings.TrimSpace(repoPath), "/")
	if c.opts.BaseURL == "" {
		return rp
	}
	if rp == "" {
		return c.opts.BaseURL
	}
	return c.opts.BaseURL + "/" + rp
}

// Repo is a cloned repository living entirely in memory: a memory object
// store plus a memfs working tree. It is not safe for concurrent use —
// go-git worktree mutation isn't — so serialise calls per Repo.
type Repo struct {
	client *Client
	repo   *git.Repository
	fs     billy.Filesystem
	branch string
}

// Clone shallow-clones repoPath at branch into a fresh in-memory repo.
// A depth <= 0 collapses to 1: the browser client only ever wants the
// tip. The go-git option set is kept minimal on purpose (see package
// doc) so the request rides the Fetch RoundTripper under js/wasm.
func (c *Client) Clone(ctx context.Context, repoPath, branch string, depth int) (*Repo, error) {
	if branch == "" {
		branch = DefaultBranch
	}
	if depth <= 0 {
		depth = 1
	}
	storer := memory.NewStorage()
	wtfs := memfs.New()

	repo, err := git.CloneContext(ctx, storer, wtfs, &git.CloneOptions{
		URL:           c.repoURL(repoPath),
		Auth:          c.auth(),
		Depth:         depth,
		SingleBranch:  true,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
	})
	if err != nil {
		return nil, classifyErr("clone", err)
	}
	return &Repo{client: c, repo: repo, fs: wtfs, branch: branch}, nil
}

// Branch reports the branch this repo tracks.
func (r *Repo) Branch() string { return r.branch }

// ReadFile returns the bytes of path from the in-memory working tree.
// A missing path is reported as ErrNotExist.
func (r *Repo) ReadFile(p string) ([]byte, error) {
	data, err := billyutil.ReadFile(r.fs, cleanPath(p))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, p)
		}
		return nil, fmt.Errorf("browsergit: read %s: %w", p, err)
	}
	return data, nil
}

// WriteFile writes data to path in the in-memory working tree, creating
// parent directories as needed. It does not stage or commit — call
// Commit for that.
func (r *Repo) WriteFile(p string, data []byte) error {
	if err := billyutil.WriteFile(r.fs, cleanPath(p), data, 0o644); err != nil {
		return fmt.Errorf("browsergit: write %s: %w", p, err)
	}
	return nil
}

// List walks the in-memory working tree and returns every regular file's
// slash-relative path, sorted, with the .git control directory pruned. The
// UI uses it to enumerate a freshly cloned repo (e.g. to find the .tex files
// to open); it never touches a host filesystem, since the working tree is a
// memfs.
func (r *Repo) List() ([]string, error) {
	var out []string
	err := billyutil.Walk(r.fs, "/", func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		out = append(out, cleanPath(p))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("browsergit: list: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// Change is one dirty entry in the working tree.
type Change struct {
	Path   string
	Status string // "untracked" | "modified" | "deleted" | "staged"
}

// Status mirrors loom's gitStatus shape: the dirty-file list plus the
// ahead/behind divergence against the remote-tracking branch.
type Status struct {
	Branch  string
	Ahead   int
	Behind  int
	Changes []Change
	Clean   bool
}

// Status reads the working tree and the remote-tracking ref to report
// the dirty-file list and ahead/behind counts. Ahead/behind is
// best-effort: a shallow (Depth:1) clone often lacks the history to
// compute divergence, in which case both are zero (never an error).
func (r *Repo) Status() (Status, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return Status{}, fmt.Errorf("browsergit: worktree: %w", err)
	}
	st, err := wt.Status()
	if err != nil {
		return Status{}, fmt.Errorf("browsergit: status: %w", err)
	}
	changes := make([]Change, 0, len(st))
	for p, fileStat := range st {
		changes = append(changes, Change{Path: p, Status: classify(fileStat.Worktree, fileStat.Staging)})
	}
	ahead, behind := r.aheadBehind()
	return Status{
		Branch:  r.branch,
		Ahead:   ahead,
		Behind:  behind,
		Changes: changes,
		Clean:   st.IsClean(),
	}, nil
}

// classify maps go-git's two-char status to a single UI label. Ported
// verbatim from loom's classify so both surfaces badge identically.
func classify(work, stage git.StatusCode) string {
	switch {
	case work == git.Untracked:
		return "untracked"
	case work == git.Deleted, stage == git.Deleted:
		return "deleted"
	case work == git.Modified, stage == git.Modified:
		return "modified"
	case stage == git.Added, stage == git.Renamed, stage == git.Copied:
		return "staged"
	}
	return "modified"
}

// aheadBehind computes divergence between the local branch tip and its
// origin remote-tracking ref. Best-effort: any missing ref or shallow
// history yields (0, 0).
func (r *Repo) aheadBehind() (int, int) {
	local, err := r.repo.Reference(plumbing.NewBranchReferenceName(r.branch), true)
	if err != nil {
		return 0, 0
	}
	remote, err := r.repo.Reference(plumbing.NewRemoteReferenceName(RemoteName, r.branch), true)
	if err != nil {
		return 0, 0
	}
	if local.Hash() == remote.Hash() {
		return 0, 0
	}
	base, err := r.mergeBase(local.Hash(), remote.Hash())
	if err != nil {
		return 0, 0
	}
	ahead := r.countSince(local.Hash(), base)
	behind := r.countSince(remote.Hash(), base)
	return ahead, behind
}

func (r *Repo) mergeBase(a, b plumbing.Hash) (plumbing.Hash, error) {
	ca, err := r.repo.CommitObject(a)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	cb, err := r.repo.CommitObject(b)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	bases, err := ca.MergeBase(cb)
	if err != nil || len(bases) == 0 {
		return plumbing.ZeroHash, errors.New("no merge base")
	}
	return bases[0].Hash, nil
}

// errStopWalk halts a commit walk once the merge base is reached.
var errStopWalk = errors.New("stop")

func (r *Repo) countSince(head, base plumbing.Hash) int {
	iter, err := r.repo.Log(&git.LogOptions{From: head})
	if err != nil {
		return 0
	}
	defer iter.Close()
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == base {
			return errStopWalk
		}
		count++
		return nil
	})
	return count
}

// Commit stages every tracked + untracked change (git add -A) and commits
// with the configured identity. It returns the new commit hash. When the
// tree is clean it returns an empty hash and no error, matching loom's
// no-op-on-clean auto-commit.
func (r *Repo) Commit(message string) (string, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("browsergit: worktree: %w", err)
	}
	st, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("browsergit: status: %w", err)
	}
	if st.IsClean() {
		return "", nil
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", fmt.Errorf("browsergit: add: %w", err)
	}
	name, email := r.client.signature()
	h, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: name, Email: email, When: time.Now()},
	})
	if err != nil {
		return "", fmt.Errorf("browsergit: commit: %w", err)
	}
	return h.String(), nil
}

// Stage stages every tracked + untracked change (git add -A) WITHOUT committing,
// so a following Status reports the staged entries. It is the "git add" half of
// Commit, split out so the UI can offer a distinct Stage action; a clean tree is
// a no-op (returns nil). AddWithOptions{All:true} on an unchanged tree does
// nothing and never errors.
func (r *Repo) Stage() error {
	wt, err := r.repo.Worktree()
	if err != nil {
		return fmt.Errorf("browsergit: worktree: %w", err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("browsergit: add: %w", err)
	}
	return nil
}

// signature returns the commit identity, defaulting sensibly when the
// options omit one so go-git never rejects the commit for a missing
// author.
func (c *Client) signature() (name, email string) {
	name = strings.TrimSpace(c.opts.Author)
	if name == "" {
		name = "go-tex playground"
	}
	email = strings.TrimSpace(c.opts.Email)
	if email == "" {
		email = "playground@go-tex.local"
	}
	return name, email
}

// Pull fast-forwards the working tree against origin. "Already
// up-to-date" is tolerated (returns nil), mirroring loom.
func (r *Repo) Pull(ctx context.Context) error {
	wt, err := r.repo.Worktree()
	if err != nil {
		return fmt.Errorf("browsergit: worktree: %w", err)
	}
	err = wt.PullContext(ctx, &git.PullOptions{
		RemoteName:    RemoteName,
		ReferenceName: plumbing.NewBranchReferenceName(r.branch),
		Auth:          r.client.auth(),
		Depth:         1,
		SingleBranch:  true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return classifyErr("pull", err)
	}
	return nil
}

// Push pushes the tracked branch to origin (refs/heads/b:refs/heads/b).
// "Already up-to-date" is tolerated. A rejected non-fast-forward surfaces
// as ErrNonFastForward.
func (r *Repo) Push(ctx context.Context) error {
	spec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", r.branch, r.branch)
	err := r.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: RemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(spec)},
		Auth:       r.client.auth(),
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return classifyErr("push", err)
	}
	return nil
}

// Commit is one entry in Log, HEAD-first. Mirrors loom's gitLogEntry.
type Commit struct {
	Hash    string
	Parents []string
	Author  string
	Email   string
	Subject string
	When    time.Time
}

// Log returns up to limit commits from HEAD, newest first. A limit <= 0
// collapses to loom's default cap of 200.
func (r *Repo) Log(limit int) ([]Commit, error) {
	if limit <= 0 {
		limit = 200
	}
	head, err := r.repo.Head()
	if err != nil {
		// Empty repo (no commits) → empty log, not an error.
		return []Commit{}, nil
	}
	iter, err := r.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, fmt.Errorf("browsergit: log: %w", err)
	}
	defer iter.Close()

	out := make([]Commit, 0, limit)
	_ = iter.ForEach(func(c *object.Commit) error {
		if len(out) >= limit {
			return errStopWalk
		}
		parents := make([]string, 0, len(c.ParentHashes))
		for _, p := range c.ParentHashes {
			parents = append(parents, p.String())
		}
		out = append(out, Commit{
			Hash:    c.Hash.String(),
			Parents: parents,
			Author:  c.Author.Name,
			Email:   c.Author.Email,
			Subject: firstLine(c.Message),
			When:    c.Author.When,
		})
		return nil
	})
	return out, nil
}

// cleanPath normalises a working-tree path to a slash-relative form so
// memfs stores it under a stable key regardless of leading "./" or "/".
func cleanPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// firstLine returns the commit subject (text up to the first newline).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// classifyErr maps a raw go-git error onto exactly one sentinel class,
// wrapping the original so callers keep both the class (errors.Is) and
// the detail (Error()). op names the operation for the message prefix.
func classifyErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w (%s): %v", ErrAuth, op, err)
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w (%s): %v", ErrRepoNotFound, op, err)
	case strings.Contains(err.Error(), "non-fast-forward"),
		errors.Is(err, git.ErrForceNeeded):
		return fmt.Errorf("%w (%s): %v", ErrNonFastForward, op, err)
	default:
		return fmt.Errorf("%w (%s): %v", ErrTransport, op, err)
	}
}
