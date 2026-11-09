// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package gitd implements [remote.Remote] over slides-gitd
// (github.com/go-slides/gitd), the self-hosted backend-for-frontend a
// playground is served behind. The page holds no forge credential at all: the
// server does, and this package only speaks to the page's OWN origin.
//
// What it sends and keeps, and why:
//
//   - every request goes to Config.Base on the page's own origin, with the
//     Fetch API in mode "same-origin" (the browser itself refuses another
//     origin), so no forge host is ever contacted;
//   - the session is the server's HttpOnly cookie, which no script can read
//     and this package never sees; what it keeps is the per-session CSRF token
//     the session endpoint returns, in memory only, as a [secret.Token] (never
//     in a field a JSON encoder, a log line or an error message could reach);
//   - every write carries the headers slides-gitd demands (a JSON body,
//     X-Requested-With: slides-gitd, X-CSRF-Token); the browser adds the exact
//     Origin by itself;
//   - the server names the proposal's branch (slides/<readable>-<hash>/…): the
//     Proposal's Branch is ignored, and a branch the server returns is checked;
//   - a 401 is [ErrSignIn]: the IdP session ended and the person must sign in
//     again. Nothing here ever falls back to another way of authenticating;
//   - an error message is built from the status and the server's error CLASS
//     (a short lower-case word) only, never from a body.
package gitd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/go-browserhttp/browsergit/forge"
	"github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

// DefaultBase is where slides-gitd serves its API.
const DefaultBase = "/api/git"

// Errors this package adds to the shared remote classes.
var (
	// ErrNoServer: nothing at Base answers like slides-gitd. A playground that
	// gets it behaves as it does without a server.
	ErrNoServer = errors.New("gitd: no slides-gitd on this origin")
	// ErrSignIn wraps [remote.ErrAuth]: the server wants an identity it does not
	// have (HTTP 401): the sign-in expired or never happened.
	ErrSignIn = fmt.Errorf("%w: sign in again (the session ended)", remote.ErrAuth)
	// ErrNoCredential: the forge is one where each user stores their own token
	// on the server (HTTP 428) and this user has not.
	ErrNoCredential = errors.New("gitd: this forge needs your own token, stored on the server")
	// ErrRefused: the server refused the request itself (a path, a size, an
	// empty proposal).
	ErrRefused = errors.New("gitd: the server refused the request")
)

// Config configures a Client.
type Config struct {
	// Base is the API's URL without a trailing slash: a path on the page's own
	// origin ("/api/git", the default), which the browser resolves against the
	// page, or an absolute URL (https, or http to a loopback host; tests).
	Base string
	// Origin, when set, is sent as the Origin header. A browser sets it itself
	// and forbids a script to; a native client (a test) has none to send.
	Origin string
	// Transport sends the requests; nil means the default (the Fetch API in the
	// browser, with the page's cookies; natively net/http WITHOUT a cookie jar:
	// pass an HTTPTransport whose Client has one).
	Transport forge.Transport
	// Limits bound what a Load brings in and what a Propose sends.
	Limits remote.Limits
}

// Repo is one repository the signed-in user may use.
type Repo struct {
	// Forge is the server's name for the forge ("github", "gitlab").
	Forge string
	// Name is "owner/name".
	Name string
}

// String is "forge:owner/name", the server's own spelling.
func (r Repo) String() string { return r.Forge + ":" + r.Name }

// Info is what the session endpoint says about the signed-in user.
type Info struct {
	// User is the identity the proxy vouched for (the OIDC subject): what the
	// pane shows as "signed in as".
	User string
	// Repos are the repositories the user is granted, in the server's order.
	Repos []Repo
}

// Client talks to one slides-gitd.
type Client struct {
	base   string
	origin string
	tr     forge.Transport
	lim    remote.Limits
	maxRsp int64

	mu   sync.Mutex
	csrf secret.Token
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(cfg.Base, "/")
	if base == "" {
		base = DefaultBase
	}
	if err := checkBase(base); err != nil {
		return nil, err
	}
	tr := cfg.Transport
	if tr == nil {
		tr = forge.DefaultTransport()
	}
	lim := cfg.Limits.OrDefault()
	// a file is base64 in JSON: 4/3 of its bytes, and some slack for the rest
	maxRsp := lim.MaxTotalBytes/3*4 + 1<<20
	return &Client{base: base, origin: cfg.Origin, tr: tr, lim: lim, maxRsp: maxRsp}, nil
}

