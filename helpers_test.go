// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

import (
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func memfsNew() billy.Filesystem { return memfs.New() }

// commitOnlyRepo returns an in-memory repo with exactly one commit on
// main and no configured remote — the shape the aheadBehind defensive
// branches need.
func commitOnlyRepo(t *testing.T) *git.Repository {
	t.Helper()
	fs := memfs.New()
	st := memory.NewStorage()
	repo, err := git.Init(st, fs)
	if err != nil {
		t.Fatal(err)
	}
	// go-git defaults HEAD to refs/heads/master; point it at main so the
	// seeded commit lands on the branch the tests reference.
	if err := st.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	if err := billyutil.WriteFile(fs, "f.txt", []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "seed@x", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}
