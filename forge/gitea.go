// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/go-browserhttp/browsergit/remote"
)

// gitea speaks the Gitea API v1, which Forgejo shares. Shapes are Gitea's
// modules/structs: GitTreeResponse (repo_tree.go), GitBlobResponse
// (git_blob.go), ChangeFilesOptions / FilesResponse (repo_file.go),
// CreatePullRequestOption (pull.go).
//
// A Gitea token needs write:repository (Forgejo OAuth tokens carry no scopes
// and so can do everything the user can: prefer a scoped access token there).
// It is sent as "Authorization: token <t>".
type gitea struct{ c *client }

func (g *gitea) repoPath(repo string) string { return "/repos/" + escRepo(repo) }

func (g *gitea) defaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.c.getJSON(ctx, request{op: "get repository", path: g.repoPath(repo)}, &r); err != nil {
		return "", err
	}
	if err := remote.CheckBranch(r.DefaultBranch); err != nil {
		return "", fmt.Errorf("%w: default branch: %v", remote.ErrBadResponse, err)
	}
	return r.DefaultBranch, nil
}

func (g *gitea) branchPath(repo, branch string) string {
	return g.repoPath(repo) + "/branches/" + escFile(branch)
}

func (g *gitea) head(ctx context.Context, repo, branch string) (string, error) {
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

// parseGiteaTree decodes one page of a recursive GitTreeResponse.
func parseGiteaTree(data []byte) (es []entry, n, total int, err error) {
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			Size int64  `json:"size"`
			SHA  string `json:"sha"`
		} `json:"tree"`
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: tree: %v", remote.ErrBadResponse, err)
	}
	for _, e := range t.Tree {
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
			continue
		}
		es = append(es, entry{path: e.Path, blob: e.SHA, size: e.Size})
	}
	return es, len(t.Tree), t.TotalCount, nil
}

func (g *gitea) list(ctx context.Context, repo, commit string) ([]entry, error) {
	var all []entry
	seen := 0
	for page := 1; ; page++ {
		if page > maxTreePages {
			return nil, fmt.Errorf("%w: tree has more than %d pages", remote.ErrTooLarge, maxTreePages)
		}
		q := url.Values{"recursive": {"true"}, "page": {strconv.Itoa(page)}, "per_page": {"1000"}}
		rsp, err := g.c.do(ctx, request{op: "get tree", path: g.repoPath(repo) + "/git/trees/" + esc(commit), query: q})
		if err != nil {
			return nil, err
		}
		es, n, total, err := parseGiteaTree(rsp.body)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
		if len(all) > g.c.lim.MaxFiles*4 {
			// Files outside dir are filtered later; still bound what we hold.
			return nil, fmt.Errorf("%w: tree too large", remote.ErrTooLarge)
		}
		seen += n
		if n == 0 || seen >= total {
			return all, nil
		}
	}
}

func (g *gitea) blob(ctx context.Context, repo string, e entry) ([]byte, error) {
	var b struct {
		Content  *string `json:"content"`
		Encoding *string `json:"encoding"`
		Size     int64   `json:"size"`
	}
	rq := request{op: "get blob", path: g.repoPath(repo) + "/git/blobs/" + esc(e.blob), limit: blobResponseLimit(g.c.lim.MaxFileBytes)}
	if _, err := g.c.getJSON(ctx, rq, &b); err != nil {
		return nil, err
	}
	if b.Size > g.c.lim.MaxFileBytes {
		return nil, fmt.Errorf("%w: %s", remote.ErrTooLarge, e.path)
	}
	if b.Content == nil {
		return nil, fmt.Errorf("%w: blob %s has no content", remote.ErrBadResponse, e.blob)
	}
	enc := ""
	if b.Encoding != nil {
		enc = *b.Encoding
	}
	return decodeBase64Blob(*b.Content, enc, g.c.lim.MaxFileBytes)
}

// Load implements remote.Remote.
func (g *gitea) Load(ctx context.Context, repo, ref, dir string) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(repo, false); err != nil {
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
	return g.loadAt(ctx, remote.Base{Forge: string(Gitea), Repo: repo, Ref: ref, Dir: dir, Commit: commit})
}

func (g *gitea) loadAt(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
	all, err := g.list(ctx, base.Repo, base.Commit)
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
func (g *gitea) Refresh(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(base.Repo, false); err != nil {
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

type gtFile struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Content   string `json:"content,omitempty"`
	SHA       string `json:"sha,omitempty"`
}

// Propose implements remote.Remote. Gitea's change-files call commits on the
// tip of "branch" into "new_branch"; it cannot start from an older commit, so a
// moved base is refused as stale, and every update and delete carries the blob
// id it expects, which Gitea checks.
func (g *gitea) Propose(ctx context.Context, base remote.Base, files remote.Files, p remote.Proposal) (remote.Result, error) {
	changes, err := g.c.precheck(base, files, p, false)
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
	head, err := g.head(ctx, base.Repo, base.Ref)
	if err != nil {
		return remote.Result{}, err
	}
	if head != base.Commit {
		return remote.Result{}, fmt.Errorf("%w: %s is now at %s", remote.ErrStale, base.Ref, head)
	}
	ops := make([]gtFile, 0, len(changes))
	for _, ch := range changes {
		f := gtFile{Operation: ch.Op.String(), Path: ch.Path, SHA: ch.OldBlob}
		if ch.Op != remote.Delete {
			f.Content = base64.StdEncoding.EncodeToString(ch.Data)
		}
		ops = append(ops, f)
	}
	body := map[string]any{
		"branch":     base.Ref,
		"new_branch": p.Branch,
		"message":    remote.Message(p),
		"files":      ops,
		"force_push": false,
	}
	var out struct {
		Commit *struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := g.c.getJSON(ctx, request{op: "change files", method: methodPost, path: g.repoPath(base.Repo) + "/contents", body: body}, &out); err != nil {
		return remote.Result{}, err
	}
	if out.Commit == nil || !remote.IsCommitID(out.Commit.SHA) {
		return remote.Result{}, fmt.Errorf("%w: commit id", remote.ErrBadResponse)
	}
	res := remote.Result{Branch: p.Branch, Commit: out.Commit.SHA}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	prBody := map[string]any{"head": p.Branch, "base": base.Ref, "title": p.Title, "body": p.Body}
	if _, err := g.c.getJSON(ctx, request{op: "create pull request", method: methodPost, path: g.repoPath(base.Repo) + "/pulls", body: prBody}, &pr); err != nil {
		return res, fmt.Errorf("branch %s was pushed but the pull request failed: %w", p.Branch, err)
	}
	if res.URL, err = g.c.checkWebURL(pr.HTMLURL); err != nil {
		return res, err
	}
	return res, nil
}
