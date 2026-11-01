// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package remote

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-browserhttp/browsergit"
	"github.com/go-browserhttp/browsergit/secret"
)

// GitConfig configures a [Git] remote.
type GitConfig struct {
	// BaseURL is the smart-HTTP base: a forge ("https://codeberg.org") or a CORS
	// proxy prefix ("https://proxy.example/codeberg.org"). It must not carry a
	// credential; a URL that does is refused rather than cleaned, so the caller
	// learns where the token was.
	BaseURL string
	// Web, optional, is the forge's web base, used to build the compare page
	// URL that a proposal returns ("https://codeberg.org").
	Web string
	// Token authenticates pushes and private clones (HTTP Basic, token as password).
	Token secret.Token
	// Provider selects the Basic user name ("gitlab" uses "oauth2").
	Provider string
	// Author and Email sign proposal commits.
	Author, Email string
	Limits        Limits
	// BranchPrefix, when set, is required of every proposal branch.
	BranchPrefix string
}

// Git implements [Remote] over git smart HTTP. Smart HTTP cannot open a pull
// request, so Propose pushes the new branch and returns the forge's compare
// page, where the person opens the request themselves.
type Git struct {
	cfg    GitConfig
	client *browsergit.Client
}

// NewGit validates cfg and returns a Git remote.
func NewGit(cfg GitConfig) (*Git, error) {
	if err := checkBase(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Web != "" {
		if err := checkBase(cfg.Web); err != nil {
			return nil, err
		}
	}
	cfg.Limits = cfg.Limits.OrDefault()
	c := browsergit.New(browsergit.Options{
		BaseURL:  cfg.BaseURL,
		Token:    cfg.Token.Reveal(),
		Author:   cfg.Author,
		Email:    cfg.Email,
		Provider: cfg.Provider,
	})
	return &Git{cfg: cfg, client: c}, nil
}

// checkBase accepts an absolute http(s) URL without user info, query or
// fragment; plain http only for a loopback host.
func checkBase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: base URL %q", ErrNotConfigured, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%w: a base URL must not carry a credential", ErrNotConfigured)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: base URL %q has a query or fragment", ErrNotConfigured, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopback(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("%w: base URL %q is not https", ErrNotConfigured, raw)
}

// IsLoopback reports whether host names the local machine.
func IsLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (g *Git) repoPath(repo string) string { return repo + ".git" }

func (g *Git) clone(ctx context.Context, repo, ref string) (*browsergit.Repo, error) {
	r, err := g.client.Clone(ctx, g.repoPath(repo), ref, 1)
	if err != nil {
		return nil, g.classify(err)
	}
	return r, nil
}

// Load implements [Remote].
func (g *Git) Load(ctx context.Context, repo, ref, dir string) (Files, Base, error) {
	if err := CheckRepo(repo, true); err != nil {
		return nil, Base{}, err
	}
	if err := CheckDir(dir); err != nil {
		return nil, Base{}, err
	}
	if ref == "" {
		ref = browsergit.DefaultBranch
	}
	if err := CheckBranch(ref); err != nil {
		return nil, Base{}, err
	}
	r, err := g.clone(ctx, repo, ref)
	if err != nil {
		return nil, Base{}, err
	}
	return g.read(r, Base{Forge: "git", Repo: repo, Ref: ref, Dir: dir})
}

