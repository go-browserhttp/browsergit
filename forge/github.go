// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/go-browserhttp/browsergit/remote"
)

// github speaks the GitHub REST git database API:
// https://docs.github.com/en/rest/git (blobs, trees, commits, refs) and
// https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request.
//
// Fine-grained token permissions (per GitHub's "Permissions required for
// fine-grained personal access tokens"): Metadata read, Contents read (Load) and
// write (Propose), Pull requests write. A change touching .github/workflows also
// needs Workflows write to create the ref.
type github struct{ c *client }

func (g *github) repoPath(repo string) string { return "/repos/" + escRepo(repo) }

type ghRepo struct {
	DefaultBranch string `json:"default_branch"`
}

func (g *github) defaultBranch(ctx context.Context, repo string) (string, error) {
	var r ghRepo
	if _, err := g.c.getJSON(ctx, request{op: "get repository", path: g.repoPath(repo)}, &r); err != nil {
		return "", err
	}
	if err := remote.CheckBranch(r.DefaultBranch); err != nil {
		return "", fmt.Errorf("%w: default branch: %v", remote.ErrBadResponse, err)
	}
	return r.DefaultBranch, nil
}

type ghRef struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func (g *github) refPath(repo, branch string) string {
	return g.repoPath(repo) + "/git/ref/heads/" + escFile(branch)
}

func (g *github) head(ctx context.Context, repo, branch string) (string, error) {
	var r ghRef
	if _, err := g.c.getJSON(ctx, request{op: "get branch", path: g.refPath(repo, branch)}, &r); err != nil {
		return "", err
	}
	if r.Object.Type != "commit" || !remote.IsCommitID(r.Object.SHA) {
		return "", fmt.Errorf("%w: branch %q does not point at a commit", remote.ErrBadResponse, branch)
	}
	return r.Object.SHA, nil
}

type ghCommit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
}

func (g *github) treeOf(ctx context.Context, repo, commit string) (string, error) {
	var c ghCommit
	if _, err := g.c.getJSON(ctx, request{op: "get commit", path: g.repoPath(repo) + "/git/commits/" + esc(commit)}, &c); err != nil {
		return "", err
	}
	if !remote.IsCommitID(c.Tree.SHA) {
		return "", fmt.Errorf("%w: commit without a tree", remote.ErrBadResponse)
	}
	return c.Tree.SHA, nil
}

// parseGitHubTree decodes a recursive tree listing into its regular files.
// Symbolic links (120000) and submodules (160000) are left out.
func parseGitHubTree(data []byte) ([]entry, error) {
	var t struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size *int64 `json:"size"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: tree: %v", remote.ErrBadResponse, err)
	}
	if t.Truncated {
		// GitHub caps a recursive listing at 100 000 entries / 7 MB. A partial
		// listing would make a proposal delete what it never saw.
		return nil, fmt.Errorf("%w: the repository tree is too large to list", remote.ErrTooLarge)
	}
	var out []entry
	for _, e := range t.Tree {
		if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
			continue
		}
		size := int64(-1)
		if e.Size != nil {
			size = *e.Size
		}
		out = append(out, entry{path: e.Path, blob: e.SHA, size: size})
	}
	return out, nil
}

func (g *github) list(ctx context.Context, repo, commit string) ([]entry, error) {
	tree, err := g.treeOf(ctx, repo, commit)
	if err != nil {
		return nil, err
	}
	q := url.Values{"recursive": {"1"}}
	rsp, err := g.c.do(ctx, request{op: "get tree", path: g.repoPath(repo) + "/git/trees/" + esc(tree), query: q})
	if err != nil {
		return nil, err
	}
	return parseGitHubTree(rsp.body)
}

type ghBlob struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *github) blob(ctx context.Context, repo string, e entry) ([]byte, error) {
	var b ghBlob
	rq := request{op: "get blob", path: g.repoPath(repo) + "/git/blobs/" + esc(e.blob), limit: blobResponseLimit(g.c.lim.MaxFileBytes)}
	if _, err := g.c.getJSON(ctx, rq, &b); err != nil {
		return nil, err
	}
	return decodeBase64Blob(b.Content, b.Encoding, g.c.lim.MaxFileBytes)
}

// Load implements remote.Remote.
func (g *github) Load(ctx context.Context, repo, ref, dir string) (remote.Files, remote.Base, error) {
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
	return g.loadAt(ctx, remote.Base{Forge: string(GitHub), Repo: repo, Ref: ref, Dir: dir, Commit: commit})
}

func (g *github) loadAt(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
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
func (g *github) Refresh(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
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

type ghTreeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"` // null deletes the path; never omitted
}

