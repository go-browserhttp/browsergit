// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Native only: these drive fake forges over the loopback with HTTPTransport.
//go:build !(js && wasm)

package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

var kinds = []Kind{GitHub, GitLab, Gitea}

var seed = map[string]string{
	"README.md":        "# repo\n",
	"deck/slides.md":   "# one\n",
	"deck/img/a.txt":   "a\n",
	"deck/notes.txt":   "bye\n",
	"other/ignore.txt": "x\n",
}

func newRemote(t *testing.T, f *fake, token, prefix string) remote.Remote {
	t.Helper()
	r, err := New(Config{Kind: f.kind, API: f.api(), Token: secret.New(token), BranchPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadProposeRefresh(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			f := newFake(t, k, seed, hostile{})
			r := newRemote(t, f, goodToken, "slides/")
			ctx := context.Background()
			files, base, err := r.Load(ctx, "o/r", "", "deck")
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 3 || string(files["slides.md"]) != "# one\n" || base.Ref != "main" || base.Forge != string(k) {
				t.Fatalf("load = %v %+v", files, base)
			}
			mainBefore := f.files("main")

			files["slides.md"] = []byte("# one, edited\n")
			files["new/added.md"] = []byte("added\n")
			delete(files, "notes.txt")
			res, err := r.Propose(ctx, base, files, remote.Proposal{Branch: "slides/edit", Title: "Edit", Body: "Because."})
			if err != nil {
				t.Fatal(err)
			}
			if res.Branch != "slides/edit" || !remote.IsCommitID(res.Commit) || !strings.HasPrefix(res.URL, f.srv.URL+"/o/r/") {
				t.Fatalf("result = %+v", res)
			}
			got := f.files("slides/edit")
			want := map[string]string{"README.md": "# repo\n", "deck/slides.md": "# one, edited\n", "deck/img/a.txt": "a\n", "deck/new/added.md": "added\n", "other/ignore.txt": "x\n"}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("branch content\n got %v\nwant %v", got, want)
			}
			if fmt.Sprint(f.files("main")) != fmt.Sprint(mainBefore) {
				t.Fatal("main changed")
			}
			if len(f.prs) != 1 {
				t.Fatalf("prs = %v", f.prs)
			}

			// Refresh: unchanged, then moved by someone else.
			theirs, same, err := r.Refresh(ctx, base)
			if err != nil || theirs != nil || same.Commit != base.Commit {
				t.Fatalf("refresh unchanged = %v %v", theirs, err)
			}
			f.advance(map[string]string{"deck/slides.md": "# theirs\n"})
			theirs, moved, err := r.Refresh(ctx, base)
			if err != nil || string(theirs["slides.md"]) != "# theirs\n" || moved.Commit == base.Commit {
				t.Fatalf("refresh moved = %v %+v %v", theirs, moved, err)
			}
		})
	}
}

func TestExistingAndProtectedBranchesAreNeverWritten(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			f := newFake(t, k, seed, hostile{})
			r := newRemote(t, f, goodToken, "")
			ctx := context.Background()
			f.mu.Lock()
			f.branches["feature"] = f.branches["main"]
			f.branches["slides/taken"] = f.branches["main"]
			f.mu.Unlock()
			files, base, err := r.Load(ctx, "o/r", "feature", "deck")
			if err != nil {
				t.Fatal(err)
			}
			files["slides.md"] = []byte("x")
			for _, b := range []string{"slides/taken", "main", "MAIN", "feature"} {
				f.mu.Lock()
				f.writes = 0
				f.mu.Unlock()
				_, err := r.Propose(ctx, base, files, remote.Proposal{Branch: b, Title: "t"})
				if !errors.Is(err, remote.ErrBranchExists) && !errors.Is(err, remote.ErrBadBranch) {
					t.Errorf("branch %q: %v", b, err)
				}
				if f.writes != 0 {
					t.Errorf("branch %q: %d write requests made", b, f.writes)
				}
			}
		})
	}
}