func (g *Git) read(r *browsergit.Repo, base Base) (Files, Base, error) {
	head, err := r.Head()
	if err != nil {
		return nil, Base{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	blobs, err := r.Blobs()
	if err != nil {
		return nil, Base{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	files := Files{}
	base.Commit = head
	base.Blobs = map[string]string{}
	var total int64
	for full, id := range blobs {
		rel, ok := Rel(base.Dir, full)
		if !ok {
			continue
		}
		if err := CheckPath(rel); err != nil {
			return nil, Base{}, err
		}
		if len(files) >= g.cfg.Limits.MaxFiles {
			return nil, Base{}, fmt.Errorf("%w: more than %d files", ErrTooLarge, g.cfg.Limits.MaxFiles)
		}
		data, err := r.ReadFile(full)
		if err != nil {
			return nil, Base{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
		}
		if int64(len(data)) > g.cfg.Limits.MaxFileBytes {
			return nil, Base{}, fmt.Errorf("%w: %s", ErrTooLarge, full)
		}
		total += int64(len(data))
		if total > g.cfg.Limits.MaxTotalBytes {
			return nil, Base{}, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, g.cfg.Limits.MaxTotalBytes)
		}
		files[rel] = data
		base.Blobs[rel] = id
	}
	return files, base, nil
}

// Propose implements [Remote].
func (g *Git) Propose(ctx context.Context, base Base, files Files, p Proposal) (Result, error) {
	if err := CheckRepo(base.Repo, true); err != nil {
		return Result{}, err
	}
	if err := CheckProposal(base, p); err != nil {
		return Result{}, err
	}
	if err := RequirePrefix(p.Branch, g.cfg.BranchPrefix); err != nil {
		return Result{}, err
	}
	changes, err := Changes(base, files, g.cfg.Limits)
	if err != nil {
		return Result{}, err
	}
	if len(changes) == 0 {
		return Result{}, ErrNoChanges
	}
	r, err := g.clone(ctx, base.Repo, base.Ref)
	if err != nil {
		return Result{}, err
	}
	head, err := r.Head()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if head != base.Commit {
		// A depth-1 clone holds only the tip, so we cannot branch from an older
		// commit; and proposing on top of a tip the person never saw would put
		// other people's changes in their pull request without telling them.
		return Result{}, fmt.Errorf("%w: %s is now at %s", ErrStale, base.Ref, head)
	}
	for _, c := range changes {
		switch c.Op {
		case Delete:
			err = r.RemoveFile(c.Path)
		default:
			err = r.WriteFile(c.Path, c.Data)
		}
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	commit, err := r.Commit(Message(p))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err := r.PushNewBranch(ctx, p.Branch); err != nil {
		return Result{}, g.classify(err)
	}
	return Result{Branch: p.Branch, Commit: commit, URL: g.compareURL(base, p.Branch)}, nil
}

// compareURL is the forge's "open a pull request" page for branch. Gitea,
// Forgejo and GitHub use /owner/repo/compare/base...head; GitLab uses
// /-/merge_requests/new with the source branch as a query parameter.
func (g *Git) compareURL(base Base, branch string) string {
	if g.cfg.Web == "" {
		return ""
	}
	web := strings.TrimRight(g.cfg.Web, "/")
	if strings.EqualFold(g.cfg.Provider, "gitlab") {
		q := url.Values{}
		q.Set("merge_request[source_branch]", branch)
		q.Set("merge_request[target_branch]", base.Ref)
		return web + "/" + base.Repo + "/-/merge_requests/new?" + q.Encode()
	}
	return web + "/" + base.Repo + "/compare/" + url.PathEscape(base.Ref) + "..." + url.PathEscape(branch)
}

// Refresh implements [Remote].
func (g *Git) Refresh(ctx context.Context, base Base) (Files, Base, error) {
	if err := CheckRepo(base.Repo, true); err != nil {
		return nil, Base{}, err
	}
	if err := CheckBranch(base.Ref); err != nil {
		return nil, Base{}, err
	}
	r, err := g.clone(ctx, base.Repo, base.Ref)
	if err != nil {
		return nil, Base{}, err
	}
	head, err := r.Head()
	if err != nil {
		return nil, Base{}, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if head == base.Commit {
		return nil, base, nil
	}
	return g.read(r, Base{Forge: base.Forge, Repo: base.Repo, Ref: base.Ref, Dir: base.Dir})
}

// classify maps browsergit's classes onto this package's, redacting the token
// from the detail.
func (g *Git) classify(err error) error {
	msg := secret.Redact(err.Error(), g.cfg.Token)
	switch {
	case errors.Is(err, browsergit.ErrAuth):
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case errors.Is(err, browsergit.ErrRepoNotFound):
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case errors.Is(err, browsergit.ErrBranchExists):
		return fmt.Errorf("%w: %s", ErrBranchExists, msg)
	case errors.Is(err, browsergit.ErrNonFastForward):
		return fmt.Errorf("%w: %s", ErrBranchExists, msg)
	}
	return fmt.Errorf("%w: %s", ErrTransport, msg)
}
