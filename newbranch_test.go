// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Constrained like testserver_test.go, whose helpers it uses.
//go:build !js

package browsergit

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/storage/memory"
)

func revParse(t *testing.T, bare, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", rev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestPushNewBranchOnlyEverCreates(t *testing.T) {
	root := t.TempDir()
	bare := seedBareRepo(t, root, "demo", map[string]string{"dir/a.txt": "a\n"})
	srv := startOrigin(t, root, "")
	c := New(Options{BaseURL: srv.URL})
	ctx := context.Background()
	r, err := c.Clone(ctx, "demo.git", "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil || head != revParse(t, bare, "main") {
		t.Fatalf("Head = %q %v", head, err)
	}
	blobs, err := r.Blobs()
	if err != nil || len(blobs) != 2 || blobs["dir/a.txt"] == "" {
		t.Fatalf("Blobs = %v %v", blobs, err)
	}
	if err := r.RemoveFile("dir/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveFile("never/there"); err != nil {
		t.Fatalf("removing an absent path: %v", err)
	}
	if _, err := r.Commit("remove a"); err != nil {
		t.Fatal(err)
	}

	// The tracked branch and an empty name are refused without a request.
	for _, b := range []string{"main", ""} {
		if err := r.PushNewBranch(ctx, b); !errors.Is(err, ErrBranchExists) {
			t.Fatalf("PushNewBranch(%q) = %v", b, err)
		}
	}
	if err := r.PushNewBranch(ctx, "topic/x"); err != nil {
		t.Fatal(err)
	}
	if revParse(t, bare, "topic/x") == "" || revParse(t, bare, "main") != head {
		t.Fatal("the new branch must exist and main must not move")
	}
	if ok, err := r.RemoteHasBranch(ctx, "topic/x"); !ok || err != nil {
		t.Fatalf("RemoteHasBranch = %v %v", ok, err)
	}
	// A second push to the same name is refused before anything is sent.
	if err := r.PushNewBranch(ctx, "topic/x"); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("existing branch: %v", err)
	}
}

func TestPushNewBranchSurfacesTransportErrors(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "demo", nil)
	srv := startOrigin(t, root, "")
	r, err := New(Options{BaseURL: srv.URL}).Clone(context.Background(), "demo.git", "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := r.PushNewBranch(context.Background(), "x"); !errors.Is(err, ErrTransport) {
		t.Fatalf("closed origin: %v", err)
	}
	if _, err := r.RemoteHasBranch(context.Background(), "x"); !errors.Is(err, ErrTransport) {
		t.Fatalf("closed origin: %v", err)
	}
}

func TestHeadAndBlobsOnAnEmptyRepo(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	empty := &Repo{client: New(Options{}), repo: repo, branch: "main"}
	if _, err := empty.Head(); err == nil {
		t.Fatal("Head of an empty repo must fail")
	}
	if _, err := empty.Blobs(); err == nil {
		t.Fatal("Blobs of an empty repo must fail")
	}
	noRemote := &Repo{client: New(Options{}), repo: commitOnlyRepo(t), branch: "main"}
	if err := noRemote.PushNewBranch(context.Background(), "x"); err == nil {
		t.Fatal("PushNewBranch without a remote must fail")
	}
}
