// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package forge implements [remote.Remote] over the REST APIs of GitHub, GitLab
// and Gitea/Forgejo.
//
// What the token may reach is the point of this package:
//
//   - it goes in one header, on requests to the configured API origin, and
//     nowhere else: redirects are refused (natively by CheckRedirect, in the
//     browser by the Fetch "redirect: error" mode), and no URL a server returns
//     is ever requested with it (GitLab pagination uses the page number header,
//     not the Link URL);
//   - the API base must be https (plain http only to a loopback host);
//   - browser requests are sent with credentials "omit", so no cookie of the
//     forge rides along with them;
//   - every error message built from a response is passed through
//     [secret.Redact], so a server that echoes the request cannot put the token
//     on screen;
//   - response bodies are read through a size limit, paths from the server are
//     checked by [remote.CheckPath], and the pull request URL handed back to the
//     page must be http(s) on the forge's web origin.
package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/go-browserhttp/browsergit/remote"
	"github.com/go-browserhttp/browsergit/secret"
)

// Kind names a forge API family.
type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
	// Gitea covers Forgejo, whose API is Gitea's.
	Gitea Kind = "gitea"
)

// Config configures a forge remote.
type Config struct {
	Kind Kind
	// API is the REST base: "https://api.github.com",
	// "https://gitlab.com/api/v4", "https://codeberg.org/api/v1".
	API string
	// Web is the forge's web origin, against which a returned pull request URL
	// is checked ("https://github.com"). Empty: derived from API (GitHub's
	// api.github.com maps to github.com; others drop the API path).
	Web string
	// Token authenticates every request. Zero: anonymous (Load of a public
	// repository only).
	Token secret.Token
	// Transport sends the requests; nil means net/http natively (60 s timeout)
	// and the Fetch API in the browser. Either refuses redirects.
	Transport Transport
	Limits    remote.Limits
	// BranchPrefix, when set, is required of every proposal branch.
	BranchPrefix string
	// MaxResponseBytes bounds one JSON response (default 32 MiB).
	MaxResponseBytes int64
	// Concurrency bounds parallel blob downloads (default 6).
	Concurrency int
}

// New returns the Remote for cfg.Kind.
func New(cfg Config) (remote.Remote, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	switch cfg.Kind {
	case GitHub:
		return &github{c}, nil
	case GitLab:
		return &gitlab{c}, nil
	case Gitea:
		return &gitea{c}, nil
	}
	return nil, fmt.Errorf("%w: unknown forge kind %q", remote.ErrNotConfigured, cfg.Kind)
}

// client is the shared HTTP layer.
type client struct {
	kind   Kind
	api    *url.URL
	web    *url.URL
	token  secret.Token
	tr     Transport
	lim    remote.Limits
	prefix string
	maxRsp int64
	par    int
}

func newClient(cfg Config) (*client, error) {
	api, err := checkOrigin(cfg.API)
	if err != nil {
		return nil, err
	}
	webRaw := cfg.Web
	if webRaw == "" {
		webRaw = deriveWeb(api)
	}
	web, err := checkOrigin(webRaw)
	if err != nil {
		return nil, err
	}
	tr := cfg.Transport
	if tr == nil {
		tr = defaultTransport()
	}
	c := &client{
		kind: cfg.Kind, api: api, web: web, token: cfg.Token, tr: tr,
		lim: cfg.Limits.OrDefault(), prefix: cfg.BranchPrefix,
		maxRsp: cfg.MaxResponseBytes, par: cfg.Concurrency,
	}
	if c.maxRsp <= 0 {
		c.maxRsp = 32 << 20
	}
	if c.par <= 0 {
		c.par = 6
	}
	return c, nil
}

func checkOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: URL %q", remote.ErrNotConfigured, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: URL %q carries user info, a query or a fragment", remote.ErrNotConfigured, raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && remote.IsLoopback(u.Hostname())) {
		return nil, fmt.Errorf("%w: URL %q is not https", remote.ErrNotConfigured, raw)
	}
	return u, nil
}

func deriveWeb(api *url.URL) string {
	host := api.Host
	if strings.EqualFold(api.Hostname(), "api.github.com") {
		host = "github.com"
	}
	return api.Scheme + "://" + host
}

// checkWebURL accepts a URL a server returned for the person to open only when
// it is http(s) on the forge's web origin.
func (c *client) checkWebURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || !strings.EqualFold(u.Scheme, c.web.Scheme) || !strings.EqualFold(u.Host, c.web.Host) {
		return "", fmt.Errorf("%w: the forge returned a link outside %s", remote.ErrBadResponse, c.web.Host)
	}
	return u.String(), nil
}

// apiError is a non-2xx response, already redacted.
type apiError struct {
	class  error
	status int
	op     string
	detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%v (%s: HTTP %d): %s", e.class, e.op, e.status, e.detail)
}

func (e *apiError) Unwrap() error { return e.class }

