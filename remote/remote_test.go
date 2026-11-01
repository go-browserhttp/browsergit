// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package remote

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckPath(t *testing.T) {
	good := []string{"a", "a/b.tex", "slides/deck.md", "x/.hidden", "dir/file.lock", "é/ü.txt", ".github/workflows/ci.yml"}
	for _, p := range good {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"", "/etc/passwd", "../x", "a/../../x", "a/./b", "./a", "a/", "a//b", "a\\b",
		".git", ".git/config", "a/.git/hooks/pre-commit", ".GIT/config", ".git./x", ".git /x",
		"a\x00b", "a\nb", "a\x7fb", "\u0085x", string([]byte{0xff, 0xfe}), strings.Repeat("a", MaxPathLen+1),
	}
	for _, p := range bad {
		if err := CheckPath(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("CheckPath(%q) = %v, want ErrBadPath", p, err)
		}
	}
	if CheckDir("") != nil || CheckDir("../x") == nil {
		t.Fatal("CheckDir")
	}
}

func TestCheckBranch(t *testing.T) {
	good := []string{"slides/x", "fix-1", "a.b", "user/deck-20261101-090000", "x@y"}
	for _, b := range good {
		if err := CheckBranch(b); err != nil {
			t.Errorf("CheckBranch(%q) = %v", b, err)
		}
	}
	// One per git-check-ref-format rule, plus ours.
	bad := []string{
		"", "HEAD", "-x", "/x", "x/", "a//b", // reserved, --branch, rule 6
		"a..b", "x.", "a@{b", "@", "a\\b", // rules 3, 7, 8, 9, 10
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\tb", "a\x7fb", // rules 4, 5
		".x", "a/.x", "x.lock", "a/x.lock/b", // rule 1
		"é", strings.Repeat("a", MaxBranchLen+1), // ours: ASCII only, bounded
	}
	for _, b := range bad {
		if err := CheckBranch(b); !errors.Is(err, ErrBadBranch) {
			t.Errorf("CheckBranch(%q) = %v, want ErrBadBranch", b, err)
		}
	}
	if err := CheckBranch("Main", "main"); !errors.Is(err, ErrBadBranch) {
		t.Fatal("a refused target must be refused case-insensitively")
	}
	if err := CheckBranch("x", ""); err != nil {
		t.Fatal("an empty refused entry must be ignored")
	}
}

func TestRequirePrefix(t *testing.T) {
	if RequirePrefix("anything", "") != nil {
		t.Fatal("no prefix, no constraint")
	}
	if RequirePrefix("slides/ada/x", "slides/ada/") != nil {
		t.Fatal("inside the prefix")
	}
	for _, b := range []string{"slides/bob/x", "slides/ada/", "main", "slides/adax"} {
		if err := RequirePrefix(b, "slides/ada/"); !errors.Is(err, ErrBadBranch) {
			t.Errorf("RequirePrefix(%q) = %v", b, err)
		}
	}
}

func TestCheckRepo(t *testing.T) {
	for _, r := range []string{"o/r", "go-tex/go-tex.github.io", "a_b/c.d"} {
		if CheckRepo(r, false) != nil {
			t.Errorf("CheckRepo(%q)", r)
		}
	}
	if CheckRepo("g/s/p", true) != nil || CheckRepo("g/s/p", false) == nil {
		t.Fatal("nested")
	}
	for _, r := range []string{"", "o", "o/", "/r", "o/r/", "o/../r", "o/r?x=1", "o/r#f", "o/.git", "o/a..b", "o r/x", "o/%2e%2e", strings.Repeat("a/", 21) + "a"} {
		if err := CheckRepo(r, true); !errors.Is(err, ErrBadRepo) {
			t.Errorf("CheckRepo(%q) = %v", r, err)
		}
	}
}

func TestBranchName(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"Fix the intro slide!":       "slides/fix-the-intro-slide-20261101-090000",
		"":                           "slides/change-20261101-090000",
		"Ünïcödé — only":             "slides/n-c-d-only-20261101-090000",
		"../../etc ~^:?*[ @{ x.lock": "slides/etc-x-lock-20261101-090000",
	}
	for in, want := range cases {
		if got := BranchName("slides/", in, now); got != want {
			t.Errorf("BranchName(%q) = %q, want %q", in, got, want)
		}
	}
	long := BranchName("p/", strings.Repeat("abc ", 100), now)
	if CheckBranch(long) != nil || len(long) > MaxBranchLen {
		t.Fatalf("long title gave %q", long)
	}
}

func FuzzBranchName(f *testing.F) {
	f.Add("slides/", "hello world")
	f.Add("", "..")
	f.Fuzz(func(t *testing.T, prefix, title string) {
		if CheckBranch(prefix+"x") != nil || strings.HasSuffix(prefix, "/") == false && prefix != "" {
			return // only well-formed prefixes ending in "/" are in contract
		}
		b := BranchName(prefix, title, time.Unix(0, 0))
		if len(b) <= MaxBranchLen {
			if err := CheckBranch(b); err != nil {
				t.Fatalf("BranchName(%q, %q) = %q: %v", prefix, title, b, err)
			}
		}
	})
}

