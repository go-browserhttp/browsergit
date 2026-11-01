// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-browserhttp/browsergit/remote"
)

// gitlab speaks GitLab REST v4: https://docs.gitlab.com/api/repositories/
// (tree, raw blob), /api/branches/, /api/commits/ (create a commit with
// multiple files and actions) and /api/merge_requests/.
//
// A token needs the api scope for Propose (read_api / read_repository suffice
// for Load). It is sent as "Authorization: Bearer", which GitLab accepts for
// OAuth and personal access tokens alike; never in the query string.
type gitlab struct{ c *client }

// maxTreePages bounds the tree pagination: 100 entries a page.
const maxTreePages = 1000

func (g *gitlab) project(repo string) string { return "/projects/" + url.PathEscape(repo) }

func (g *gitlab) defaultBranch(ctx context.Context, repo string) (string, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.c.getJSON(ctx, request{op: "get project", path: g.project(repo)}, &p); err != nil {
		return "", err
	}
	if err := remote.CheckBranch(p.DefaultBranch); err != nil {
		return "", fmt.Errorf("%w: default branch: %v", remote.ErrBadResponse, err)
	}
	return p.DefaultBranch, nil
}

func (g *gitlab) branchPath(repo, branch string) string {
	return g.project(repo) + "/repository/branches/" + url.PathEscape(branch)
}

func (g *gitlab) head(ctx context.Context, repo, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := g.c.getJSON(ctx, request{op: "get branch", path: g.branchPath(repo, branch)}, &b); err != nil {
		return "", err
	}
	if !remote.IsCommitID(b.Commit.ID) {
		return "", fmt.Errorf("%w: branch %q has no commit id", remote.ErrBadResponse, branch)
	}
	return b.Commit.ID, nil
}

// parseGitLabTree decodes one page of a tree listing into its regular files.
func parseGitLabTree(data []byte) ([]entry, int, error) {
	var t []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, 0, fmt.Errorf("%w: tree: %v", remote.ErrBadResponse, err)
	}
	var out []entry
	for _, e := range t {
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
			continue
		}
		out = append(out, entry{path: e.Path, blob: e.ID, size: -1})
	}
	return out, len(t), nil
}

func (g *gitlab) list(ctx context.Context, repo, commit, dir string) ([]entry, error) {
	var all []entry
	page := "1"
	for n := 0; page != ""; n++ {
		if n >= maxTreePages {
			return nil, fmt.Errorf("%w: tree has more than %d pages", remote.ErrTooLarge, maxTreePages)
		}
		q := url.Values{"ref": {commit}, "recursive": {"true"}, "per_page": {"100"}, "page": {page}}
		if dir != "" {
			q.Set("path", dir)
		}
		rsp, err := g.c.do(ctx, request{op: "list tree", path: g.project(repo) + "/repository/tree", query: q})
		if err != nil {
			return nil, err
		}
		es, _, err := parseGitLabTree(rsp.body)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
		if len(all) > g.c.lim.MaxFiles {
			return nil, fmt.Errorf("%w: more than %d files", remote.ErrTooLarge, g.c.lim.MaxFiles)
		}
		// The page NUMBER, never the Link URL: a URL from the server would be
		// requested with the token, wherever it pointed.
		next := rsp.header.Get("X-Next-Page")
		if next != "" {
			if v, err := strconv.Atoi(next); err != nil || v <= n+1 {
				return nil, fmt.Errorf("%w: X-Next-Page %q", remote.ErrBadResponse, next)
			}
		}
		page = next
	}
	return all, nil
}

func (g *gitlab) blob(ctx context.Context, repo string, e entry) ([]byte, error) {
	rsp, err := g.c.do(ctx, request{op: "get blob", path: g.project(repo) + "/repository/blobs/" + esc(e.blob) + "/raw", raw: true, limit: g.c.lim.MaxFileBytes})
	if err != nil {
		return nil, err
	}
	return rsp.body, nil
}