func TestTheTokenGoesOnlyInTheHeaderAndOnlyToTheAPI(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			var stolen atomic.Int32
			thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stolen.Add(1)
			}))
			defer thief.Close()

			f := newFake(t, k, seed, hostile{})
			r := newRemote(t, f, goodToken, "")
			files, base, err := r.Load(context.Background(), "o/r", "", "")
			if err != nil {
				t.Fatal(err)
			}
			files["README.md"] = []byte("changed")
			if _, err := r.Propose(context.Background(), base, files, remote.Proposal{Branch: "x/y", Title: "t"}); err != nil {
				t.Fatal(err)
			}
			for i, rq := range f.requests {
				if strings.Contains(rq, goodToken) {
					t.Fatalf("token in a URL: %s", rq)
				}
				if !f.authOK(f.auth[i]) {
					t.Fatalf("request %s had Authorization %q", rq, f.auth[i])
				}
			}

			// A redirecting API: the request is not followed, the thief sees nothing.
			f2 := newFake(t, k, seed, hostile{redirectTo: thief.URL})
			_, _, err = newRemote(t, f2, goodToken, "").Load(context.Background(), "o/r", "", "")
			if err == nil || stolen.Load() != 0 {
				t.Fatalf("redirect: err=%v, thief saw %d requests", err, stolen.Load())
			}
		})
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			// The server echoes whatever Authorization it got into every error.
			f := newFake(t, k, seed, hostile{echoToken: true})
			wrong := "WRONG-TOKEN-PLACEHOLDER-123"
			_, _, err := newRemote(t, f, wrong, "").Load(context.Background(), "o/r", "", "")
			if !errors.Is(err, remote.ErrAuth) {
				t.Fatalf("want ErrAuth, got %v", err)
			}
			if strings.Contains(err.Error(), wrong) {
				t.Fatalf("token in error: %v", err)
			}
			if !strings.Contains(err.Error(), secret.Redacted) {
				t.Fatalf("control: the echo should have been redacted, not absent: %v", err)
			}
			// Not found, with the right token, also echoed.
			_, _, err = newRemote(t, f, goodToken, "").Load(context.Background(), "o/missing", "", "")
			if !errors.Is(err, remote.ErrNotFound) || strings.Contains(err.Error(), goodToken) {
				t.Fatalf("not found: %v", err)
			}
		})
	}
}

func TestHostileResponses(t *testing.T) {
	cases := []struct {
		name string
		h    hostile
		want error
	}{
		{"path escaping the repo", hostile{extraEntry: "../evil"}, remote.ErrBadPath},
		{"git control dir", hostile{extraEntry: ".git/hooks/post-checkout"}, remote.ErrBadPath},
		{"blob not matching its id", hostile{wrongBlob: true}, remote.ErrBadResponse},
		{"enormous blob", hostile{hugeBlob: true}, remote.ErrTooLarge},
	}
	for _, k := range kinds {
		for _, c := range cases {
			t.Run(string(k)+"/"+c.name, func(t *testing.T) {
				f := newFake(t, k, seed, c.h)
				_, _, err := newRemote(t, f, goodToken, "").Load(context.Background(), "o/r", "", "")
				if !errors.Is(err, c.want) {
					t.Fatalf("got %v, want %v", err, c.want)
				}
			})
		}
	}
}

func TestPullRequestLinkMustStayOnTheForge(t *testing.T) {
	for _, k := range kinds {
		for _, bad := range []string{"javascript:alert(1)", "https://evil.example/o/r/pull/1", "data:text/html,x"} {
			t.Run(string(k)+"/"+bad, func(t *testing.T) {
				f := newFake(t, k, seed, hostile{prURL: bad})
				r := newRemote(t, f, goodToken, "")
				files, base, err := r.Load(context.Background(), "o/r", "", "")
				if err != nil {
					t.Fatal(err)
				}
				files["README.md"] = []byte("changed")
				res, err := r.Propose(context.Background(), base, files, remote.Proposal{Branch: "s/x", Title: "t"})
				if !errors.Is(err, remote.ErrBadResponse) || res.URL != "" {
					t.Fatalf("link %q: res=%+v err=%v", bad, res, err)
				}
			})
		}
	}
}

