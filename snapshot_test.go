// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

package browsergit

import (
	"context"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	billyutil "github.com/go-git/go-billy/v5/util"
)

// emptyTree is a filesystem with nothing in it, to snapshot an archive that is
// well-formed but holds no repository.
func emptyTree(t *testing.T) billy.Filesystem {
	t.Helper()
	return memfs.New()
}

// A browser tab that reloads used to lose its clone entirely: the repository
// lived in memory and memory is what a reload throws away. These tests pin the
// round trip that lets it survive — clone, write it down, open it again — and
// that what comes back is a working repository, not just its files.

func TestSnapshotRoundTrip(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "demo", map[string]string{"main.tex": "\\documentclass{article}\n"})
	srv := startOrigin(t, root, "")
	ctx := context.Background()

	c := New(Options{BaseURL: srv.URL, Author: "Ada Lovelace", Email: "ada@go-tex.local"})
	repo, err := c.Clone(ctx, "demo.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// An edit that is NOT committed must survive too: it is the reader's work.
	if err := repo.WriteFile("main.tex", []byte("\\documentclass{book}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	wantLog, err := repo.Log(10)
	if err != nil {
		t.Fatalf("log: %v", err)
	}

	snap, err := repo.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) == 0 {
		t.Fatal("snapshot is empty")
	}

	// Reopen from the bytes alone — no origin is contacted.
	back, err := c.Open(snap, "main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if back.Branch() != "main" {
		t.Fatalf("branch = %q, want main", back.Branch())
	}
	files, err := back.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 || files[0] != "README.md" || files[1] != "main.tex" {
		t.Fatalf("List = %v, want [README.md main.tex] with .git pruned", files)
	}
	if b, err := back.ReadFile("main.tex"); err != nil || string(b) != "\\documentclass{book}\n" {
		t.Fatalf("the uncommitted edit did not survive: read = %q, %v", b, err)
	}

	// The history came back with it, not just the files.
	gotLog, err := back.Log(10)
	if err != nil {
		t.Fatalf("log after restore: %v", err)
	}
	if len(gotLog) != len(wantLog) {
		t.Fatalf("history is %d commits after restore, want %d", len(gotLog), len(wantLog))
	}
	for i := range gotLog {
		if gotLog[i].Hash != wantLog[i].Hash {
			t.Fatalf("commit %d is %s after restore, want %s", i, gotLog[i].Hash, wantLog[i].Hash)
		}
	}

	// And it is a REPOSITORY, not a pile of files: the pending edit shows as a
	// modification against the restored history, and it can still be committed.
	st, err := back.Status()
	if err != nil {
		t.Fatalf("status after restore: %v", err)
	}
	if len(st.Changes) != 1 || st.Changes[0].Path != "main.tex" {
		t.Fatalf("status after restore = %+v, want main.tex modified", st.Changes)
	}
	if _, err := back.Commit("restored and committed"); err != nil {
		t.Fatalf("commit after restore: %v", err)
	}
	after, err := back.Log(10)
	if err != nil {
		t.Fatalf("log after commit: %v", err)
	}
	if len(after) != len(wantLog)+1 {
		t.Fatalf("history is %d after committing, want %d", len(after), len(wantLog)+1)
	}
}

func TestOpenRejectsRubbish(t *testing.T) {
	c := New(Options{BaseURL: "http://example.invalid"})
	if _, err := c.Open([]byte("this is not a tar archive"), "main"); err == nil {
		t.Fatal("Open accepted bytes that are not an archive")
	}
	// A well-formed archive with no repository in it is not a repository.
	empty, err := (&Repo{fs: emptyTree(t)}).Snapshot()
	if err != nil {
		t.Fatalf("snapshot of an empty tree: %v", err)
	}
	if _, err := c.Open(empty, ""); err == nil {
		t.Fatal("Open accepted an archive holding no repository")
	}
}

func TestSnapshotEmptyBranchDefaults(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "demo", nil)
	srv := startOrigin(t, root, "")
	c := New(Options{BaseURL: srv.URL})
	repo, err := c.Clone(context.Background(), "demo.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	snap, err := repo.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	back, err := c.Open(snap, "") // no branch given
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if back.Branch() != DefaultBranch {
		t.Fatalf("branch = %q, want the default %q", back.Branch(), DefaultBranch)
	}
	if !strings.HasPrefix(back.Branch(), "m") {
		t.Fatalf("unexpected default branch %q", back.Branch())
	}
}

// The guards below are the ones a healthy in-memory tree never reaches. They
// exist because a snapshot is written and read on a device we do not control —
// a browser's storage can be full, evicted, or corrupted — and a failure there
// must surface as an error, never as a half-restored repository.

// failChrootFS refuses to make the .git chroot.
type failChrootFS struct{ billy.Filesystem }

func (f failChrootFS) Chroot(string) (billy.Filesystem, error) { return nil, errInjected }

// failWriteFS accepts everything except writing a file.
type failWriteFS struct{ billy.Filesystem }

func (f failWriteFS) Create(string) (billy.File, error) { return nil, errInjected }

func (f failWriteFS) OpenFile(name string, flag int, perm fs.FileMode) (billy.File, error) {
	if flag&os.O_CREATE != 0 {
		return nil, errInjected
	}
	return f.Filesystem.OpenFile(name, flag, perm)
}

func (f failWriteFS) Chroot(p string) (billy.Filesystem, error) {
	sub, err := f.Filesystem.Chroot(p)
	if err != nil {
		return nil, err
	}
	return failWriteFS{sub}, nil
}

// failWriter fails on the first byte written to it.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errInjected }

func TestNewTreeChrootFailure(t *testing.T) {
	if _, _, err := newTree(failChrootFS{memfs.New()}); err == nil {
		t.Fatal("newTree ignored a filesystem that cannot make .git")
	}
	c := New(Options{BaseURL: "http://example.invalid"})
	if _, err := c.openInto(failChrootFS{memfs.New()}, nil, "main"); err == nil {
		t.Fatal("Open ignored a filesystem that cannot make .git")
	}
}

func TestSnapshotWriteFailures(t *testing.T) {
	fsys := memfs.New()
	if err := billyutil.WriteFile(fsys, "a.tex", []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := &Repo{fs: fsys}
	if err := r.snapshotTo(failWriter{}); err == nil {
		t.Fatal("a snapshot onto a failing writer reported success")
	}
	// An unreadable directory must fail the walk, not silently skip it.
	if err := (&Repo{fs: &failReadDirFS{Filesystem: fsys, armed: true}}).snapshotTo(io.Discard); err == nil {
		t.Fatal("a snapshot over an unreadable tree reported success")
	}
}

func TestSnapshotSkipsNonRegularEntries(t *testing.T) {
	fsys := memfs.New()
	if err := billyutil.WriteFile(fsys, "real.tex", []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := fsys.Symlink("real.tex", "link.tex"); err != nil {
		t.Skipf("this billy build has no symlinks: %v", err)
	}
	snap, err := (&Repo{fs: fsys}).Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if strings.Contains(string(snap), "link.tex") {
		t.Fatal("the symlink was archived; a snapshot carries regular files only")
	}
}

func TestOpenRestoreFailures(t *testing.T) {
	c := New(Options{BaseURL: "http://example.invalid"})

	// A truncated archive: the header promises bytes the reader cannot deliver.
	fsys := memfs.New()
	if err := billyutil.WriteFile(fsys, "a.tex", []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	full, err := (&Repo{fs: fsys}).Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := c.Open(full[:len(full)-2048], "main"); err == nil {
		t.Fatal("Open accepted a truncated archive")
	}

	// Storage that refuses the write.
	if _, err := c.openInto(failWriteFS{memfs.New()}, full, "main"); err == nil {
		t.Fatal("Open reported success on a filesystem that refuses writes")
	}
}

// failReadDirAt fails ReadDir for one chosen path only, so the recursive descent
// can be broken below the root rather than at it.
type failReadDirAt struct {
	billy.Filesystem
	at string
}

func (f failReadDirAt) ReadDir(p string) ([]os.FileInfo, error) {
	if p == f.at {
		return nil, errInjected
	}
	return f.Filesystem.ReadDir(p)
}

// failOpenFS refuses to open one file for reading, so a directory entry can be
// listed and then turn out to be unreadable.
type failOpenFS struct {
	billy.Filesystem
	at string
}

func (f failOpenFS) Open(p string) (billy.File, error) {
	if p == f.at {
		return nil, errInjected
	}
	return f.Filesystem.Open(p)
}

func (f failOpenFS) OpenFile(p string, flag int, perm fs.FileMode) (billy.File, error) {
	if p == f.at {
		return nil, errInjected
	}
	return f.Filesystem.OpenFile(p, flag, perm)
}

// failAfterWriter accepts n bytes and then fails, to break a tar write after its
// header has gone through.
type failAfterWriter struct{ left int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, errInjected
	}
	if len(p) > w.left {
		n := w.left
		w.left = 0
		return n, errInjected
	}
	w.left -= len(p)
	return len(p), nil
}

func TestCloneTreePreparationFailure(t *testing.T) {
	c := New(Options{BaseURL: "http://example.invalid"})
	c.newRoot = func() billy.Filesystem { return failChrootFS{memfs.New()} }
	if _, err := c.Clone(context.Background(), "demo.git", "main", 1); err == nil {
		t.Fatal("Clone ignored a filesystem that cannot make .git")
	}
}

func TestSnapshotSurfacesEveryWriteFault(t *testing.T) {
	// Reported through Snapshot itself, not only through the seam.
	if _, err := (&Repo{fs: &failReadDirFS{Filesystem: memfs.New(), armed: true}}).Snapshot(); err == nil {
		t.Fatal("Snapshot reported success over an unreadable tree")
	}
	// Nothing to write, so the failure lands on the archive trailer.
	if err := (&Repo{fs: memfs.New()}).snapshotTo(failWriter{}); err == nil {
		t.Fatal("Snapshot ignored a writer that refused the trailer")
	}

	seed := func() billy.Filesystem {
		fsys := memfs.New()
		if err := billyutil.WriteFile(fsys, "sub/a.tex", []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return fsys
	}
	// A subdirectory that cannot be listed breaks the descent, not just the root.
	if err := (&Repo{fs: failReadDirAt{seed(), "sub"}}).snapshotTo(io.Discard); err == nil {
		t.Fatal("Snapshot skipped an unreadable subdirectory instead of failing")
	}
	// A file that lists but cannot be read.
	if err := (&Repo{fs: failOpenFS{seed(), "sub/a.tex"}}).snapshotTo(io.Discard); err == nil {
		t.Fatal("Snapshot skipped an unreadable file instead of failing")
	}
	// A writer that takes the header and then dies mid-payload.
	if err := (&Repo{fs: seed()}).snapshotTo(&failAfterWriter{left: 512}); err == nil {
		t.Fatal("Snapshot ignored a writer that failed after the header")
	}
}
