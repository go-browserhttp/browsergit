// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package gitd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-browserhttp/browsergit/forge"
	"github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

const (
	testCSRF   = "csrf-token-0123456789abcdef"
	testOrigin = "https://slides.example.org"
	testCommit = "0123456789abcdef0123456789abcdef01234567"
)

// fake is a slides-gitd stand-in that enforces the same request rules.
type fake struct {
	mu       sync.Mutex
	session  bool // a session exists (the cookie is valid)
	identity bool // the proxy vouches for a user
	seen     []string
	rules    []string // rule violations found
	reply    func(path string, body map[string]any) (int, any)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !f.identity {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
		return
	}
	if r.URL.Path == "/api/git/session" {
		if _, err := r.Cookie("gitd"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "gitd", Value: "s1", Path: "/", HttpOnly: true})
			f.session = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user": "sub-1", "csrf": testCSRF, "repos": []string{"github:lab/talks", "gitlab:team/sub/slides"}})
		return
	}
	f.seen = append(f.seen, r.URL.Path)
	if r.Method != "POST" || r.Header.Get("Origin") != testOrigin || r.Header.Get("X-Requested-With") != "slides-gitd" ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		f.rules = append(f.rules, "csrf headers: "+r.URL.Path)
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"csrf"}`))
		return
	}
	if c, err := r.Cookie("gitd"); err != nil || !f.session || c.Value != "s1" {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"no-session"}`))
		return
	}
	if r.Header.Get("X-CSRF-Token") != testCSRF {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"csrf"}`))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	code, out := f.reply(r.URL.Path, body)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

// recorder wraps a Transport and records that every request is SameOrigin.
type recorder struct {
	forge.Transport
	mu    sync.Mutex
	other []string
}

func (t *recorder) RoundTrip(ctx context.Context, r *forge.Request) (*forge.Response, error) {
	if !r.SameOrigin {
		t.mu.Lock()
		t.other = append(t.other, r.URL)
		t.mu.Unlock()
	}
	return t.Transport.RoundTrip(ctx, r)
}

func newClient(t *testing.T, f *fake) (*Client, *recorder) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	rec := &recorder{Transport: forge.HTTPTransport{Client: &http.Client{Jar: jar}}}
	c, err := New(Config{Base: srv.URL + "/api/git", Origin: testOrigin, Transport: rec})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func okFake() *fake {
	f := &fake{identity: true}
	f.reply = func(path string, body map[string]any) (int, any) {
		switch path {
		case "/api/git/load":
			return 200, map[string]any{
				"files": map[string][]byte{"talk/deck.md": []byte("# hi\n")},
				"base":  remote.Base{Forge: "github", Repo: body["repo"].(string), Ref: "main", Dir: body["dir"].(string), Commit: testCommit, Blobs: map[string]string{"deck.md": strings.Repeat("a", 40)}},
			}
		case "/api/git/propose":
			return 200, map[string]any{"branch": "slides/ada-0123456789/update-the-slides-20260101-000000", "commit": testCommit, "url": "https://github.com/lab/talks/pull/7"}
		case "/api/git/refresh":
			return 200, map[string]any{"files": nil, "base": remote.Base{Forge: "github", Repo: "lab/talks", Ref: "main", Commit: testCommit}}
		case "/api/git/credential", "/api/git/credential/delete":
			return 200, map[string]any{"stored": true}
		}
		return 404, map[string]any{"error": "not-found"}
	}
	return f
}

func TestSessionAndRemote(t *testing.T) {
	f := okFake()
	c, rec := newClient(t, f)
	ctx := context.Background()
	info, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.User != "sub-1" || len(info.Repos) != 2 || info.Repos[1].String() != "gitlab:team/sub/slides" {
		t.Fatalf("info %+v", info)
	}
	r := c.Remote("github")
	files, base, err := r.Load(ctx, "lab/talks", "main", "")
	if err != nil || string(files["talk/deck.md"]) != "# hi\n" || base.Commit != testCommit {
		t.Fatalf("load: %v %v %+v", err, files, base)
	}
	res, err := r.Propose(ctx, base, files, remote.Proposal{Branch: "ignored", Title: "t", Body: "b"})
	if err != nil || !strings.HasPrefix(res.Branch, "slides/ada-") || res.URL != "https://github.com/lab/talks/pull/7" {
		t.Fatalf("propose: %v %+v", err, res)
	}
	got, same, err := r.Refresh(ctx, base)
	if err != nil || got != nil || same.Commit != base.Commit {
		t.Fatalf("refresh: %v %v %+v", err, got, same)
	}
	if err := c.StoreCredential(ctx, "gitlab", secret.New("tok")); err != nil {
		t.Fatal(err)
	}
	if err := c.ForgetCredential(ctx, "gitlab"); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) > 0 {
		t.Fatalf("rule violations: %v", f.rules)
	}
	if len(rec.other) > 0 {
		t.Fatalf("requests that were not same-origin: %v", rec.other)
	}
}

func TestRefreshMoved(t *testing.T) {
	f := okFake()
	f.reply = func(string, map[string]any) (int, any) {
		return 200, map[string]any{"files": map[string][]byte{"deck.md": []byte("x")}, "base": remote.Base{Forge: "github", Repo: "lab/talks", Ref: "main", Commit: strings.Repeat("b", 40)}}
	}
	c, _ := newClient(t, f)
	files, head, err := c.Remote("github").Refresh(context.Background(), remote.Base{Repo: "lab/talks", Ref: "main", Commit: testCommit})
	if err != nil || files == nil || head.Commit != strings.Repeat("b", 40) {
		t.Fatalf("%v %v %+v", err, files, head)
	}
}

func TestDiscovery(t *testing.T) {
	for name, tc := range map[string]struct {
		h    http.HandlerFunc
		want error
	}{
		"a static host's 404": {func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }, ErrNoServer},
		"HTML on every path": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>app</html>"))
		}, ErrNoServer},
		"JSON that is not a session": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hello":"world"}`))
		}, ErrNoServer},
		"a CSRF token too short": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user":"u","csrf":"x","repos":[]}`))
		}, ErrNoServer},
		"a repository that is not one": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user":"u","csrf":"0123456789abcdef","repos":["github:../x"]}`))
		}, ErrNoServer},
		"signed out": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, ErrSignIn},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			c, _ := New(Config{Base: srv.URL + "/api/git"})
			if _, err := c.Session(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := New(Config{Base: srv.URL})
	srv.Close()
	if _, err := c.Session(context.Background()); !errors.Is(err, remote.ErrTransport) {
		t.Fatalf("a dead server: %v", err)
	}
}