func TestPullRequestFailureReportsThePushedBranch(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			f := newFake(t, k, seed, hostile{failPR: true})
			r := newRemote(t, f, goodToken, "")
			files, base, _ := r.Load(context.Background(), "o/r", "", "")
			files["README.md"] = []byte("changed")
			res, err := r.Propose(context.Background(), base, files, remote.Proposal{Branch: "s/x", Title: "t"})
			if !errors.Is(err, remote.ErrConflict) || res.Branch != "s/x" || !strings.Contains(err.Error(), "s/x") {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
}

func TestGiteaRefusesAStaleBase(t *testing.T) {
	f := newFake(t, Gitea, seed, hostile{})
	r := newRemote(t, f, goodToken, "")
	files, base, _ := r.Load(context.Background(), "o/r", "", "")
	f.advance(map[string]string{"README.md": "theirs"})
	files["deck/slides.md"] = []byte("mine")
	if _, err := r.Propose(context.Background(), base, files, remote.Proposal{Branch: "s/x", Title: "t"}); !errors.Is(err, remote.ErrStale) {
		t.Fatal(err)
	}
}

func TestGitHubAndGitLabBranchFromTheLoadedCommit(t *testing.T) {
	for _, k := range []Kind{GitHub, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := newFake(t, k, seed, hostile{})
			r := newRemote(t, f, goodToken, "")
			files, base, _ := r.Load(context.Background(), "o/r", "", "")
			f.advance(map[string]string{"README.md": "theirs"})
			files["deck/slides.md"] = []byte("mine")
			if _, err := r.Propose(context.Background(), base, files, remote.Proposal{Branch: "s/x", Title: "t"}); err != nil {
				t.Fatal(err)
			}
			// The branch carries our change on the base we saw, not theirs.
			if got := f.files("s/x")["README.md"]; got != "# repo\n" {
				t.Fatalf("README on the branch = %q", got)
			}
		})
	}
}

func TestConfig(t *testing.T) {
	bad := []Config{
		{Kind: GitHub, API: "http://api.github.com"},
		{Kind: GitHub, API: "https://u:p@api.github.com"},
		{Kind: GitHub, API: "https://api.github.com?x=1"},
		{Kind: GitHub, API: "https://api.github.com", Web: "http://github.com"},
		{Kind: GitHub, API: "::"},
		{Kind: "svn", API: "https://x.example"},
	}
	for _, c := range bad {
		if _, err := New(c); !errors.Is(err, remote.ErrNotConfigured) {
			t.Errorf("New(%+v) = %v", c, err)
		}
	}
	c, err := newClient(Config{Kind: GitHub, API: "https://api.github.com/"})
	if err != nil || c.web.String() != "https://github.com" {
		t.Fatalf("derived web = %v %v", c.web, err)
	}
	c, _ = newClient(Config{Kind: Gitea, API: "https://codeberg.org/api/v1"})
	if c.web.String() != "https://codeberg.org" {
		t.Fatalf("derived web = %v", c.web)
	}
	// The caller's client is copied, not modified, and still never redirected.
	hc := &http.Client{}
	_, err = HTTPTransport{Client: hc}.RoundTrip(context.Background(), &Request{Method: "GET", URL: "http://127.0.0.1:1/", Limit: 1})
	if hc.CheckRedirect != nil || err == nil {
		t.Fatalf("the caller's http.Client was modified, or a closed port answered: %v", err)
	}
	if statusText(404) != "Not Found" || statusText(302) != "Redirect" || statusText(503) != "Server Error" || statusText(299) != "HTTP error" {
		t.Fatal("statusText")
	}
}

