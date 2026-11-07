// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-browserhttp/browsergit/secret"
)

// fakeAS is a GitLab-like authorization server: it remembers the challenge of
// each code it issues, checks the verifier, rotates refresh tokens and records
// revocations.
type fakeAS struct {
	mu        sync.Mutex
	srv       *httptest.Server
	clientID  string
	redirect  string
	codes     map[string]string // code -> challenge
	refreshes map[string]bool   // live refresh tokens
	revoked   []string
	n         int
	echo      bool // error descriptions echo the request
	failRev   bool
	bodies    []string
}

func newAS(t *testing.T) *fakeAS {
	a := &fakeAS{clientID: "app-1", redirect: "https://pg.example/playground/", codes: map[string]string{}, refreshes: map[string]bool{}}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAS) cfg() Config {
	c := GitLab(a.srv.URL, a.clientID, a.redirect)
	return c
}

// issue is what the authorize endpoint does after the person agrees.
func (a *fakeAS) issue(authURL string) (code, state string) {
	u, _ := url.Parse(authURL)
	q := u.Query()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	code = fmt.Sprintf("CODE-%d", a.n)
	a.codes[code] = q.Get("code_challenge")
	return code, q.Get("state")
}

func (a *fakeAS) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *fakeAS) fail(w http.ResponseWriter, r *http.Request, e string) {
	d := e
	if a.echo {
		d += " (request was " + r.Form.Encode() + ")"
	}
	a.reply(w, 400, map[string]string{"error": e, "error_description": d})
}

func (a *fakeAS) tokens(w http.ResponseWriter) {
	a.n++
	rt := fmt.Sprintf("REFRESH-%d", a.n)
	a.refreshes[rt] = true
	a.reply(w, 200, map[string]any{"access_token": fmt.Sprintf("ACCESS-%d", a.n), "token_type": "Bearer", "expires_in": 7200, "refresh_token": rt})
}

func (a *fakeAS) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = r.ParseForm()
	a.bodies = append(a.bodies, r.URL.RawQuery+" "+r.Form.Encode())
	if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		a.reply(w, 405, map[string]string{"error": "method"})
		return
	}
	if r.Form.Get("client_id") != a.clientID {
		a.fail(w, r, "invalid_client")
		return
	}
	switch r.URL.Path {
	case "/oauth/token":
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			ch, ok := a.codes[r.Form.Get("code")]
			delete(a.codes, r.Form.Get("code")) // single use
			if !ok || r.Form.Get("redirect_uri") != a.redirect || Challenge(r.Form.Get("code_verifier")) != ch {
				a.fail(w, r, "invalid_grant")
				return
			}
			a.tokens(w)
		case "refresh_token":
			if !a.refreshes[r.Form.Get("refresh_token")] {
				a.fail(w, r, "invalid_grant")
				return
			}
			delete(a.refreshes, r.Form.Get("refresh_token")) // rotation
			a.tokens(w)
		default:
			a.fail(w, r, "unsupported_grant_type")
		}
	case "/oauth/revoke":
		a.revoked = append(a.revoked, r.Form.Get("token"))
		if a.failRev {
			a.reply(w, 500, map[string]string{"error": "boom"})
			return
		}
		a.reply(w, 200, map[string]string{})
	default:
		a.reply(w, 404, map[string]string{"error": "not found"})
	}
}

func callback(redirect, code, state string) string {
	return redirect + "?" + url.Values{"code": {code}, "state": {state}}.Encode()
}

func TestBeginMakesAFreshAttemptAndSendsOnlyTheChallenge(t *testing.T) {
	a := newAS(t)
	c := a.cfg()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		at, link, err := c.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if len(at.State) != 43 || len(at.Verifier) != 43 || seen[at.State] || seen[at.Verifier] {
			t.Fatalf("attempt %d: %+v (lengths, or a repeat)", i, at)
		}
		seen[at.State], seen[at.Verifier] = true, true
		u, _ := url.Parse(link)
		q := u.Query()
		if strings.Contains(link, at.Verifier) || q.Get("code_challenge") != Challenge(at.Verifier) || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorize URL must carry S256(verifier), never the verifier: %s", link)
		}
		if q.Get("redirect_uri") != a.redirect || q.Get("state") != at.State || q.Get("response_type") != "code" || q.Get("scope") != "api" {
			t.Fatalf("authorize URL: %s", link)
		}
	}
	// RFC 7636 Appendix B's example.
	if Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("S256 does not match RFC 7636 Appendix B")
	}
}