// request is one API call.
type request struct {
	op     string     // for messages: "get tree"
	method string     // GET when empty
	path   string     // already escaped, relative to the API base, leading "/"
	query  url.Values // optional
	body   any        // JSON-encoded when non-nil
	raw    bool       // the response is raw bytes, not JSON
	limit  int64      // response bound; c.maxRsp when zero
}

// response is what do returns on success.
type response struct {
	status int
	header map[string]string // names in lower case
	body   []byte
}

func (r *response) get(name string) string { return r.header[strings.ToLower(name)] }

const (
	methodGet  = "GET"
	methodPost = "POST"
)

func (c *client) do(ctx context.Context, rq request) (*response, error) {
	u := *c.api
	rawPath := c.api.EscapedPath() + rq.path
	plain, err := url.PathUnescape(rawPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: path: %v", remote.ErrNotConfigured, rq.op, err)
	}
	// Path and RawPath must agree for url.URL to keep our escaping (GitLab's
	// "group%2Fproject" must reach the wire as %2F, not as a slash or %252F).
	u.Path, u.RawPath = plain, rawPath
	if rq.query != nil {
		u.RawQuery = rq.query.Encode()
	}
	req := &Request{Method: rq.method, URL: u.String(), Header: map[string]string{}, Limit: rq.limit}
	if req.Method == "" {
		req.Method = methodGet
	}
	if req.Limit <= 0 {
		req.Limit = c.maxRsp
	}
	if rq.body != nil {
		b, err := json.Marshal(rq.body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode %s: %v", remote.ErrBadResponse, rq.op, err)
		}
		req.Body = b
		req.Header["Content-Type"] = "application/json"
	}
	c.headers(req.Header)
	rsp, err := c.tr.RoundTrip(ctx, req)
	if errors.Is(err, ErrBodyTooLarge) {
		return nil, fmt.Errorf("%w: %s: response over %d bytes", remote.ErrTooLarge, rq.op, req.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s", remote.ErrTransport, rq.op, c.redact(err.Error()))
	}
	if int64(len(rsp.Body)) > req.Limit {
		// a Transport that did not bound the body is bounded here
		return nil, fmt.Errorf("%w: %s: response over %d bytes", remote.ErrTooLarge, rq.op, req.Limit)
	}
	if rsp.Status < 200 || rsp.Status > 299 {
		return nil, c.statusError(rq.op, rsp)
	}
	return &response{status: rsp.Status, header: rsp.Header, body: rsp.Body}, nil
}

func (c *client) headers(h map[string]string) {
	switch c.kind {
	case GitHub:
		h["Accept"] = "application/vnd.github+json"
		h["X-GitHub-Api-Version"] = "2022-11-28"
	default:
		h["Accept"] = "application/json"
	}
	if c.token.IsZero() {
		return
	}
	switch c.kind {
	case Gitea:
		h["Authorization"] = "token " + c.token.Reveal()
	default:
		h["Authorization"] = "Bearer " + c.token.Reveal()
	}
}

func (c *client) redact(s string) string { return secret.Redact(s, c.token) }

// statusError classifies a non-2xx response. Only the server's "message" field
// is kept, truncated and redacted; the raw body is not.
func (c *client) statusError(op string, rsp *Response) error {
	var m struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	_ = json.Unmarshal(rsp.Body, &m)
	detail := fmt.Sprint(firstNonNil(m.Message, m.Error, statusText(rsp.Status)))
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	detail = c.redact(detail)
	class := remote.ErrTransport
	switch rsp.Status {
	case 401:
		class = remote.ErrAuth
	case 403, 429:
		class = remote.ErrForbidden
		if rsp.Status == 429 || rsp.Get("X-RateLimit-Remaining") == "0" {
			class = remote.ErrRateLimited
		}
	case 404:
		class = remote.ErrNotFound
	case 400, 409, 422:
		class = remote.ErrConflict
	case 413:
		class = remote.ErrTooLarge
	}
	if rsp.Status >= 300 && rsp.Status < 400 {
		detail = "redirect refused"
	}
	return &apiError{class: class, status: rsp.Status, op: op, detail: detail}
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

func (c *client) getJSON(ctx context.Context, rq request, out any) (*response, error) {
	rsp, err := c.do(ctx, rq)
	if err != nil {
		return nil, err
	}
	if err := decode(rsp.body, out); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", remote.ErrBadResponse, rq.op, err)
	}
	return rsp, nil
}

func decode(data []byte, out any) error {
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// exists reports whether a GET answers 2xx (true) or 404 (false).
func (c *client) exists(ctx context.Context, rq request) (bool, error) {
	_, err := c.do(ctx, rq)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, remote.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// entry is one regular file of a tree listing, path relative to the repo root.
type entry struct {
	path string
	blob string
	size int64 // -1 when the listing does not say
}

// selectEntries keeps the regular files under dir, checks every path and the
// limits, and returns them with dir-relative names.
func (c *client) selectEntries(all []entry, dir string) ([]entry, error) {
	var out []entry
	var total int64
	for _, e := range all {
		rel, ok := remote.Rel(dir, e.path)
		if !ok {
			continue
		}
		if err := remote.CheckPath(rel); err != nil {
			return nil, err
		}
		if !remote.IsCommitID(e.blob) {
			return nil, fmt.Errorf("%w: blob id %q", remote.ErrBadResponse, e.blob)
		}
		if len(out) >= c.lim.MaxFiles {
			return nil, fmt.Errorf("%w: more than %d files under %q", remote.ErrTooLarge, c.lim.MaxFiles, dir)
		}
		if e.size > c.lim.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s is %d bytes", remote.ErrTooLarge, e.path, e.size)
		}
		if e.size > 0 {
			total += e.size
			if total > c.lim.MaxTotalBytes {
				return nil, fmt.Errorf("%w: more than %d bytes", remote.ErrTooLarge, c.lim.MaxTotalBytes)
			}
		}
		out = append(out, entry{path: rel, blob: e.blob, size: e.size})
	}
	return out, nil
}

// fetchAll downloads every entry's blob with bounded parallelism, checking the
// size limits on the bytes actually received, and that each blob's content
// hashes to the id the listing gave (a server cannot hand back other bytes).
func (c *client) fetchAll(ctx context.Context, es []entry, get func(context.Context, entry) ([]byte, error)) (remote.Files, map[string]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	files := make(remote.Files, len(es))
	blobs := make(map[string]string, len(es))
	var (
		mu    sync.Mutex
		total int64
		first error
		wg    sync.WaitGroup
	)
	sem := make(chan struct{}, c.par)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
			cancel()
		}
		mu.Unlock()
	}
	for _, e := range es {
		wg.Add(1)
		sem <- struct{}{}
		go func(e entry) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := get(ctx, e)
			if err != nil {
				fail(err)
				return
			}
			if int64(len(data)) > c.lim.MaxFileBytes {
				fail(fmt.Errorf("%w: %s", remote.ErrTooLarge, e.path))
				return
			}
			if len(e.blob) == 40 && remote.BlobID(data) != e.blob {
				fail(fmt.Errorf("%w: %s does not match its blob id", remote.ErrBadResponse, e.path))
				return
			}
			mu.Lock()
			defer mu.Unlock()
			total += int64(len(data))
			if total > c.lim.MaxTotalBytes && first == nil {
				first = fmt.Errorf("%w: more than %d bytes", remote.ErrTooLarge, c.lim.MaxTotalBytes)
				cancel()
			}
			files[e.path] = data
			blobs[e.path] = e.blob
		}(e)
	}
	wg.Wait()
	if first != nil {
		return nil, nil, first
	}
	return files, blobs, nil
}

