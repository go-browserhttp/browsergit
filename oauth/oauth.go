// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package oauth is the OAuth 2.0 authorization code flow with PKCE for a
// public client (a page with no secret): GitLab, Gitea, Forgejo.
//
// What it holds to, and where from:
//
//   - RFC 7636: a fresh code_verifier per attempt (32 random octets,
//     base64url, 43 characters), sent only to the token endpoint; the
//     authorize URL carries only S256(verifier).
//   - RFC 9700 §2.1: CSRF is refused with a one-time state per attempt, bound
//     to the attempt and compared in constant time, on top of PKCE; §4.1.3:
//     the redirect URI is one exact string, sent the same in both requests, and
//     a callback on any other URL is refused; §2.2.2: refresh tokens of a public
//     client must rotate (GitLab invalidates the old pair on refresh) — the old
//     pair is forgotten here too; §4.3.2: tokens never go in a URL.
//   - An attempt is single-use and expires (10 minutes): a replayed callback,
//     or one for an attempt that was never begun here, exchanges nothing.
//   - Tokens are secret.Token: never printed, never serialized. Revoke drops
//     them from memory whether or not the server answered.
//
// Requests go through forge.Transport: the Fetch API in the browser
// (redirect "error", credentials "omit"), net/http natively.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-browserhttp/browsergit/forge"
	"github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

// Errors.
var (
	ErrConfig    = errors.New("oauth: not configured")
	ErrCallback  = errors.New("oauth: callback refused")
	ErrDenied    = errors.New("oauth: authorization denied")
	ErrExpired   = errors.New("oauth: the sign-in attempt expired")
	ErrExchange  = errors.New("oauth: token exchange failed")
	ErrNoRefresh = errors.New("oauth: no refresh token")
)

// AttemptTTL bounds how long a begun sign-in may wait for its callback.
const AttemptTTL = 10 * time.Minute

// Config is one OAuth application on one forge.
type Config struct {
	AuthorizeURL string // https://gitlab.com/oauth/authorize
	TokenURL     string // https://gitlab.com/oauth/token
	RevokeURL    string // https://gitlab.com/oauth/revoke ("" if none)
	ClientID     string
	// RedirectURI is matched EXACTLY (scheme, host, port, path) on callback.
	RedirectURI string
	Scopes      []string
	Transport   forge.Transport // nil: forge.DefaultTransport()
	// Rand and Now are for tests; nil means crypto/rand and time.Now.
	Rand func([]byte) (int, error)
	Now  func() time.Time
}

// GitLab is the configuration for a GitLab instance at base
// ("https://gitlab.com"). The api scope is what the REST commit and merge
// request calls need (docs.gitlab.com/integration/oauth_provider).
func GitLab(base, clientID, redirectURI string) Config {
	base = strings.TrimRight(base, "/")
	return Config{
		AuthorizeURL: base + "/oauth/authorize",
		TokenURL:     base + "/oauth/token",
		RevokeURL:    base + "/oauth/revoke",
		ClientID:     clientID,
		RedirectURI:  redirectURI,
		Scopes:       []string{"api"},
	}
}

func (c Config) check() error {
	if c.ClientID == "" || c.RedirectURI == "" {
		return fmt.Errorf("%w: client id and redirect URI are required", ErrConfig)
	}
	for _, u := range []string{c.AuthorizeURL, c.TokenURL, c.RedirectURI} {
		if err := httpsURL(u); err != nil {
			return err
		}
	}
	if c.RevokeURL != "" {
		return httpsURL(c.RevokeURL)
	}
	return nil
}

func httpsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%w: URL %q", ErrConfig, raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && remote.IsLoopback(u.Hostname())) {
		return fmt.Errorf("%w: URL %q is not https", ErrConfig, raw)
	}
	return nil
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) random(n int) (string, error) {
	b := make([]byte, n)
	read := rand.Read
	if c.Rand != nil {
		read = c.Rand
	}
	if _, err := read(b); err != nil {
		return "", fmt.Errorf("oauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (c Config) transport() forge.Transport {
	if c.Transport != nil {
		return c.Transport
	}
	return forge.DefaultTransport()
}

// Attempt is one sign-in between Begin and Finish. The page keeps it for the
// round trip through the forge (sessionStorage: this tab only) and must drop
// it after Finish, whatever the outcome.
type Attempt struct {
	State    string    `json:"state"`
	Verifier string    `json:"verifier"`
	Created  time.Time `json:"created"`
}

// Challenge is S256(verifier) per RFC 7636 §4.2.
func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Begin makes a fresh attempt and the URL to send the person to.
func (c Config) Begin() (Attempt, string, error) {
	if err := c.check(); err != nil {
		return Attempt{}, "", err
	}
	state, err := c.random(32)
	if err != nil {
		return Attempt{}, "", err
	}
	verifier, err := c.random(32)
	if err != nil {
		return Attempt{}, "", err
	}
	q := url.Values{
		"client_id":             {c.ClientID},
		"redirect_uri":          {c.RedirectURI},
		"response_type":         {"code"},
		"state":                 {state},
		"scope":                 {strings.Join(c.Scopes, " ")},
		"code_challenge":        {Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return Attempt{State: state, Verifier: verifier, Created: c.now()}, c.AuthorizeURL + "?" + q.Encode(), nil
}

// IsCallback reports whether pageURL is a callback to this configuration: the
// exact redirect URI carrying a state, with a code or an error.
func (c Config) IsCallback(pageURL string) bool {
	u, err := url.Parse(pageURL)
	if err != nil || !c.sameRedirect(u) {
		return false
	}
	q := u.Query()
	return q.Get("state") != "" && (q.Get("code") != "" || q.Get("error") != "")
}

func (c Config) sameRedirect(u *url.URL) bool {
	r, err := url.Parse(c.RedirectURI)
	if err != nil {
		return false
	}
	return u.Scheme == r.Scheme && u.Host == r.Host && u.EscapedPath() == r.EscapedPath() && u.User == nil
}

// Tokens are a signed-in session's credentials.
type Tokens struct {
	Access  secret.Token
	Refresh secret.Token
	Expiry  time.Time
}

// Forget drops both tokens from memory.
func (t *Tokens) Forget() {
	if t == nil {
		return
	}
	t.Access.Forget()
	t.Refresh.Forget()
	t.Access, t.Refresh = secret.Token{}, secret.Token{}
}

// Finish checks the callback against the attempt and exchanges the code. The
// attempt is spent either way: the caller drops it.
func (c Config) Finish(ctx context.Context, a Attempt, pageURL string) (*Tokens, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	u, err := url.Parse(pageURL)
	if err != nil || !c.sameRedirect(u) {
		return nil, fmt.Errorf("%w: not the registered redirect URI", ErrCallback)
	}
	q := u.Query()
	got := q.Get("state")
	if a.State == "" || a.Verifier == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.State)) != 1 {
		return nil, fmt.Errorf("%w: state does not match this tab's sign-in", ErrCallback)
	}
	if c.now().Sub(a.Created) > AttemptTTL || c.now().Before(a.Created.Add(-time.Minute)) {
		return nil, ErrExpired
	}
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("%w: %s", ErrDenied, clip(e))
	}
	code := q.Get("code")
	if code == "" || len(code) > 1024 {
		return nil, fmt.Errorf("%w: no usable code", ErrCallback)
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {c.RedirectURI},
		"code_verifier": {a.Verifier},
	}
	return c.tokenRequest(ctx, form, "exchange")
}

// Refresh trades t's refresh token for a new pair, forgetting the old one
// (rotation, RFC 9700 §2.2.2). On failure t is left signed out.
func (c Config) Refresh(ctx context.Context, t *Tokens) error {
	if t == nil || t.Refresh.IsZero() {
		return ErrNoRefresh
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"refresh_token": {t.Refresh.Reveal()},
		"grant_type":    {"refresh_token"},
		"redirect_uri":  {c.RedirectURI},
	}
	nt, err := c.tokenRequest(ctx, form, "refresh")
	old := *t
	if err != nil {
		t.Forget()
		return err
	}
	old.Access.Forget()
	old.Refresh.Forget()
	*t = *nt
	return nil
}

// Expired reports whether t should be refreshed before use (a minute early).
func (c Config) Expired(t *Tokens) bool {
	return t == nil || t.Access.IsZero() || (!t.Expiry.IsZero() && c.now().Add(time.Minute).After(t.Expiry))
}

// Revoke asks the forge to revoke both tokens (RFC 7009) and forgets them
// whatever it answers: signing out never leaves a token in memory.
func (c Config) Revoke(ctx context.Context, t *Tokens) error {
	if t == nil {
		return nil
	}
	defer t.Forget()
	if c.RevokeURL == "" {
		return nil
	}
	var first error
	for _, tok := range []secret.Token{t.Refresh, t.Access} {
		if tok.IsZero() {
			continue
		}
		form := url.Values{"client_id": {c.ClientID}, "token": {tok.Reveal()}}
		rsp, err := c.post(ctx, c.RevokeURL, form)
		if err == nil && (rsp.Status < 200 || rsp.Status > 299) {
			err = fmt.Errorf("oauth: revoke: HTTP %d", rsp.Status)
		}
		if err != nil && first == nil {
			first = errors.New(secret.Redact(err.Error(), tok))
		}
	}
	return first
}

func (c Config) post(ctx context.Context, endpoint string, form url.Values) (*forge.Response, error) {
	return c.transport().RoundTrip(ctx, &forge.Request{
		Method: "POST",
		URL:    endpoint,
		Header: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Accept":       "application/json",
		},
		Body:  []byte(form.Encode()),
		Limit: 64 << 10,
	})
}