func checkBase(base string) error {
	if strings.HasPrefix(base, "/") && !strings.HasPrefix(base, "//") && !strings.ContainsAny(base, "?#\\") {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: base %q", remote.ErrNotConfigured, base)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && remote.IsLoopback(u.Hostname())) {
		return fmt.Errorf("%w: base %q is not https", remote.ErrNotConfigured, base)
	}
	return nil
}

// Forget drops the CSRF token held in memory (a sign-out of this page).
func (c *Client) Forget() {
	c.mu.Lock()
	c.csrf.Forget()
	c.csrf = secret.Token{}
	c.mu.Unlock()
}

// sessionRsp is the session endpoint's answer.
type sessionRsp struct {
	User  string   `json:"user"`
	CSRF  string   `json:"csrf"`
	Repos []string `json:"repos"`
}

// parseSession checks the session endpoint's body. It refuses anything that is
// not exactly what slides-gitd sends, so a page that answers every path with
// HTML (a single-page host) is not mistaken for a server.
func parseSession(body []byte) (Info, secret.Token, error) {
	var s sessionRsp
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return Info{}, secret.Token{}, ErrNoServer
	}
	if s.User == "" || len(s.User) > 256 || strings.IndexFunc(s.User, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return Info{}, secret.Token{}, ErrNoServer
	}
	if len(s.CSRF) < 16 || len(s.CSRF) > 256 || strings.ContainsAny(s.CSRF, " \r\n\t") {
		return Info{}, secret.Token{}, ErrNoServer
	}
	if len(s.Repos) > 1000 {
		return Info{}, secret.Token{}, ErrNoServer
	}
	info := Info{User: s.User}
	for _, r := range s.Repos {
		f, name, ok := strings.Cut(r, ":")
		if !ok || f == "" || len(f) > 32 || remote.CheckRepo(name, true) != nil {
			return Info{}, secret.Token{}, ErrNoServer
		}
		info.Repos = append(info.Repos, Repo{Forge: f, Name: name})
	}
	return info, secret.New(s.CSRF), nil
}

// errClass is the server's error class from a body: a short lower-case word,
// or "" for anything else. Nothing else of a body is ever used.
func errClass(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error) == 0 || len(e.Error) > 32 {
		return ""
	}
	for _, r := range e.Error {
		if !(r >= 'a' && r <= 'z' || r == '-') {
			return ""
		}
	}
	return e.Error
}

// Session asks the server who the user is and starts (or resumes) their
// session; the cookie the server sets is the browser's, the CSRF token is kept
// here. It is the discovery too:
//
//   - [ErrNoServer]: nothing here answers like slides-gitd (a static host's 404,
//     an HTML page): behave as without a server;
//   - [ErrSignIn]: slides-gitd (or its proxy) is here and wants the person to
//     sign in;
//   - [remote.ErrTransport]: the request itself failed.
func (c *Client) Session(ctx context.Context) (Info, error) {
	rsp, err := c.tr.RoundTrip(ctx, &forge.Request{
		Method: "GET", URL: c.base + "/session", SameOrigin: true, Limit: 1 << 20,
		Header: map[string]string{"Accept": "application/json", "X-Requested-With": "slides-gitd"},
	})
	if err != nil {
		return Info{}, fmt.Errorf("%w: session: %v", remote.ErrTransport, err)
	}
	switch {
	case rsp.Status == 401:
		c.Forget()
		return Info{}, ErrSignIn
	case rsp.Status != 200 || !strings.HasPrefix(rsp.Get("content-type"), "application/json"):
		return Info{}, ErrNoServer
	}
	info, tok, err := parseSession(rsp.Body)
	if err != nil {
		return Info{}, err
	}
	c.mu.Lock()
	c.csrf.Forget()
	c.csrf = tok
	c.mu.Unlock()
	return info, nil
}