// decodeBase64Blob decodes a JSON blob's content field, tolerating the line
// breaks GitHub inserts.
func decodeBase64Blob(content, encoding string, limit int64) ([]byte, error) {
	if encoding != "" && encoding != "base64" {
		return nil, fmt.Errorf("%w: blob encoding %q", remote.ErrBadResponse, encoding)
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, content)
	if int64(base64.StdEncoding.DecodedLen(len(clean))) > limit+3 {
		return nil, fmt.Errorf("%w: blob over %d bytes", remote.ErrTooLarge, limit)
	}
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("%w: blob content: %v", remote.ErrBadResponse, err)
	}
	return data, nil
}

// blobResponseLimit bounds a JSON blob response for a file of at most n bytes:
// base64 is 4/3 of it, GitHub adds a newline every 60 characters, and the
// envelope is small. 1.4n + 64 KiB covers all three.
func blobResponseLimit(n int64) int64 { return n*14/10 + 64<<10 }

// esc escapes one path segment.
func esc(s string) string { return url.PathEscape(s) }

// escRepo escapes each segment of "owner/name".
func escRepo(repo string) string {
	segs := strings.Split(repo, "/")
	for i, s := range segs {
		segs[i] = esc(s)
	}
	return strings.Join(segs, "/")
}

// escFile escapes each segment of a file path, keeping the slashes.
func escFile(p string) string { return escRepo(p) }

// precheck validates a proposal before any request.
func (c *client) precheck(base remote.Base, files remote.Files, p remote.Proposal, nested bool) ([]remote.Change, error) {
	if err := remote.CheckRepo(base.Repo, nested); err != nil {
		return nil, err
	}
	if err := remote.CheckProposal(base, p); err != nil {
		return nil, err
	}
	if err := remote.RequirePrefix(p.Branch, c.prefix); err != nil {
		return nil, err
	}
	changes, err := remote.Changes(base, files, c.lim)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, remote.ErrNoChanges
	}
	return changes, nil
}

// guardBranch refuses the default branch and any branch that already exists,
// before anything is written.
func (c *client) guardBranch(ctx context.Context, p remote.Proposal, defaultBranch string, exists func(context.Context, string) (bool, error)) error {
	if err := remote.CheckBranch(p.Branch, defaultBranch); err != nil {
		return err
	}
	ok, err := exists(ctx, p.Branch)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%w: %q", remote.ErrBranchExists, p.Branch)
	}
	return nil
}
