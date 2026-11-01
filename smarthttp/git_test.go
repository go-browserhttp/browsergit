// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package smarthttp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/go-browserhttp/browsergit/internal/gittest"
	. "github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

// the names the test was written with, before the package moved
type GitConfig = Config

var NewGit = New

const placeholder = "SMART-HTTP-TOKEN-PLACEHOLDER"

func newOrigin(t *testing.T) (root, bare string, o *gittest.Origin) {
	t.Helper()
	root = t.TempDir()
	bare = gittest.Seed(t, root, "o/deck", map[string]string{
		"README.md":         "# repo\n",
		"slides/deck.md":    "# one\n",
		"slides/img/a.txt":  "a\n",
		"other/ignored.txt": "x\n",
	})
	return root, bare, gittest.Serve(t, root, placeholder)
}

func newGit(t *testing.T, o *gittest.Origin, token string) *Git {
	t.Helper()
	g, err := NewGit(GitConfig{BaseURL: o.URL, Web: "https://forge.example", Token: secret.New(token), BranchPrefix: "slides/"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGitLoadProposeRefresh(t *testing.T) {
	root, bare, o := newOrigin(t)
	g := newGit(t, o, placeholder)
	ctx := context.Background()

	files, base, err := g.Load(ctx, "o/deck", "", "slides")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files["deck.md"]) != "# one\n" || base.Ref != "main" || base.Dir != "slides" || !IsCommitID(base.Commit) {
		t.Fatalf("load = %v %+v", files, base)
	}
	if base.Blobs["img/a.txt"] != BlobID([]byte("a\n")) {
		t.Fatal("blob ids must be git's")
	}

	files["deck.md"] = []byte("# one, edited\n")
	delete(files, "img/a.txt")
	files["new.md"] = []byte("new\n")
	res, err := g.Propose(ctx, base, files, Proposal{Branch: "slides/edit-1", Title: "Edit deck", Body: "why"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "slides/edit-1" || !IsCommitID(res.Commit) || res.URL != "https://forge.example/o/deck/compare/main...slides%2Fedit-1" {
		t.Fatalf("result = %+v", res)
	}
	// main did not move; the branch holds the change.
	if got := gittest.Git(t, bare, "rev-parse", "main"); got != base.Commit {
		t.Fatalf("main moved to %s", got)
	}
	if got := gittest.Git(t, bare, "show", "slides/edit-1:slides/deck.md"); got != "# one, edited" {
		t.Fatalf("branch content = %q", got)
	}
	if got := gittest.Git(t, bare, "log", "-1", "--format=%s", "slides/edit-1"); got != "Edit deck" {
		t.Fatalf("subject = %q", got)
	}

	// The same branch again is refused, and nothing is pushed over it.
	if _, err := g.Propose(ctx, base, files, Proposal{Branch: "slides/edit-1", Title: "again"}); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("existing branch: %v", err)
	}
	if got := gittest.Git(t, bare, "rev-parse", "slides/edit-1"); got != res.Commit {
		t.Fatal("the existing branch moved")
	}

	// Refresh with no change upstream.
	theirs, same, err := g.Refresh(ctx, base)
	if err != nil || theirs != nil || same.Commit != base.Commit {
		t.Fatalf("refresh unchanged = %v %+v %v", theirs, same, err)
	}
	// Someone else advances main: Refresh sees it, Propose on the old base is stale.
	gittest.Advance(t, root, bare, "slides/deck.md", "# theirs\n")
	theirs, moved, err := g.Refresh(ctx, base)
	if err != nil || string(theirs["deck.md"]) != "# theirs\n" || moved.Commit == base.Commit {
		t.Fatalf("refresh moved = %v %+v %v", theirs, moved, err)
	}
	if _, err := g.Propose(ctx, base, files, Proposal{Branch: "slides/edit-2", Title: "late"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale base: %v", err)
	}

	// The token travelled only as a Basic password, never in a URL.
	for _, h := range o.Auth() {
		if h == "" {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if string(raw) != "git:"+placeholder {
			t.Fatalf("unexpected Authorization %q", h)
		}
	}
}

func TestGitRefusesBeforeTouchingTheNetwork(t *testing.T) {
	_, _, o := newOrigin(t)
	g := newGit(t, o, placeholder)
	ctx := context.Background()
	_, base, err := g.Load(ctx, "o/deck", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(o.Auth())
	cases := map[string]struct {
		p    Proposal
		f    Files
		want error
	}{
		"onto the loaded branch": {Proposal{Branch: "main", Title: "t"}, Files{"a": nil}, ErrBadBranch},
		"outside the prefix":     {Proposal{Branch: "other/x", Title: "t"}, Files{"a": nil}, ErrBadBranch},
		"bad ref name":           {Proposal{Branch: "slides/a..b", Title: "t"}, Files{"a": nil}, ErrBadBranch},
		"escaping path":          {Proposal{Branch: "slides/x", Title: "t"}, Files{"../a": nil}, ErrBadPath},
		"git control dir":        {Proposal{Branch: "slides/x", Title: "t"}, Files{".git/hooks/post-checkout": nil}, ErrBadPath},
	}
	for name, c := range cases {
		if _, err := g.Propose(ctx, base, c.f, c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if n := len(o.Auth()); n != before {
		t.Fatalf("%d requests were made for proposals that must be refused locally", n-before)
	}
	files, _, _ := g.Load(ctx, "o/deck", "main", "")
	if _, err := g.Propose(ctx, base, files, Proposal{Branch: "slides/none", Title: "t"}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("no changes: %v", err)
	}
}

func TestGitAuthFailureDoesNotEchoTheToken(t *testing.T) {
	_, _, o := newOrigin(t)
	wrong := "WRONG-TOKEN-PLACEHOLDER"
	g := newGit(t, o, wrong)
	_, _, err := g.Load(context.Background(), "o/deck", "", "")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if strings.Contains(err.Error(), wrong) {
		t.Fatalf("error carries the token: %v", err)
	}
	if _, _, err := newGit(t, o, placeholder).Load(context.Background(), "o/missing", "", ""); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrTransport) {
		t.Fatalf("missing repo: %v", err)
	}
}

func TestNewGitConfig(t *testing.T) {
	for _, u := range []string{
		"http://forge.example", "https://user:pw@forge.example", "ftp://x", "https://f.example?q=1", "https://f.example#x", "::",
	} {
		if _, err := NewGit(GitConfig{BaseURL: u}); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("NewGit(%q) = %v", u, err)
		}
	}
	if _, err := NewGit(GitConfig{BaseURL: "https://f.example", Web: "http://f.example"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatal("web must be https too")
	}
	for _, u := range []string{"https://f.example", "http://localhost:3000", "http://127.0.0.1:1"} {
		if _, err := NewGit(GitConfig{BaseURL: u}); err != nil {
			t.Errorf("NewGit(%q) = %v", u, err)
		}
	}
}

func TestGitInputValidation(t *testing.T) {
	g, _ := NewGit(GitConfig{BaseURL: "https://f.example"})
	ctx := context.Background()
	if _, _, err := g.Load(ctx, "../x", "", ""); !errors.Is(err, ErrBadRepo) {
		t.Fatal(err)
	}
	if _, _, err := g.Load(ctx, "o/r", "", "../x"); !errors.Is(err, ErrBadPath) {
		t.Fatal(err)
	}
	if _, _, err := g.Load(ctx, "o/r", "a..b", ""); !errors.Is(err, ErrBadBranch) {
		t.Fatal(err)
	}
	if _, _, err := g.Refresh(ctx, Base{Repo: "x"}); !errors.Is(err, ErrBadRepo) {
		t.Fatal(err)
	}
	if _, _, err := g.Refresh(ctx, Base{Repo: "o/r", Ref: "a..b"}); !errors.Is(err, ErrBadBranch) {
		t.Fatal(err)
	}
	if _, err := g.Propose(ctx, Base{Repo: "x"}, nil, Proposal{}); !errors.Is(err, ErrBadRepo) {
		t.Fatal(err)
	}
}

func TestCompareURL(t *testing.T) {
	g, _ := NewGit(GitConfig{BaseURL: "https://f.example", Web: "https://gitlab.example/", Provider: "gitlab"})
	got := g.compareURL(Base{Repo: "g/p", Ref: "main"}, "slides/x")
	if got != "https://gitlab.example/g/p/-/merge_requests/new?merge_request%5Bsource_branch%5D=slides%2Fx&merge_request%5Btarget_branch%5D=main" {
		t.Fatal(got)
	}
	g2, _ := NewGit(GitConfig{BaseURL: "https://f.example"})
	if g2.compareURL(Base{}, "x") != "" {
		t.Fatal("no web base, no URL")
	}
}
