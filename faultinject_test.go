// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

// Fault-injecting go-git storage + worktree filesystem wrappers. They
// exist solely to drive the defensive error-return guards in Status /
// Commit / Log / countSince that a healthy in-memory repo never reaches,
// so the package hits 100% of its error branches natively.

import (
	"errors"
	"os"
	"testing"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

var errInjected = errors.New("injected fault")

// failReadDirFS breaks the worktree Status walk (ReadDir is its first
// filesystem call) once armed, forcing wt.Status() to error.
type failReadDirFS struct {
	billy.Filesystem
	armed bool
}

func (f *failReadDirFS) ReadDir(p string) ([]os.FileInfo, error) {
	if f.armed {
		return nil, errInjected
	}
	return f.Filesystem.ReadDir(p)
}

// failObjStorer embeds a full in-memory Storer and fails storing objects
// of one chosen type once armed. Failing BlobObject breaks `git add`
// (which stores the file's blob); failing CommitObject lets add succeed
// but breaks wt.Commit's object write — isolating the two guards.
type failObjStorer struct {
	*memory.Storage
	failType plumbing.ObjectType
	armed    bool
}

func (s *failObjStorer) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if s.armed && o.Type() == s.failType {
		return plumbing.ZeroHash, errInjected
	}
	return s.Storage.SetEncodedObject(o)
}

func TestStatusReadDirFault(t *testing.T) {
	fs := &failReadDirFS{Filesystem: memfs.New()}
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := billyutil.WriteFile(fs, "f.txt", []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Repo{client: New(Options{Author: "A", Email: "a@x"}), repo: repo, branch: "main"}
	fs.armed = true
	if _, err := r.Status(); err == nil {
		t.Fatal("Status should fail when the worktree walk errors")
	}
	if _, err := r.Commit("x"); err == nil {
		t.Fatal("Commit should fail when its status check errors")
	}
}

// commitStoreFaultRepo builds a repo backed by a storer that fails
// objects of failType, with one untracked file staged for commit.
func objFaultRepo(t *testing.T, failType plumbing.ObjectType) (*Repo, *failObjStorer) {
	t.Helper()
	st := &failObjStorer{Storage: memory.NewStorage(), failType: failType}
	fs := memfs.New()
	repo, err := git.Init(st, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := billyutil.WriteFile(fs, "f.txt", []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Repo{client: New(Options{Author: "A", Email: "a@x"}), repo: repo, branch: "main"}, st
}

func TestCommitAddFault(t *testing.T) {
	// Status succeeds (untracked file seen via the walk), but staging
	// fails because the blob object cannot be written.
	r, st := objFaultRepo(t, plumbing.BlobObject)
	st.armed = true
	if _, err := r.Commit("x"); err == nil {
		t.Fatal("Commit should fail when the blob cannot be staged")
	}
}

func TestCommitStoreFault(t *testing.T) {
	// Blobs + trees store; the commit object write fails.
	r, st := objFaultRepo(t, plumbing.CommitObject)
	st.armed = true
	if _, err := r.Commit("boom"); err == nil {
		t.Fatal("Commit should fail when the commit object cannot be stored")
	}
}

func TestStageStagesUntracked(t *testing.T) {
	// A healthy repo (storer NOT armed) with one untracked file: Stage must
	// stage it (git add) so Status reports it as "staged", not "untracked".
	r, _ := objFaultRepo(t, plumbing.BlobObject)
	if err := r.Stage(); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	st, err := r.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	found := false
	for _, c := range st.Changes {
		if c.Path == "f.txt" {
			found = true
			if c.Status != "staged" {
				t.Fatalf("f.txt status after Stage = %q, want staged", c.Status)
			}
		}
	}
	if !found {
		t.Fatalf("Stage did not stage f.txt (changes = %+v)", st.Changes)
	}
}

func TestStageWorktreeError(t *testing.T) {
	// git.Init with a nil worktree yields a bare repo whose Worktree() errors,
	// exercising Stage's worktree-error branch.
	bare, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{client: New(Options{}), repo: bare, branch: "main"}
	if err := r.Stage(); err == nil {
		t.Fatal("Stage on a bare repo should error")
	}
}

func TestStageAddFault(t *testing.T) {
	// Staging fails because the blob object cannot be written.
	r, st := objFaultRepo(t, plumbing.BlobObject)
	st.armed = true
	if err := r.Stage(); err == nil {
		t.Fatal("Stage should fail when the blob cannot be staged")
	}
}

func TestLogAndCountSinceResolveFault(t *testing.T) {
	// HEAD resolves to a branch ref whose commit object is absent, so
	// repo.Head() succeeds but repo.Log(From: hash) fails to resolve.
	st := memory.NewStorage()
	bogus := plumbing.NewHash("0123456789012345678901234567890123456789")
	if err := st.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), bogus)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	repo, err := git.Open(st, memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{client: New(Options{}), repo: repo, branch: "main"}

	// Log: Head() ok, repo.Log(From: bogus) errors → error return.
	if _, err := r.Log(10); err == nil {
		t.Fatal("Log should fail when HEAD's commit object is missing")
	}
	// countSince: Log(From: bogus) errors → returns 0.
	if n := r.countSince(bogus, plumbing.ZeroHash); n != 0 {
		t.Fatalf("countSince with unresolvable head = %d, want 0", n)
	}
}