func TestSignInRefreshRevoke(t *testing.T) {
	a := newAS(t)
	c := a.cfg()
	at, link, _ := c.Begin()
	code, state := a.issue(link)
	tok, err := c.Finish(context.Background(), at, callback(a.redirect, code, state))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Access.Reveal(), "ACCESS-") || tok.Refresh.IsZero() || time.Until(tok.Expiry) < time.Hour {
		t.Fatalf("tokens: expiry %v", tok.Expiry)
	}
	// The code is single-use at the server; the same callback again fails.
	if _, err := c.Finish(context.Background(), at, callback(a.redirect, code, state)); !errors.Is(err, ErrExchange) {
		t.Fatalf("replayed code: %v", err)
	}
	// Refresh rotates: the old refresh token is dead afterwards.
	oldRefresh := tok.Refresh.Reveal()
	oldAccess := tok.Access
	if err := c.Refresh(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if tok.Refresh.Reveal() == oldRefresh || !oldAccess.IsZero() {
		t.Fatal("refresh must rotate and forget the old pair")
	}
	stale := &Tokens{Refresh: secret.New(oldRefresh)}
	if err := c.Refresh(context.Background(), stale); !errors.Is(err, ErrExchange) || !stale.Refresh.IsZero() {
		t.Fatalf("a rotated-out refresh token: %v (and it must be forgotten)", err)
	}
	// Revoke sends both, and forgets both.
	acc, ref := tok.Access.Reveal(), tok.Refresh.Reveal()
	if err := c.Revoke(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if len(a.revoked) != 2 || a.revoked[0] != ref || a.revoked[1] != acc || !tok.Access.IsZero() || !tok.Refresh.IsZero() {
		t.Fatalf("revoked %v", a.revoked)
	}
	// No token ever travelled in a URL.
	for _, b := range a.bodies {
		if strings.Contains(strings.SplitN(b, " ", 2)[0], "ACCESS-") || strings.Contains(strings.SplitN(b, " ", 2)[0], "REFRESH-") {
			t.Fatalf("a token in a query string: %s", b)
		}
	}
}

func TestRevokeForgetsEvenWhenTheServerFails(t *testing.T) {
	a := newAS(t)
	a.failRev = true
	c := a.cfg()
	tok := &Tokens{Access: secret.New("ACCESS-X"), Refresh: secret.New("REFRESH-X")}
	err := c.Revoke(context.Background(), tok)
	if err == nil || strings.Contains(err.Error(), "ACCESS-X") || !tok.Access.IsZero() || !tok.Refresh.IsZero() {
		t.Fatalf("err %v; tokens must be forgotten", err)
	}
	if c.Revoke(context.Background(), nil) != nil {
		t.Fatal("nil tokens")
	}
}

func TestFinishRefusesBeforeAnyRequest(t *testing.T) {
	a := newAS(t)
	c := a.cfg()
	now := time.Now()
	c.Now = func() time.Time { return now }
	at, link, _ := c.Begin()
	code, state := a.issue(link)
	cases := map[string]struct {
		at   Attempt
		url  string
		want error
	}{
		"another state (CSRF)":      {at, callback(a.redirect, code, "forged"), ErrCallback},
		"no attempt in this tab":    {Attempt{}, callback(a.redirect, code, state), ErrCallback},
		"another path":              {at, callback("https://pg.example/other/", code, state), ErrCallback},
		"another host":              {at, callback("https://evil.example/playground/", code, state), ErrCallback},
		"http instead of https":     {at, callback("http://pg.example/playground/", code, state), ErrCallback},
		"user info in the callback": {at, callback("https://u@pg.example/playground/", code, state), ErrCallback},
		"denied":                    {at, a.redirect + "?" + url.Values{"error": {"access_denied"}, "state": {state}}.Encode(), ErrDenied},
		"no code":                   {at, a.redirect + "?state=" + url.QueryEscape(state), ErrCallback},
		"expired":                   {Attempt{State: at.State, Verifier: at.Verifier, Created: now.Add(-AttemptTTL - time.Second)}, callback(a.redirect, code, state), ErrExpired},
		"not a URL":                 {at, "::", ErrCallback},
	}
	before := len(a.bodies)
	for name, k := range cases {
		if _, err := c.Finish(context.Background(), k.at, k.url); !errors.Is(err, k.want) {
			t.Errorf("%s: %v, want %v", name, err, k.want)
		}
	}
	if len(a.bodies) != before {
		t.Fatalf("%d requests made for callbacks that must be refused locally", len(a.bodies)-before)
	}
	// A verifier that does not match the challenge is refused by the server.
	bad := at
	bad.Verifier = strings.Repeat("x", 43)
	if _, err := c.Finish(context.Background(), bad, callback(a.redirect, code, state)); !errors.Is(err, ErrExchange) {
		t.Fatalf("wrong verifier: %v", err)
	}
	if !c.IsCallback(callback(a.redirect, "c", "s")) || c.IsCallback(a.redirect) || c.IsCallback(callback("https://evil.example/playground/", "c", "s")) || c.IsCallback("::") {
		t.Fatal("IsCallback")
	}
}

func TestErrorsDoNotEchoWhatWasSent(t *testing.T) {
	a := newAS(t)
	a.echo = true
	c := a.cfg()
	at, link, _ := c.Begin()
	code, state := a.issue(link)
	at.Verifier = strings.Repeat("v", 43) // the server refuses, echoing the request
	_, err := c.Finish(context.Background(), at, callback(a.redirect, code, state))
	if err == nil || strings.Contains(err.Error(), code) || strings.Contains(err.Error(), at.Verifier) {
		t.Fatalf("the error echoes the code or the verifier: %v", err)
	}
	if !strings.Contains(err.Error(), secret.Redacted) {
		t.Fatalf("control: the echo should be there, redacted: %v", err)
	}
	tok := &Tokens{Refresh: secret.New("REFRESH-ECHO-123456")}
	err = c.Refresh(context.Background(), tok)
	if err == nil || strings.Contains(err.Error(), "REFRESH-ECHO-123456") {
		t.Fatalf("refresh error echoes the token: %v", err)
	}
}

func TestConfigAndHelpers(t *testing.T) {
	for _, c := range []Config{
		{},
		GitLab("https://gitlab.com", "", "https://x/"),
		GitLab("http://gitlab.com", "id", "https://x/"),
		GitLab("https://gitlab.com", "id", "http://pg.example/"),
		GitLab("https://gitlab.com", "id", "https://u@pg.example/"),
		{AuthorizeURL: "https://a/", TokenURL: "https://t/", RevokeURL: "http://r.example/", ClientID: "i", RedirectURI: "https://x/"},
	} {
		if _, _, err := c.Begin(); !errors.Is(err, ErrConfig) {
			t.Errorf("Begin(%+v) = %v", c, err)
		}
	}
	c := GitLab("https://gitlab.com/", "id", "https://pg.example/playground/")
	if c.TokenURL != "https://gitlab.com/oauth/token" {
		t.Fatal(c.TokenURL)
	}
	c.Rand = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if _, _, err := c.Begin(); err == nil {
		t.Fatal("a failing random source must fail Begin")
	}
	if !c.Expired(nil) || !c.Expired(&Tokens{}) {
		t.Fatal("Expired")
	}
	now := time.Now()
	c.Now = func() time.Time { return now }
	if c.Expired(&Tokens{Access: secret.New("a"), Expiry: now.Add(time.Hour)}) || !c.Expired(&Tokens{Access: secret.New("a"), Expiry: now.Add(30 * time.Second)}) {
		t.Fatal("Expired near the end")
	}
	if !errors.Is(c.Refresh(context.Background(), &Tokens{}), ErrNoRefresh) {
		t.Fatal("refresh without a token")
	}
	if clip("a\nb") != "a b" || len(clip(strings.Repeat("x", 500))) > 210 {
		t.Fatal("clip")
	}
	var nilTokens *Tokens
	nilTokens.Forget()
}

func TestParseToken(t *testing.T) {
	for _, bad := range []string{`nope`, `{}`, `{"access_token":"a","token_type":"mac"}`, `{"access_token":"a","token_type":"bearer","expires_in":-1}`} {
		if _, err := parseToken([]byte(bad)); err == nil {
			t.Errorf("parseToken(%s) accepted", bad)
		}
	}
	if _, err := parseToken([]byte(`{"error":"invalid_grant"}`)); err != nil {
		t.Fatal("an error response parses")
	}
}

func FuzzParseToken(f *testing.F) {
	f.Add([]byte(`{"access_token":"a","token_type":"Bearer","expires_in":7200,"refresh_token":"r"}`))
	f.Add([]byte(`{"error":"x","error_description":"y"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		tr, err := parseToken(b)
		if err == nil && tr.Error == "" && (tr.AccessToken == "" || tr.ExpiresIn < 0) {
			t.Fatalf("accepted %q", b)
		}
	})
}