// A Transport that does not bound the body is bounded by the client anyway.
type unbounded struct{}

func (unbounded) RoundTrip(context.Context, *Request) (*Response, error) {
	return &Response{Status: 200, Header: map[string]string{}, Body: make([]byte, 100)}, nil
}

func TestABodyOverTheLimitIsRefusedWhateverTheTransport(t *testing.T) {
	r, err := New(Config{Kind: GitHub, API: "https://api.github.com", Transport: unbounded{}, MaxResponseBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Load(context.Background(), "o/r", "main", ""); !errors.Is(err, remote.ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestInputValidationBeforeAnyRequest(t *testing.T) {
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			f := newFake(t, k, seed, hostile{})
			r := newRemote(t, f, goodToken, "slides/")
			ctx := context.Background()
			base := remote.Base{Repo: "o/r", Ref: "main", Commit: strings.Repeat("a", 40)}
			checks := []error{
				func() error { _, _, e := r.Load(ctx, "../x", "", ""); return e }(),
				func() error { _, _, e := r.Load(ctx, "o/r", "", "../x"); return e }(),
				func() error { _, _, e := r.Load(ctx, "o/r", "a..b", ""); return e }(),
				func() error { _, _, e := r.Refresh(ctx, remote.Base{Repo: "x"}); return e }(),
				func() error { _, _, e := r.Refresh(ctx, remote.Base{Repo: "o/r", Ref: "a..b"}); return e }(),
				func() error {
					_, e := r.Propose(ctx, base, remote.Files{"a": nil}, remote.Proposal{Branch: "other/x", Title: "t"})
					return e
				}(),
				func() error {
					_, e := r.Propose(ctx, base, remote.Files{".git/config": nil}, remote.Proposal{Branch: "slides/x", Title: "t"})
					return e
				}(),
				func() error {
					_, e := r.Propose(ctx, base, remote.Files{}, remote.Proposal{Branch: "slides/x", Title: "t"})
					return e
				}(),
			}
			for i, e := range checks {
				if e == nil {
					t.Errorf("check %d passed", i)
				}
			}
			if len(f.requests) != 0 {
				t.Fatalf("requests made for refused input: %v", f.requests)
			}
		})
	}
}

func TestTreeTruncationIsRefused(t *testing.T) {
	if _, err := parseGitHubTree([]byte(`{"truncated":true,"tree":[]}`)); !errors.Is(err, remote.ErrTooLarge) {
		t.Fatal(err)
	}
	es, err := parseGitHubTree([]byte(`{"tree":[{"path":"l","mode":"120000","type":"blob","sha":"x"},{"path":"s","mode":"160000","type":"commit"},{"path":"f","mode":"100755","type":"blob","sha":"y","size":3}]}`))
	if err != nil || len(es) != 1 || es[0].path != "f" || es[0].size != 3 {
		t.Fatalf("symlinks and submodules must be left out: %v %v", es, err)
	}
}

func FuzzParseTrees(f *testing.F) {
	f.Add([]byte(`{"tree":[{"path":"a","mode":"100644","type":"blob","sha":"x"}]}`))
	f.Add([]byte(`[{"id":"x","type":"blob","path":"a","mode":"100644"}]`))
	f.Add([]byte(`{"tree":[],"total_count":3}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseGitHubTree(data)
		_, _, _ = parseGitLabTree(data)
		_, _, _, _ = parseGiteaTree(data)
	})
}

func FuzzDecodeBase64Blob(f *testing.F) {
	f.Add("aGVsbG8K", "base64")
	f.Add("aGVs\nbG8K", "")
	f.Add("!!", "utf-8")
	f.Fuzz(func(t *testing.T, content, enc string) {
		b, err := decodeBase64Blob(content, enc, 1<<10)
		if err == nil && int64(len(b)) > 1<<10+3 {
			t.Fatalf("decoded %d bytes past the limit", len(b))
		}
	})
}