// Load implements remote.Remote.
func (g *gitlab) Load(ctx context.Context, repo, ref, dir string) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(repo, true); err != nil {
		return nil, remote.Base{}, err
	}
	if err := remote.CheckDir(dir); err != nil {
		return nil, remote.Base{}, err
	}
	if ref == "" {
		var err error
		if ref, err = g.defaultBranch(ctx, repo); err != nil {
			return nil, remote.Base{}, err
		}
	}
	if err := remote.CheckBranch(ref); err != nil {
		return nil, remote.Base{}, err
	}
	commit, err := g.head(ctx, repo, ref)
	if err != nil {
		return nil, remote.Base{}, err
	}
	return g.loadAt(ctx, remote.Base{Forge: string(GitLab), Repo: repo, Ref: ref, Dir: dir, Commit: commit})
}

func (g *gitlab) loadAt(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
	all, err := g.list(ctx, base.Repo, base.Commit, base.Dir)
	if err != nil {
		return nil, remote.Base{}, err
	}
	es, err := g.c.selectEntries(all, base.Dir)
	if err != nil {
		return nil, remote.Base{}, err
	}
	files, blobs, err := g.c.fetchAll(ctx, es, func(ctx context.Context, e entry) ([]byte, error) { return g.blob(ctx, base.Repo, e) })
	if err != nil {
		return nil, remote.Base{}, err
	}
	base.Blobs = blobs
	return files, base, nil
}

// Refresh implements remote.Remote.
func (g *gitlab) Refresh(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(base.Repo, true); err != nil {
		return nil, remote.Base{}, err
	}
	if err := remote.CheckBranch(base.Ref); err != nil {
		return nil, remote.Base{}, err
	}
	head, err := g.head(ctx, base.Repo, base.Ref)
	if err != nil {
		return nil, remote.Base{}, err
	}
	if head == base.Commit {
		return nil, base, nil
	}
	return g.loadAt(ctx, remote.Base{Forge: base.Forge, Repo: base.Repo, Ref: base.Ref, Dir: base.Dir, Commit: head})
}

type glAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

// Propose implements remote.Remote: one commit API call that creates the
// branch from base.Commit (start_sha, never force), then a merge request.
func (g *gitlab) Propose(ctx context.Context, base remote.Base, files remote.Files, p remote.Proposal) (remote.Result, error) {
	changes, err := g.c.precheck(base, files, p, true)
	if err != nil {
		return remote.Result{}, err
	}
	def, err := g.defaultBranch(ctx, base.Repo)
	if err != nil {
		return remote.Result{}, err
	}
	err = g.c.guardBranch(ctx, p, def, func(ctx context.Context, b string) (bool, error) {
		return g.c.exists(ctx, request{op: "check branch", path: g.branchPath(base.Repo, b)})
	})
	if err != nil {
		return remote.Result{}, err
	}
	actions := make([]glAction, 0, len(changes))
	for _, ch := range changes {
		a := glAction{Action: ch.Op.String(), FilePath: ch.Path}
		if ch.Op != remote.Delete {
			a.Content = base64.StdEncoding.EncodeToString(ch.Data)
			a.Encoding = "base64"
		}
		actions = append(actions, a)
	}
	body := map[string]any{
		"branch":         p.Branch,
		"start_sha":      base.Commit,
		"commit_message": remote.Message(p),
		"actions":        actions,
		"force":          false,
	}
	var commit struct {
		ID string `json:"id"`
	}
	if _, err := g.c.getJSON(ctx, request{op: "create commit", method: http.MethodPost, path: g.project(base.Repo) + "/repository/commits", body: body}, &commit); err != nil {
		return remote.Result{}, err
	}
	if !remote.IsCommitID(commit.ID) {
		return remote.Result{}, fmt.Errorf("%w: commit id", remote.ErrBadResponse)
	}
	res := remote.Result{Branch: p.Branch, Commit: commit.ID}
	var mr struct {
		WebURL string `json:"web_url"`
	}
	mrBody := map[string]any{"source_branch": p.Branch, "target_branch": base.Ref, "title": p.Title, "description": p.Body}
	if _, err := g.c.getJSON(ctx, request{op: "create merge request", method: http.MethodPost, path: g.project(base.Repo) + "/merge_requests", body: mrBody}, &mr); err != nil {
		return res, fmt.Errorf("branch %s was pushed but the merge request failed: %w", p.Branch, err)
	}
	if res.URL, err = g.c.checkWebURL(mr.WebURL); err != nil {
		return res, err
	}
	return res, nil
}