func TestSignInEndsTheSessionAndNeverRetries(t *testing.T) {
	f := okFake()
	c, _ := newClient(t, f)
	ctx := context.Background()
	if _, err := c.Session(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.identity = false
	f.mu.Unlock()
	_, _, err := c.Remote("github").Load(ctx, "lab/talks", "", "")
	if !errors.Is(err, ErrSignIn) || !errors.Is(err, remote.ErrAuth) {
		t.Fatalf("got %v", err)
	}
	if !c.csrf.IsZero() {
		t.Fatal("the CSRF token outlived the sign-in")
	}
}

func TestLostSessionIsRestoredOnce(t *testing.T) {
	f := okFake()
	c, _ := newClient(t, f)
	ctx := context.Background()
	if _, err := c.Session(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.session = false // the server restarted
	f.mu.Unlock()
	// the cookie in the jar is stale, the session endpoint re-creates it on a
	// missing cookie only: drop the server's memory of it and serve the next call
	f.reply = okFake().reply
	if _, _, err := c.Remote("github").Load(ctx, "lab/talks", "", ""); err == nil {
		t.Fatal("a call on a lost session must fail when the session cannot be re-established")
	}
	// the retry did happen (2 attempts), and not more
	n := 0
	for _, p := range f.seen {
		if p == "/api/git/load" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("load attempts: %d, want 2", n)
	}
}

func TestServerErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
		want   error
	}{
		{428, "no-credential", ErrNoCredential},
		{403, "denied", remote.ErrForbidden},
		{429, "rate-limited", remote.ErrRateLimited},
		{502, "forge-auth", remote.ErrAuth},
		{404, "not-found", remote.ErrNotFound},
		{409, "conflict", remote.ErrConflict},
		{400, "refused", ErrRefused},
		{502, "forge", remote.ErrTransport},
		{500, "", remote.ErrTransport},
	} {
		f := okFake()
		f.reply = func(string, map[string]any) (int, any) { return tc.status, map[string]any{"error": tc.class} }
		c, _ := newClient(t, f)
		_, _, err := c.Remote("github").Load(context.Background(), "lab/talks", "", "")
		if !errors.Is(err, tc.want) {
			t.Errorf("%d %s: got %v, want %v", tc.status, tc.class, err, tc.want)
		}
	}
}