// call POSTs body to path and decodes the answer into out. A session the
// server lost (a restart, an expiry) is re-established once and the call tried
// again: it was refused before it did anything.
func (c *Client) call(ctx context.Context, op, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encode %s: %v", remote.ErrBadResponse, op, err)
	}
	if int64(len(b)) > c.maxRsp {
		return fmt.Errorf("%w: %s: request over %d bytes", remote.ErrTooLarge, op, c.maxRsp)
	}
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		have := !c.csrf.IsZero()
		c.mu.Unlock()
		if !have {
			if _, err := c.Session(ctx); err != nil {
				if errors.Is(err, ErrNoServer) {
					return fmt.Errorf("%w: %s", remote.ErrBadResponse, op)
				}
				return err
			}
		}
		c.mu.Lock()
		h := map[string]string{
			"Accept": "application/json", "Content-Type": "application/json",
			"X-Requested-With": "slides-gitd", "X-CSRF-Token": c.csrf.Reveal(),
		}
		c.mu.Unlock()
		if c.origin != "" {
			h["Origin"] = c.origin
		}
		rsp, err := c.tr.RoundTrip(ctx, &forge.Request{Method: "POST", URL: c.base + path, Header: h, Body: b, Limit: c.maxRsp, SameOrigin: true})
		if errors.Is(err, forge.ErrBodyTooLarge) {
			return fmt.Errorf("%w: %s: response over %d bytes", remote.ErrTooLarge, op, c.maxRsp)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %s: %v", remote.ErrTransport, op, c.redact(err.Error()))
		}
		if rsp.Status >= 200 && rsp.Status <= 299 {
			if out == nil {
				return nil
			}
			dec := json.NewDecoder(bytes.NewReader(rsp.Body))
			if err := dec.Decode(out); err != nil {
				return fmt.Errorf("%w: %s", remote.ErrBadResponse, op)
			}
			return nil
		}
		class := errClass(rsp.Body)
		if rsp.Status == 403 && (class == "session" || class == "no-session" || class == "csrf") && attempt == 0 {
			c.Forget()
			continue
		}
		return c.statusError(op, rsp.Status, class)
	}
}

func (c *Client) redact(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return secret.Redact(s, c.csrf)
}

// statusError maps the server's status and error class to a remote class.
func (c *Client) statusError(op string, status int, class string) error {
	var base error
	switch {
	case status == 401:
		c.Forget()
		return ErrSignIn
	case status == 428 || class == "no-credential":
		return ErrNoCredential
	case status == 429 || class == "rate-limited" || class == "forge-rate-limited":
		base = remote.ErrRateLimited
	case class == "denied":
		base = remote.ErrForbidden
	case class == "forge-auth":
		base = remote.ErrAuth
	case class == "not-found" || status == 404:
		base = remote.ErrNotFound
	case class == "conflict" || status == 409:
		base = remote.ErrConflict
	case class == "refused" || class == "bad-request" || status == 400 || status == 413:
		base = ErrRefused
	case status == 403:
		base = remote.ErrForbidden
	default:
		base = remote.ErrTransport
	}
	if class == "" {
		return fmt.Errorf("%w (%s: HTTP %d)", base, op, status)
	}
	return fmt.Errorf("%w (%s: HTTP %d, %s)", base, op, status, class)
}

// StoreCredential gives the server the user's own token for a forge that works
// that way (auth "user-token"). The server seals it and never sends it back.
func (c *Client) StoreCredential(ctx context.Context, forgeName string, tok secret.Token) error {
	body := struct {
		Forge string `json:"forge"`
		Token string `json:"token"`
	}{forgeName, tok.Reveal()}
	return c.call(ctx, "store credential", "/credential", body, nil)
}

// ForgetCredential asks the server to forget the user's token for forgeName.
func (c *Client) ForgetCredential(ctx context.Context, forgeName string) error {
	return c.call(ctx, "forget credential", "/credential/delete", map[string]string{"forge": forgeName}, nil)
}

// Remote returns the [remote.Remote] for the forge the server names
// forgeName. Its Propose ignores Proposal.Branch: the server names the branch.
func (c *Client) Remote(forgeName string) remote.Remote { return &rem{c: c, forge: forgeName} }

type rem struct {
	c     *Client
	forge string
}