func FuzzCheckPath(f *testing.F) {
	for _, s := range []string{"a/b", "../x", ".git/config", "a\\b", "/abs"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if CheckPath(p) != nil {
			return
		}
		// Whatever is accepted cannot leave its directory or name .git.
		if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "\x00") {
			t.Fatalf("accepted %q", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." || seg == "." || seg == "" || strings.EqualFold(seg, ".git") {
				t.Fatalf("accepted %q (segment %q)", p, seg)
			}
		}
	})
}

func FuzzCheckBranch(f *testing.F) {
	for _, s := range []string{"slides/x", "a..b", "@{", "x.lock", "-x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b string) {
		if CheckBranch(b) != nil {
			return
		}
		for _, bad := range []string{"..", "@{", "//", "\\", " ", "~", "^", ":", "?", "*", "["} {
			if strings.Contains(b, bad) {
				t.Fatalf("accepted %q containing %q", b, bad)
			}
		}
		if b == "HEAD" || b == "@" || strings.HasPrefix(b, "-") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, "/") {
			t.Fatalf("accepted %q", b)
		}
	})
}

func TestBlobID(t *testing.T) {
	// Values from `git hash-object`.
	if got := BlobID(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty blob = %s", got)
	}
	if got := BlobID([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hello blob = %s", got)
	}
}

func TestChanges(t *testing.T) {
	base := Base{Dir: "deck", Blobs: map[string]string{
		"same.md": BlobID([]byte("same")),
		"edit.md": BlobID([]byte("old")),
		"gone.md": BlobID([]byte("bye")),
	}}
	files := Files{"same.md": []byte("same"), "edit.md": []byte("new"), "add/new.md": []byte("hi")}
	ch, err := Changes(base, files, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path string
		op   Op
	}{{"deck/add/new.md", Create}, {"deck/edit.md", Update}, {"deck/gone.md", Delete}}
	if len(ch) != len(want) {
		t.Fatalf("changes = %+v", ch)
	}
	for i, w := range want {
		if ch[i].Path != w.path || ch[i].Op != w.op {
			t.Errorf("change %d = %s %s, want %s %s", i, ch[i].Op, ch[i].Path, w.op, w.path)
		}
	}
	if ch[1].OldBlob != base.Blobs["edit.md"] || ch[2].Data != nil {
		t.Fatal("old blob / delete data")
	}
	if _, err := Changes(base, Files{"../escape": nil}, Limits{}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("a deck path escaping its dir: %v", err)
	}
	if _, err := Changes(Base{Blobs: map[string]string{"../x": "y"}}, Files{}, Limits{}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("a stored base path escaping: %v", err)
	}
	if _, err := Changes(Base{Dir: "../d"}, Files{}, Limits{}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("a base dir escaping: %v", err)
	}
}

func TestLimits(t *testing.T) {
	l := Limits{MaxFiles: 2, MaxFileBytes: 3, MaxTotalBytes: 5}
	if l.CheckFiles(Files{"a": []byte("abc"), "b": []byte("ab")}) != nil {
		t.Fatal("within limits")
	}
	for name, fs := range map[string]Files{
		"files": {"a": nil, "b": nil, "c": nil},
		"file":  {"a": []byte("abcd")},
		"total": {"a": []byte("abc"), "b": []byte("abc")},
	} {
		if err := l.CheckFiles(fs); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if (Limits{}).OrDefault() != DefaultLimits {
		t.Fatal("OrDefault")
	}
}

func TestSmallHelpers(t *testing.T) {
	if Join("", "a") != "a" || Join("d", "a") != "d/a" {
		t.Fatal("Join")
	}
	if r, ok := Rel("d", "d/a/b"); !ok || r != "a/b" {
		t.Fatal("Rel inside")
	}
	if _, ok := Rel("d", "dx/a"); ok {
		t.Fatal("Rel must not match a sibling with the same prefix")
	}
	if r, ok := Rel("", "x"); !ok || r != "x" {
		t.Fatal("Rel root")
	}
	if Message(Proposal{Title: "t"}) != "t" || Message(Proposal{Title: "t", Body: "b"}) != "t\n\nb" {
		t.Fatal("Message")
	}
	if Op(9).String() != "op(9)" || Create.String() != "create" {
		t.Fatal("Op.String")
	}
	if !IsCommitID(strings.Repeat("a", 40)) || !IsCommitID(strings.Repeat("b", 64)) || IsCommitID("ABC") {
		t.Fatal("IsCommitID")
	}
}

func TestCheckProposal(t *testing.T) {
	base := Base{Ref: "main", Commit: strings.Repeat("a", 40)}
	if err := CheckProposal(base, Proposal{Branch: "slides/x", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckProposal(base, Proposal{Branch: "main", Title: "t"}); !errors.Is(err, ErrBadBranch) {
		t.Fatalf("proposing onto the loaded branch: %v", err)
	}
	if err := CheckProposal(base, Proposal{Branch: "x", Title: "t"}, "trunk"); err != nil {
		t.Fatal(err)
	}
	if err := CheckProposal(base, Proposal{Branch: "trunk", Title: "t"}, "trunk"); !errors.Is(err, ErrBadBranch) {
		t.Fatal("refused default branch")
	}
	if err := CheckProposal(base, Proposal{Branch: "x", Title: " "}); !errors.Is(err, ErrConflict) {
		t.Fatal("empty title")
	}
	if err := CheckProposal(Base{Ref: "main", Commit: "nope"}, Proposal{Branch: "x", Title: "t"}); !errors.Is(err, ErrBadResponse) {
		t.Fatal("bad commit")
	}
}