// Propose implements remote.Remote: blobs, a tree on top of the base commit's
// tree, a commit whose only parent is base.Commit, a NEW ref, a pull request.
func (g *github) Propose(ctx context.Context, base remote.Base, files remote.Files, p remote.Proposal) (remote.Result, error) {
	changes, err := g.c.precheck(base, files, p, false)
	if err != nil {
		return remote.Result{}, err
	}
	def, err := g.defaultBranch(ctx, base.Repo)
	if err != nil {
		return remote.Result{}, err
	}
	err = g.c.guardBranch(ctx, p, def, func(ctx context.Context, b string) (bool, error) {
		return g.c.exists(ctx, request{op: "check branch", path: g.refPath(base.Repo, b)})
	})
	if err != nil {
		return remote.Result{}, err
	}
	baseTree, err := g.treeOf(ctx, base.Repo, base.Commit)
	if err != nil {
		return remote.Result{}, err
	}
	entries := make([]ghTreeEntry, 0, len(changes))
	for _, ch := range changes {
		e := ghTreeEntry{Path: ch.Path, Mode: "100644", Type: "blob"}
		if ch.Op != remote.Delete {
			var out struct {
				SHA string `json:"sha"`
			}
			body := map[string]string{"content": base64.StdEncoding.EncodeToString(ch.Data), "encoding": "base64"}
			if _, err := g.c.getJSON(ctx, request{op: "create blob", method: methodPost, path: g.repoPath(base.Repo) + "/git/blobs", body: body}, &out); err != nil {
				return remote.Result{}, err
			}
			if out.SHA != remote.BlobID(ch.Data) {
				return remote.Result{}, fmt.Errorf("%w: blob id for %s", remote.ErrBadResponse, ch.Path)
			}
			sha := out.SHA
			e.SHA = &sha
		}
		entries = append(entries, e)
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	treeBody := map[string]any{"base_tree": baseTree, "tree": entries}
	if _, err := g.c.getJSON(ctx, request{op: "create tree", method: methodPost, path: g.repoPath(base.Repo) + "/git/trees", body: treeBody}, &tree); err != nil {
		return remote.Result{}, err
	}
	var commit ghCommit
	commitBody := map[string]any{"message": remote.Message(p), "tree": tree.SHA, "parents": []string{base.Commit}}
	if _, err := g.c.getJSON(ctx, request{op: "create commit", method: methodPost, path: g.repoPath(base.Repo) + "/git/commits", body: commitBody}, &commit); err != nil {
		return remote.Result{}, err
	}
	if !remote.IsCommitID(commit.SHA) {
		return remote.Result{}, fmt.Errorf("%w: commit id", remote.ErrBadResponse)
	}
	// POST /git/refs creates; it has no force and cannot move an existing ref
	// (that is PATCH /git/refs/{ref}, which this package never calls).
	refBody := map[string]string{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}
	if _, err := g.c.do(ctx, request{op: "create branch", method: methodPost, path: g.repoPath(base.Repo) + "/git/refs", body: refBody}); err != nil {
		return remote.Result{}, err
	}
	res := remote.Result{Branch: p.Branch, Commit: commit.SHA}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	prBody := map[string]any{"title": p.Title, "head": p.Branch, "base": base.Ref, "body": p.Body}
	if _, err := g.c.getJSON(ctx, request{op: "create pull request", method: methodPost, path: g.repoPath(base.Repo) + "/pulls", body: prBody}, &pr); err != nil {
		return res, fmt.Errorf("branch %s was pushed but the pull request failed: %w", p.Branch, err)
	}
	if res.URL, err = g.c.checkWebURL(pr.HTMLURL); err != nil {
		return res, err
	}
	return res, nil
}