func TestAnErrorMessageUsesTheClassOnly(t *testing.T) {
	f := okFake()
	f.reply = func(string, map[string]any) (int, any) {
		return 502, map[string]any{"error": "forge", "message": "token ghp_SECRET leaked"}
	}
	c, _ := newClient(t, f)
	_, _, err := c.Remote("github").Load(context.Background(), "lab/talks", "", "")
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), testCSRF) {
		t.Fatalf("%v", err)
	}
	// a class that is not a short lower-case word is dropped
	if errClass([]byte(`{"error":"<script>"}`)) != "" || errClass([]byte(`{"error":"`+strings.Repeat("a", 40)+`"}`)) != "" || errClass([]byte("x")) != "" || errClass([]byte(`{"error":""}`)) != "" {
		t.Fatal("errClass accepted a body it should not")
	}
}

func TestHostileAnswersAreRefused(t *testing.T) {
	for name, reply := range map[string]func(map[string]any) map[string]any{
		"a path that leaves the folder": func(b map[string]any) map[string]any {
			return map[string]any{"files": map[string][]byte{"../etc/passwd": []byte("x")}, "base": remote.Base{Repo: "lab/talks", Commit: testCommit}}
		},
		"another repository": func(b map[string]any) map[string]any {
			return map[string]any{"files": map[string][]byte{"a": []byte("x")}, "base": remote.Base{Repo: "other/repo", Commit: testCommit}}
		},
		"no commit": func(b map[string]any) map[string]any {
			return map[string]any{"files": map[string][]byte{"a": []byte("x")}, "base": remote.Base{Repo: "lab/talks"}}
		},
		"a blob path that leaves the folder": func(b map[string]any) map[string]any {
			return map[string]any{"files": map[string][]byte{"a": []byte("x")}, "base": remote.Base{Repo: "lab/talks", Commit: testCommit, Blobs: map[string]string{"../x": "a"}}}
		},
	} {
		f := okFake()
		f.reply = func(_ string, b map[string]any) (int, any) { return 200, reply(b) }
		c, _ := newClient(t, f)
		if _, _, err := c.Remote("github").Load(context.Background(), "lab/talks", "", ""); !errors.Is(err, remote.ErrBadResponse) && !errors.Is(err, remote.ErrBadPath) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, out := range map[string]map[string]any{
		"a branch outside slides/": {"branch": "main", "commit": testCommit},
		"a javascript link":        {"branch": "slides/a-0123456789/t", "url": "javascript:alert(1)"},
		"a link with a password":   {"branch": "slides/a-0123456789/t", "url": "https://u:p@github.com/x"},
		"the base's own branch":    {"branch": "slides/a-0123456789/t", "url": "https://x/y", "commit": testCommit},
	} {
		f := okFake()
		f.reply = func(string, map[string]any) (int, any) { return 200, out }
		c, _ := newClient(t, f)
		base := remote.Base{Repo: "lab/talks", Ref: "main", Commit: testCommit}
		_, err := c.Remote("github").Propose(context.Background(), base, remote.Files{"a": []byte("x")}, remote.Proposal{Title: "t"})
		if name == "the base's own branch" {
			if err != nil {
				t.Errorf("%s: control failed: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, remote.ErrBadResponse) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLocalRefusals(t *testing.T) {
	f := okFake()
	c, _ := newClient(t, f)
	ctx := context.Background()
	r := c.Remote("github")
	if _, _, err := r.Load(ctx, "../x", "", ""); !errors.Is(err, remote.ErrBadRepo) {
		t.Errorf("repo: %v", err)
	}
	if _, _, err := r.Load(ctx, "a/b", "", "../x"); err == nil {
		t.Error("dir")
	}
	if _, err := r.Propose(ctx, remote.Base{Repo: "a/b"}, nil, remote.Proposal{}); err == nil {
		t.Error("a base with no commit")
	}
	if _, err := r.Propose(ctx, remote.Base{Repo: "a/b", Commit: testCommit}, remote.Files{"../x": nil}, remote.Proposal{}); err == nil {
		t.Error("a path")
	}
	if _, _, err := r.Refresh(ctx, remote.Base{Repo: "../x"}); err == nil {
		t.Error("refresh repo")
	}
	if len(f.seen) != 0 {
		t.Errorf("refused requests reached the server: %v", f.seen)
	}
}

func TestBase(t *testing.T) {
	for _, ok := range []string{"", "/api/git", "https://slides.example.org/api/git", "http://127.0.0.1:8080/api/git", "http://localhost/x"} {
		if _, err := New(Config{Base: ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"//evil.example/api", "http://evil.example/api", "https://u:p@x/api", "https://x/api?a=b", "ftp://x", "api/git", "/a?b", `/a\b`} {
		if _, err := New(Config{Base: bad}); !errors.Is(err, remote.ErrNotConfigured) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestTheTokenIsNotPrintable(t *testing.T) {
	f := okFake()
	c, _ := newClient(t, f)
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v", c.csrf), fmt.Sprintf("%+v", c.csrf), fmt.Sprintf("%#v", c.csrf)} {
		if strings.Contains(s, testCSRF) {
			t.Fatalf("printed: %s", s)
		}
	}
	c.Forget()
	if !c.csrf.IsZero() {
		t.Fatal("Forget")
	}
}

func TestTransportFailureOnACall(t *testing.T) {
	f := okFake()
	c, _ := newClient(t, f)
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Remote("github").Load(ctx, "lab/talks", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func FuzzParseSession(f *testing.F) {
	f.Add([]byte(`{"user":"u","csrf":"0123456789abcdef","repos":["github:a/b"]}`))
	f.Add([]byte(`{"user":"u"}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		info, tok, err := parseSession(b)
		if err == nil && (info.User == "" || tok.IsZero()) {
			t.Fatalf("accepted a session with no user or token: %q", b)
		}
		tok.Forget()
	})
}

func FuzzDecodeLoad(f *testing.F) {
	f.Add([]byte(`{"files":{"a":"eA=="},"base":{"repo":"a/b","commit":"0123456789abcdef0123456789abcdef01234567"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		fk := okFake()
		fk.reply = func(string, map[string]any) (int, any) { return 200, json.RawMessage(b) }
		srv := httptest.NewServer(fk)
		defer srv.Close()
		jar, _ := cookiejar.New(nil)
		c, _ := New(Config{Base: srv.URL + "/api/git", Origin: testOrigin, Transport: forge.HTTPTransport{Client: &http.Client{Jar: jar}}})
		files, base, err := c.Remote("github").Load(context.Background(), "a/b", "", "")
		if err == nil {
			if !remote.IsCommitID(base.Commit) || base.Repo != "a/b" {
				t.Fatalf("accepted base %+v", base)
			}
			for p := range files {
				if remote.CheckPath(p) != nil {
					t.Fatalf("accepted path %q", p)
				}
			}
		}
	})
}

// failing is a Transport that fails or answers as told, to reach the paths a
// real server rarely takes.
type failing struct {
	forge.Transport
	err    error
	status int
	body   string
}

func (t failing) RoundTrip(ctx context.Context, r *forge.Request) (*forge.Response, error) {
	if r.Method == "GET" {
		return t.Transport.RoundTrip(ctx, r)
	}
	if t.err != nil {
		return nil, t.err
	}
	return &forge.Response{Status: t.status, Header: map[string]string{}, Body: []byte(t.body)}, nil
}

func TestOddAnswers(t *testing.T) {
	srv := httptest.NewServer(okFake())
	defer srv.Close()
	ctx := context.Background()
	for name, tr := range map[string]failing{
		"body too large":                          {err: forge.ErrBodyTooLarge},
		"transport, with the token in its text":   {err: errors.New("dial " + testCSRF)},
		"a 200 that is not JSON":                  {status: 200, body: "<html>"},
		"a 200 base for refresh that is not JSON": {status: 200, body: "{"},
	} {
		jar, _ := cookiejar.New(nil)
		tr.Transport = forge.HTTPTransport{Client: &http.Client{Jar: jar}}
		c, _ := New(Config{Base: srv.URL + "/api/git", Origin: testOrigin, Transport: tr})
		for _, op := range []func() error{
			func() error { _, _, err := c.Remote("github").Load(ctx, "a/b", "", ""); return err },
			func() error { _, _, err := c.Remote("github").Refresh(ctx, remote.Base{Repo: "a/b"}); return err },
			func() error {
				_, err := c.Remote("github").Propose(ctx, remote.Base{Repo: "a/b", Commit: testCommit}, remote.Files{"a": nil}, remote.Proposal{})
				return err
			},
		} {
			err := op()
			if err == nil || strings.Contains(err.Error(), testCSRF) {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	// no session endpoint behind a call
	dead := httptest.NewServer(http.NotFoundHandler())
	defer dead.Close()
	c, _ := New(Config{Base: dead.URL + "/api/git"})
	if _, _, err := c.Remote("github").Load(ctx, "a/b", "", ""); !errors.Is(err, remote.ErrBadResponse) {
		t.Errorf("%v", err)
	}
	// a request over the bound is refused before it is sent
	c, _ = New(Config{Base: srv.URL + "/api/git", Limits: remote.Limits{MaxTotalBytes: 10}})
	_, err := c.Remote("github").Propose(ctx, remote.Base{Repo: "a/b", Commit: testCommit}, remote.Files{"a": make([]byte, 10)}, remote.Proposal{Title: strings.Repeat("x", 2<<20)})
	if !errors.Is(err, remote.ErrTooLarge) {
		t.Errorf("%v", err)
	}
}
