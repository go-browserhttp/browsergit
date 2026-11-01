// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

// Integration tests drive the real go-git HTTP transport (native
// net/http) against a live git-http-backend origin. Natively this proves
// every code path except the browser Fetch RoundTripper, which the
// separate Node-fetch wasm e2e proves. The full clone→read→write→commit
// →push cycle plus error mapping is validated here.

import (
	"context"
	"errors"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestFullCycle(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "demo", map[string]string{"main.tex": "\\documentclass{article}\n"})
	srv := startOrigin(t, root, "")
	ctx := context.Background()

	c := New(Options{BaseURL: srv.URL, Author: "Ada Lovelace", Email: "ada@go-tex.local"})
	repo, err := c.Clone(ctx, "demo.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if repo.Branch() != "main" {
		t.Fatalf("branch = %q", repo.Branch())
	}

	// Read seeded files out of the in-memory worktree.
	if b, err := repo.ReadFile("README.md"); err != nil || string(b) != "# seed\n" {
		t.Fatalf("read README.md = %q, %v", b, err)
	}
	if b, err := repo.ReadFile("main.tex"); err != nil || string(b) != "\\documentclass{article}\n" {
		t.Fatalf("read main.tex = %q, %v", b, err)
	}

	// List enumerates the working tree's regular files, sorted, with the .git
	// control directory pruned (so no plumbing paths leak into the UI's file
	// list).
	files, err := repo.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 || files[0] != "README.md" || files[1] != "main.tex" {
		t.Fatalf("List = %v, want [README.md main.tex] with .git pruned", files)
	}

	// Fresh clone is clean.
	st, err := repo.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Clean || len(st.Changes) != 0 || st.Ahead != 0 || st.Behind != 0 {
		t.Fatalf("fresh status not clean: %+v", st)
	}

	// Write a new file → dirty.
	if err := repo.WriteFile("sub/dir/from-wasm.txt", []byte("hello from go-git\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, _ = repo.Status()
	if st.Clean {
		t.Fatalf("expected dirty status after write")
	}
	foundUntracked := false
	for _, ch := range st.Changes {
		if ch.Path == "sub/dir/from-wasm.txt" && ch.Status == "untracked" {
			foundUntracked = true
		}
	}
	if !foundUntracked {
		t.Fatalf("untracked change not reported: %+v", st.Changes)
	}

	// Commit and verify ahead-by-1.
	hash, err := c2Commit(t, repo, "add from-wasm")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	st, _ = repo.Status()
	if !st.Clean {
		t.Fatalf("tree not clean after commit: %+v", st.Changes)
	}
	if st.Ahead != 1 || st.Behind != 0 {
		t.Fatalf("ahead/behind after commit = %d/%d, want 1/0", st.Ahead, st.Behind)
	}

	// Log HEAD-first.
	commits, err := repo.Log(10)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) < 2 {
		t.Fatalf("log len = %d, want >= 2", len(commits))
	}
	if commits[0].Hash != hash || commits[0].Subject != "add from-wasm" {
		t.Fatalf("log[0] = %+v, want our commit %s", commits[0], hash)
	}
	if commits[0].Author != "Ada Lovelace" || commits[0].Email != "ada@go-tex.local" {
		t.Fatalf("log[0] identity = %q/%q", commits[0].Author, commits[0].Email)
	}
	if len(commits[0].Parents) != 1 {
		t.Fatalf("log[0] parents = %v", commits[0].Parents)
	}
	// A limit smaller than the history exercises the cap.
	if capped, _ := repo.Log(1); len(capped) != 1 {
		t.Fatalf("Log(1) len = %d, want 1", len(capped))
	}

	// Push and witness via a fresh clone that must see the new file.
	if err := repo.Push(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	witness, err := c.Clone(ctx, "demo.git", "main", 1)
	if err != nil {
		t.Fatalf("witness clone: %v", err)
	}
	if b, err := witness.ReadFile("sub/dir/from-wasm.txt"); err != nil || string(b) != "hello from go-git\n" {
		t.Fatalf("witness read = %q, %v — push did not land on the origin", b, err)
	}

	// Committing a clean tree is a no-op (empty hash, no error).
	if h, err := repo.Commit("noop"); err != nil || h != "" {
		t.Fatalf("clean commit = %q, %v; want empty/no-op", h, err)
	}

	// Pull when already up-to-date tolerated.
	if err := repo.Pull(ctx); err != nil {
		t.Fatalf("pull up-to-date: %v", err)
	}

	// Empty branch + non-positive depth exercise the Clone defaults.
	if _, err := c.Clone(ctx, "demo.git", "", 0); err != nil {
		t.Fatalf("clone with defaults: %v", err)
	}
}

// c2Commit is a thin wrapper so the test reads clearly.
func c2Commit(t *testing.T, r *Repo, msg string) (string, error) {
	t.Helper()
	return r.Commit(msg)
}

func TestPullFastForward(t *testing.T) {
	root := t.TempDir()
	bare := seedBareRepo(t, root, "ff", nil)
	srv := startOrigin(t, root, "")
	ctx := context.Background()

	c := New(Options{BaseURL: srv.URL, Author: "A", Email: "a@x"})
	repo, err := c.Clone(ctx, "ff.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Another client pushes a new file.
	commitAndPushExternal(t, root, bare, "added.txt", "added upstream\n")

	if err := repo.Pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if b, err := repo.ReadFile("added.txt"); err != nil || string(b) != "added upstream\n" {
		t.Fatalf("post-pull read = %q, %v", b, err)
	}
}

func TestCloneAuth(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "private", nil)
	srv := startOrigin(t, root, "s3cr3t")
	ctx := context.Background()

	// Wrong token → ErrAuth.
	bad := New(Options{BaseURL: srv.URL, Token: "wrong"})
	if _, err := bad.Clone(ctx, "private.git", "main", 1); !errors.Is(err, ErrAuth) {
		t.Fatalf("bad-token clone err = %v, want ErrAuth", err)
	}
	// No token at all → still ErrAuth (401 with no credentials).
	anon := New(Options{BaseURL: srv.URL})
	if _, err := anon.Clone(ctx, "private.git", "main", 1); !errors.Is(err, ErrAuth) {
		t.Fatalf("anon clone err = %v, want ErrAuth", err)
	}
	// Correct token → success.
	good := New(Options{BaseURL: srv.URL, Token: "s3cr3t"})
	if _, err := good.Clone(ctx, "private.git", "main", 1); err != nil {
		t.Fatalf("good-token clone: %v", err)
	}
}

func TestPushNonFastForward(t *testing.T) {
	root := t.TempDir()
	bare := seedBareRepo(t, root, "race", nil)
	srv := startOrigin(t, root, "")
	ctx := context.Background()

	c := New(Options{BaseURL: srv.URL, Author: "A", Email: "a@x"})
	repo, err := c.Clone(ctx, "race.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Remote advances behind our back.
	commitAndPushExternal(t, root, bare, "theirs.txt", "theirs\n")

	// We commit locally then push → non-fast-forward rejection.
	if err := repo.WriteFile("ours.txt", []byte("ours\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := repo.Commit("ours"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	err = repo.Push(ctx)
	if !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("push err = %v, want ErrNonFastForward", err)
	}
}

func TestCloneTransportError(t *testing.T) {
	// Point at a closed port → connection refused → ErrTransport.
	c := New(Options{BaseURL: "http://127.0.0.1:1"})
	_, err := c.Clone(context.Background(), "nope.git", "main", 1)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("clone err = %v, want ErrTransport", err)
	}
}

func TestCloneRepoNotFound(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "exists", nil)
	srv := startOrigin(t, root, "")
	c := New(Options{BaseURL: srv.URL})
	_, err := c.Clone(context.Background(), "missing.git", "main", 1)
	if !errors.Is(err, ErrRepoNotFound) && !errors.Is(err, ErrTransport) {
		// git-http-backend returns 404 for an absent repo; go-git maps
		// that to ErrRepositoryNotFound. Some versions surface it as a
		// generic transport error — accept either, but never success.
		t.Fatalf("missing-repo clone err = %v, want ErrRepoNotFound/ErrTransport", err)
	}
	if err == nil {
		t.Fatal("expected an error cloning a missing repo")
	}
}

func TestPullTransportError(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "gone", nil)
	srv := startOrigin(t, root, "")
	ctx := context.Background()
	c := New(Options{BaseURL: srv.URL, Author: "A", Email: "a@x"})
	repo, err := c.Clone(ctx, "gone.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	srv.Close() // origin disappears
	if err := repo.Pull(ctx); !errors.Is(err, ErrTransport) {
		t.Fatalf("pull-after-close err = %v, want ErrTransport", err)
	}
}

func TestReadFileErrors(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "rf", map[string]string{"dir/keep.txt": "x\n"})
	srv := startOrigin(t, root, "")
	c := New(Options{BaseURL: srv.URL})
	repo, err := c.Clone(context.Background(), "rf.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Missing path → ErrNotExist.
	if _, err := repo.ReadFile("nope.txt"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing read err = %v, want ErrNotExist", err)
	}
	// Reading a directory → a non-not-exist error (generic branch).
	if _, err := repo.ReadFile("dir"); err == nil || errors.Is(err, ErrNotExist) {
		t.Fatalf("dir read err = %v, want a generic read error", err)
	}
}

func TestWriteFileError(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "wf", map[string]string{"dir/keep.txt": "x\n"})
	srv := startOrigin(t, root, "")
	c := New(Options{BaseURL: srv.URL})
	repo, err := c.Clone(context.Background(), "wf.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// "dir" is an existing directory; opening it as a regular file for
	// writing cannot succeed, so WriteFile must surface an error.
	if err := repo.WriteFile("dir", []byte("x")); err == nil {
		t.Fatal("expected error writing to a directory path")
	}
}

// --- white-box coverage of defensive aheadBehind / Status / Commit / Log
// branches that a healthy clone never reaches ---

func TestBareRepoNoWorktree(t *testing.T) {
	// git.Init with a nil worktree yields a bare repo whose Worktree()
	// errors — exercising the Status/Commit worktree-error branches.
	bare, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{client: New(Options{}), repo: bare, branch: "main"}
	if _, err := r.Status(); err == nil {
		t.Fatal("Status on bare repo should error")
	}
	if _, err := r.Commit("x"); err == nil {
		t.Fatal("Commit on bare repo should error")
	}
	if err := r.Pull(context.Background()); err == nil {
		t.Fatal("Pull on bare repo should error")
	}
}

func TestAheadBehindDefensive(t *testing.T) {
	c := New(Options{Author: "A", Email: "a@x"})
	// Repo with a commit on main but no origin remote-tracking ref.
	repo := commitOnlyRepo(t)

	// branch missing entirely → local-ref error → (0,0).
	r := &Repo{client: c, repo: repo, branch: "does-not-exist"}
	if a, b := r.aheadBehind(); a != 0 || b != 0 {
		t.Fatalf("missing-branch aheadBehind = %d/%d", a, b)
	}
	// branch exists but no remote-tracking ref → remote-ref error → (0,0).
	r = &Repo{client: c, repo: repo, branch: "main"}
	if a, b := r.aheadBehind(); a != 0 || b != 0 {
		t.Fatalf("no-remote aheadBehind = %d/%d", a, b)
	}
	// remote-tracking ref points at a bogus hash → mergeBase error → (0,0).
	bogus := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"),
		plumbing.NewHash("0123456789012345678901234567890123456789"))
	if err := repo.Storer.SetReference(bogus); err != nil {
		t.Fatal(err)
	}
	if a, b := r.aheadBehind(); a != 0 || b != 0 {
		t.Fatalf("bogus-remote aheadBehind = %d/%d", a, b)
	}
	// mergeBase of two missing objects errors (both CommitObject fail).
	if _, err := r.mergeBase(plumbing.ZeroHash, plumbing.ZeroHash); err == nil {
		t.Fatal("mergeBase of zero hashes should error")
	}
	// Two valid but unrelated root commits have no merge base → the
	// len(bases)==0 branch. Forge a second parent-less commit reusing
	// the first commit's tree so it is a distinct, unrelated root.
	head, _ := repo.Head()
	c0, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	root2 := &object.Commit{Author: c0.Author, Committer: c0.Committer, Message: "root2", TreeHash: c0.TreeHash}
	enc := repo.Storer.NewEncodedObject()
	if err := root2.Encode(enc); err != nil {
		t.Fatal(err)
	}
	h2, err := repo.Storer.SetEncodedObject(enc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.mergeBase(head.Hash(), h2); err == nil {
		t.Fatal("unrelated roots should have no merge base")
	}
}

func TestLogEmptyRepo(t *testing.T) {
	empty, err := git.Init(memory.NewStorage(), memfsNew())
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{client: New(Options{}), repo: empty, branch: "main"}
	commits, err := r.Log(0) // limit<=0 → default cap
	if err != nil {
		t.Fatalf("log empty: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("empty repo log len = %d", len(commits))
	}
}