type loadRsp struct {
	Files remote.Files `json:"files"`
	Base  remote.Base  `json:"base"`
}

// checkBaseRsp checks a base the server returned against what was asked.
func checkBaseRsp(b remote.Base, repo, dir string) error {
	if b.Repo != repo || b.Dir != dir || !remote.IsCommitID(b.Commit) || remote.CheckRepo(b.Repo, true) != nil || remote.CheckDir(b.Dir) != nil {
		return fmt.Errorf("%w: the server returned another base", remote.ErrBadResponse)
	}
	for p := range b.Blobs {
		if remote.CheckPath(p) != nil {
			return fmt.Errorf("%w: the server returned a path that is refused", remote.ErrBadResponse)
		}
	}
	return nil
}

// checkRsp checks files the server returned: their number, sizes and every path.
func (r *rem) checkRsp(files remote.Files) error { return r.c.lim.CheckFiles(files) }

// Load implements [remote.Remote].
func (r *rem) Load(ctx context.Context, repo, ref, dir string) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(repo, true); err != nil {
		return nil, remote.Base{}, err
	}
	if err := remote.CheckDir(dir); err != nil {
		return nil, remote.Base{}, err
	}
	var out loadRsp
	req := map[string]string{"forge": r.forge, "repo": repo, "ref": ref, "dir": dir}
	if err := r.c.call(ctx, "load", "/load", req, &out); err != nil {
		return nil, remote.Base{}, err
	}
	if err := checkBaseRsp(out.Base, repo, dir); err != nil {
		return nil, remote.Base{}, err
	}
	if err := r.checkRsp(out.Files); err != nil {
		return nil, remote.Base{}, err
	}
	return out.Files, out.Base, nil
}

// Propose implements [remote.Remote].
func (r *rem) Propose(ctx context.Context, base remote.Base, files remote.Files, p remote.Proposal) (remote.Result, error) {
	if err := remote.CheckRepo(base.Repo, true); err != nil {
		return remote.Result{}, err
	}
	if !remote.IsCommitID(base.Commit) {
		return remote.Result{}, fmt.Errorf("%w: the base has no commit", remote.ErrBadBranch)
	}
	if err := r.c.lim.CheckFiles(files); err != nil {
		return remote.Result{}, err
	}
	req := struct {
		Forge string       `json:"forge"`
		Base  remote.Base  `json:"base"`
		Files remote.Files `json:"files"`
		Title string       `json:"title"`
		Body  string       `json:"body"`
	}{r.forge, base, files, p.Title, p.Body}
	var out struct {
		Branch string `json:"branch"`
		Commit string `json:"commit"`
		URL    string `json:"url"`
	}
	if err := r.c.call(ctx, "propose", "/propose", req, &out); err != nil {
		return remote.Result{}, err
	}
	if remote.CheckBranch(out.Branch, base.Ref) != nil || !strings.HasPrefix(out.Branch, "slides/") {
		return remote.Result{}, fmt.Errorf("%w: the server returned a branch that is refused", remote.ErrBadResponse)
	}
	res := remote.Result{Branch: out.Branch, Commit: out.Commit}
	if out.URL != "" {
		u, err := url.Parse(out.URL)
		if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return remote.Result{}, fmt.Errorf("%w: the server returned a link that is not http(s)", remote.ErrBadResponse)
		}
		res.URL = u.String()
	}
	return res, nil
}

// Refresh implements [remote.Remote].
func (r *rem) Refresh(ctx context.Context, base remote.Base) (remote.Files, remote.Base, error) {
	if err := remote.CheckRepo(base.Repo, true); err != nil {
		return nil, base, err
	}
	var out loadRsp
	req := struct {
		Forge string      `json:"forge"`
		Base  remote.Base `json:"base"`
	}{r.forge, base}
	if err := r.c.call(ctx, "refresh", "/refresh", req, &out); err != nil {
		return nil, base, err
	}
	if out.Files == nil {
		return nil, base, nil
	}
	if err := checkBaseRsp(out.Base, base.Repo, base.Dir); err != nil {
		return nil, base, err
	}
	if err := r.checkRsp(out.Files); err != nil {
		return nil, base, err
	}
	return out.Files, out.Base, nil
}
