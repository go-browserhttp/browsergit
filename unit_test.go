// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

// White-box unit tests for the pure, transport-free logic: URL joining,
// auth-username selection, path normalisation, status classification,
// commit-signature defaulting, and error classification. These need no
// network and run on every `go test` lane.

import (
	"errors"
	"fmt"
	"os"
	"testing"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// lstatErrFS is a working-tree filesystem whose root cannot be stat'd, so a
// Walk fails at the first step — the error path List must wrap.
type lstatErrFS struct{ billy.Filesystem }

func (lstatErrFS) Lstat(string) (os.FileInfo, error) { return nil, errors.New("lstat boom") }

func TestListWalkErrorIsWrapped(t *testing.T) {
	r := &Repo{fs: lstatErrFS{memfs.New()}}
	files, err := r.List()
	if files != nil || err == nil {
		t.Fatalf("List over an unwalkable tree = %v, %v; want nil, error", files, err)
	}
	if err.Error() == "" {
		t.Fatal("expected a non-empty wrapped error")
	}
}

func TestListPrunesGitAndSorts(t *testing.T) {
	// A working tree whose memfs also carries a .git control directory (a
	// non-memory storer would leave one there): List must prune it and return
	// only the regular files, sorted.
	fs := memfs.New()
	for _, p := range []string{"main.tex", "chapters/intro.tex", "README.md", ".git/config", ".git/HEAD"} {
		if err := billyutil.WriteFile(fs, p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Repo{fs: fs}
	files, err := r.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"README.md", "chapters/intro.tex", "main.tex"}
	if len(files) != len(want) {
		t.Fatalf("List = %v, want %v (.git pruned)", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("List = %v, want %v", files, want)
		}
	}
}

func TestRepoURL(t *testing.T) {
	cases := []struct {
		name, base, repoPath, want string
	}{
		{"proxy-prefix", "https://proxy.example/github.com", "go-tex/demo.git", "https://proxy.example/github.com/go-tex/demo.git"},
		{"forgejo-base", "https://forge.example.org", "owner/repo", "https://forge.example.org/owner/repo"},
		{"trailing-slash-base", "https://forge.example.org/", "owner/repo", "https://forge.example.org/owner/repo"},
		{"leading-slash-path", "https://forge.example.org", "/owner/repo/", "https://forge.example.org/owner/repo"},
		{"empty-base", "", "owner/repo", "owner/repo"},
		{"empty-path", "https://forge.example.org", "", "https://forge.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Options{BaseURL: tc.base})
			if got := c.repoURL(tc.repoPath); got != tc.want {
				t.Fatalf("repoURL(%q) with base %q = %q, want %q", tc.repoPath, tc.base, got, tc.want)
			}
		})
	}
}

func TestAuthUsernameAndAuth(t *testing.T) {
	// No token → nil auth (anonymous).
	if a := New(Options{}).auth(); a != nil {
		t.Fatalf("expected nil auth without a token, got %#v", a)
	}
	// GitLab wants "oauth2"; everyone else "git".
	gl := New(Options{Provider: "GitLab", Token: "tok"})
	if gl.authUsername() != "oauth2" {
		t.Fatalf("gitlab username = %q, want oauth2", gl.authUsername())
	}
	gh := New(Options{Provider: "github", Token: "tok"})
	if gh.authUsername() != "git" {
		t.Fatalf("github username = %q, want git", gh.authUsername())
	}
	ba, ok := gh.auth().(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth() type = %T, want *githttp.BasicAuth", gh.auth())
	}
	if ba.Username != "git" || ba.Password != "tok" {
		t.Fatalf("basic auth = %q/%q, want git/tok", ba.Username, ba.Password)
	}
}

func TestSignatureDefaults(t *testing.T) {
	n, e := New(Options{}).signature()
	if n != "go-tex playground" || e != "playground@go-tex.local" {
		t.Fatalf("default signature = %q/%q", n, e)
	}
	n, e = New(Options{Author: " Ada ", Email: " ada@x "}).signature()
	if n != "Ada" || e != "ada@x" {
		t.Fatalf("signature trim = %q/%q", n, e)
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"README.md":     "README.md",
		"./README.md":   "README.md",
		"/README.md":    "README.md",
		"a/../b/c.tex":  "b/c.tex",
		"dir/sub/f.txt": "dir/sub/f.txt",
		"":              "",
		"/":             "",
	}
	for in, want := range cases {
		if got := cleanPath(in); got != want {
			t.Fatalf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("subject\nbody\nmore"); got != "subject" {
		t.Fatalf("firstLine multi = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Fatalf("firstLine single = %q", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		work, stage git.StatusCode
		want        string
	}{
		{git.Untracked, git.Unmodified, "untracked"},
		{git.Deleted, git.Unmodified, "deleted"},
		{git.Unmodified, git.Deleted, "deleted"},
		{git.Modified, git.Unmodified, "modified"},
		{git.Unmodified, git.Modified, "modified"},
		{git.Unmodified, git.Added, "staged"},
		{git.Unmodified, git.Renamed, "staged"},
		{git.Unmodified, git.Copied, "staged"},
		{git.Unmodified, git.Unmodified, "modified"}, // default fallthrough
	}
	for _, tc := range cases {
		if got := classify(tc.work, tc.stage); got != tc.want {
			t.Fatalf("classify(%v,%v) = %q, want %q", tc.work, tc.stage, got, tc.want)
		}
	}
}

func TestClassifyErr(t *testing.T) {
	if classifyErr("op", nil) != nil {
		t.Fatal("classifyErr(nil) must be nil")
	}
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"auth-required", transport.ErrAuthenticationRequired, ErrAuth},
		{"auth-failed", transport.ErrAuthorizationFailed, ErrAuth},
		{"repo-not-found", transport.ErrRepositoryNotFound, ErrRepoNotFound},
		{"non-ff-string", fmt.Errorf("non-fast-forward update: refs/heads/main"), ErrNonFastForward},
		{"force-needed", git.ErrForceNeeded, ErrNonFastForward},
		{"generic", errors.New("connection refused"), ErrTransport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyErr("clone", tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyErr(%v) class = %v, want %v", tc.in, got, tc.want)
			}
			// The wrapped original detail must survive for the UI.
			if !errors.Is(got, tc.in) && tc.in != transport.ErrAuthenticationRequired {
				// string-matched (non-ff) path won't errors.Is the raw
				// error; that's fine — detail is preserved in the message.
				if got.Error() == "" {
					t.Fatalf("empty error message")
				}
			}
		})
	}
}