// tokenResponse is RFC 6749 §5.1.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (c Config) tokenRequest(ctx context.Context, form url.Values, op string) (*Tokens, error) {
	sent := []secret.Token{secret.New(form.Get("code")), secret.New(form.Get("refresh_token")), secret.New(form.Get("code_verifier"))}
	rsp, err := c.post(ctx, c.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %s", ErrExchange, op, secret.Redact(err.Error(), sent...))
	}
	defer func() {
		for _, s := range sent {
			s.Forget()
		}
	}()
	tr, err := parseToken(rsp.Body)
	if err != nil && rsp.Status == 200 {
		return nil, fmt.Errorf("%w (%s): %v", ErrExchange, op, err)
	}
	if rsp.Status != 200 || tr.Error != "" {
		msg := tr.Error
		if tr.Description != "" {
			msg += ": " + tr.Description
		}
		// a server can echo what it was sent: the code, the verifier, a token
		return nil, fmt.Errorf("%w (%s): HTTP %d %s", ErrExchange, op, rsp.Status, secret.Redact(clip(msg), sent...))
	}
	t := &Tokens{Access: secret.New(tr.AccessToken), Refresh: secret.New(tr.RefreshToken)}
	if tr.ExpiresIn > 0 {
		t.Expiry = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return t, nil
}

// parseToken decodes a token response; a 200 without a bearer access token is
// refused.
func parseToken(body []byte) (tokenResponse, error) {
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tr, fmt.Errorf("malformed token response")
	}
	if tr.Error == "" {
		if tr.AccessToken == "" || !strings.EqualFold(tr.TokenType, "bearer") {
			return tr, fmt.Errorf("no bearer access token in the response")
		}
		if tr.ExpiresIn < 0 || tr.ExpiresIn > 366*24*3600 {
			return tr, fmt.Errorf("expires_in %d out of range", tr.ExpiresIn)
		}
	}
	return tr, nil
}

// clip keeps a server-supplied message short and on one line.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
