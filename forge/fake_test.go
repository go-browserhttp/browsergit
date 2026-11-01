// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package forge

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-browserhttp/browsergit/remote"
)

// store is an in-memory git object model: blobs, trees (path -> blob id),
// commits (-> tree), branches. The three fake APIs below are faces on it.
type store struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	trees    map[string]map[string]string
	commits  map[string]string
	branches map[string]string
	def      string
	prs      []map[string]any
	writes   int      // mutating requests
	requests []string // "METHOD path?query"
	auth     []string // Authorization header of every request
}

func newStore(files map[string]string) *store {
	s := &store{blobs: map[string][]byte{}, trees: map[string]map[string]string{}, commits: map[string]string{}, branches: map[string]string{}, def: "main"}
	tree := map[string]string{}
	for p, c := range files {
		tree[p] = s.putBlob([]byte(c))
	}
	s.branches["main"] = s.commit(s.putTree(tree), "seed")
	return s
}

func (s *store) putBlob(b []byte) string {
	id := remote.BlobID(b)
	s.blobs[id] = b
	return id
}

func hashOf(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *store) putTree(t map[string]string) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k+"="+t[k])
	}
	sort.Strings(keys)
	id := hashOf(append([]string{"tree"}, keys...)...)
	s.trees[id] = t
	return id
}

func (s *store) commit(tree, msg string) string {
	id := hashOf("commit", tree, msg, strconv.Itoa(len(s.commits)))
	s.commits[id] = tree
	return id
}

// files returns the content of branch's tip.
func (s *store) files(branch string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for p, b := range s.trees[s.commits[s.branches[branch]]] {
		out[p] = string(s.blobs[b])
	}
	return out
}

// advance commits files onto main, as another person would.
func (s *store) advance(files map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := map[string]string{}
	for p, b := range s.trees[s.commits[s.branches["main"]]] {
		t[p] = b
	}
	for p, c := range files {
		t[p] = s.putBlob([]byte(c))
	}
	s.branches["main"] = s.commit(s.putTree(t), "advance")
}

// hostile configures misbehaviour.
type hostile struct {
	echoToken  bool   // every error body echoes the Authorization header
	redirectTo string // every request is redirected there
	extraEntry string // a tree listing also lists this path
	prURL      string // the pull request URL returned
	hugeBlob   bool   // blob responses are enormous
	wrongBlob  bool   // blob content does not match its id
	failPR     bool   // pull request creation fails
}

type fake struct {
	*store
	t    *testing.T
	kind Kind
	srv  *httptest.Server
	h    hostile
	page int // tree page size for paginated faces
}

func newFake(t *testing.T, kind Kind, files map[string]string, h hostile) *fake {
	f := &fake{store: newStore(files), t: t, kind: kind, h: h, page: 2}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) api() string {
	switch f.kind {
	case GitLab:
		return f.srv.URL + "/api/v4"
	case Gitea:
		return f.srv.URL + "/api/v1"
	}
	return f.srv.URL
}

func (f *fake) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if f.h.echoToken {
		msg += " (you sent " + r.Header.Get("Authorization") + ")"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (f *fake) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.Method != http.MethodGet {
		f.writes++
	}
	f.mu.Unlock()
	if f.h.redirectTo != "" {
		http.Redirect(w, r, f.h.redirectTo+r.URL.Path, http.StatusFound)
		return
	}
	if r.Header.Get("Authorization") == "" {
		f.fail(w, r, http.StatusUnauthorized, "requires authentication")
		return
	}
	if !f.authOK(r.Header.Get("Authorization")) {
		f.fail(w, r, http.StatusUnauthorized, "bad credentials")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.kind {
	case GitHub:
		f.github(w, r)
	case GitLab:
		f.gitlab(w, r)
	case Gitea:
		f.gitea(w, r)
	}
}

const goodToken = "FORGE-TOKEN-PLACEHOLDER"

func (f *fake) authOK(h string) bool {
	switch f.kind {
	case Gitea:
		return h == "token "+goodToken
	}
	return h == "Bearer "+goodToken
}

func (f *fake) treeEntries(tree string) []map[string]any {
	t := f.trees[tree]
	keys := make([]string, 0, len(t)+1)
	for p := range t {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	var out []map[string]any
	if f.h.extraEntry != "" {
		out = append(out, map[string]any{"path": f.h.extraEntry, "mode": "100644", "type": "blob", "sha": remote.BlobID(nil), "id": remote.BlobID(nil), "size": 0})
	}
	dirs := map[string]bool{}
	for _, p := range keys {
		if i := strings.LastIndex(p, "/"); i > 0 && !dirs[p[:i]] {
			dirs[p[:i]] = true
			out = append(out, map[string]any{"path": p[:i], "mode": "040000", "type": "tree", "sha": hashOf(p[:i]), "id": hashOf(p[:i])})
		}
		out = append(out, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": t[p], "id": t[p], "size": len(f.blobs[t[p]])})
	}
	return out
}

func (f *fake) blobBytes(id string) []byte {
	b := f.blobs[id]
	if f.h.wrongBlob {
		b = append([]byte("tampered "), b...)
	}
	if f.h.hugeBlob {
		b = make([]byte, 64<<20)
	}
	return b
}

func wrap60(s string) string {
	var b strings.Builder
	for len(s) > 60 {
		b.WriteString(s[:60] + "\n")
		s = s[60:]
	}
	b.WriteString(s)
	return b.String()
}

func decodeBody(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// --- GitHub face ---------------------------------------------------------

func (f *fake) github(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	if p == r.URL.Path {
		f.fail(w, r, 404, "Not Found")
		return
	}
	switch {
	case r.Method == "GET" && p == "":
		f.reply(w, 200, map[string]any{"default_branch": f.def})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/ref/heads/"):
		c, ok := f.branches[strings.TrimPrefix(p, "/git/ref/heads/")]
		if !ok {
			f.fail(w, r, 404, "Not Found")
			return
		}
		f.reply(w, 200, map[string]any{"object": map[string]string{"type": "commit", "sha": c}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		id := strings.TrimPrefix(p, "/git/commits/")
		tree, ok := f.commits[id]
		if !ok {
			f.fail(w, r, 404, "Not Found")
			return
		}
		f.reply(w, 200, map[string]any{"sha": id, "tree": map[string]string{"sha": tree}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
		id := strings.TrimPrefix(p, "/git/trees/")
		if _, ok := f.trees[id]; !ok || r.URL.Query().Get("recursive") == "" {
			f.fail(w, r, 404, "Not Found")
			return
		}
		f.reply(w, 200, map[string]any{"sha": id, "truncated": false, "tree": f.treeEntries(id)})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/blobs/"):
		id := strings.TrimPrefix(p, "/git/blobs/")
		if _, ok := f.blobs[id]; !ok {
			f.fail(w, r, 404, "Not Found")
			return
		}
		f.reply(w, 200, map[string]any{"sha": id, "encoding": "base64", "content": wrap60(base64.StdEncoding.EncodeToString(f.blobBytes(id)))})
	case r.Method == "POST" && p == "/git/blobs":
		var b struct{ Content, Encoding string }
		if decodeBody(r, &b) != nil || b.Encoding != "base64" {
			f.fail(w, r, 422, "bad blob")
			return
		}
		data, err := base64.StdEncoding.DecodeString(b.Content)
		if err != nil {
			f.fail(w, r, 422, "bad base64")
			return
		}
		f.reply(w, 201, map[string]string{"sha": f.putBlob(data)})
	case r.Method == "POST" && p == "/git/trees":
		var b struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path, Mode, Type string
				SHA              *string `json:"sha"`
			} `json:"tree"`
		}
		if decodeBody(r, &b) != nil {
			f.fail(w, r, 422, "bad tree")
			return
		}
		base, ok := f.trees[b.BaseTree]
		if !ok {
			f.fail(w, r, 404, "base tree")
			return
		}
		t := map[string]string{}
		for k, v := range base {
			t[k] = v
		}
		for _, e := range b.Tree {
			if e.SHA == nil {
				if _, ok := t[e.Path]; !ok {
					f.fail(w, r, 422, "delete of a missing file")
					return
				}
				delete(t, e.Path)
				continue
			}
			if _, ok := f.blobs[*e.SHA]; !ok {
				f.fail(w, r, 422, "unknown blob")
				return
			}
			t[e.Path] = *e.SHA
		}
		f.reply(w, 201, map[string]string{"sha": f.putTree(t)})
	case r.Method == "POST" && p == "/git/commits":
		var b struct {
			Message string
			Tree    string
			Parents []string
		}
		if decodeBody(r, &b) != nil || len(b.Parents) != 1 {
			f.fail(w, r, 422, "bad commit")
			return
		}
		if _, ok := f.trees[b.Tree]; !ok {
			f.fail(w, r, 422, "unknown tree")
			return
		}
		f.reply(w, 201, map[string]any{"sha": f.commit(b.Tree, b.Message)})
	case r.Method == "POST" && p == "/git/refs":
		var b struct{ Ref, SHA string }
		if decodeBody(r, &b) != nil || !strings.HasPrefix(b.Ref, "refs/heads/") {
			f.fail(w, r, 422, "bad ref")
			return
		}
		name := strings.TrimPrefix(b.Ref, "refs/heads/")
		if _, ok := f.branches[name]; ok {
			f.fail(w, r, 422, "Reference already exists")
			return
		}
		f.branches[name] = b.SHA
		f.reply(w, 201, map[string]any{"ref": b.Ref})
	case r.Method == "POST" && p == "/pulls":
		f.createPR(w, r, "head", "base", "html_url", "/o/r/pull/")
	default:
		f.fail(w, r, 404, "Not Found")
	}
}

func (f *fake) createPR(w http.ResponseWriter, r *http.Request, headKey, baseKey, urlKey, urlPath string) {
	var b map[string]any
	if decodeBody(r, &b) != nil {
		f.fail(w, r, 422, "bad pr")
		return
	}
	if f.h.failPR {
		f.fail(w, r, 422, "pull request refused")
		return
	}
	if _, ok := f.branches[fmt.Sprint(b[headKey])]; !ok {
		f.fail(w, r, 422, "head does not exist")
		return
	}
	f.prs = append(f.prs, b)
	u := f.srv.URL + urlPath + strconv.Itoa(len(f.prs))
	if f.h.prURL != "" {
		u = f.h.prURL
	}
	f.reply(w, 201, map[string]any{urlKey: u})
}

// --- GitLab face ---------------------------------------------------------

func (f *fake) gitlab(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.EscapedPath()
	const proj = "/api/v4/projects/o%2Fr"
	if !strings.HasPrefix(raw, proj) {
		f.fail(w, r, 404, "404 Project Not Found")
		return
	}
	p := strings.TrimPrefix(raw, proj)
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && p == "":
		f.reply(w, 200, map[string]any{"default_branch": f.def})
	case r.Method == "GET" && strings.HasPrefix(p, "/repository/branches/"):
		name, _ := url.PathUnescape(strings.TrimPrefix(p, "/repository/branches/"))
		c, ok := f.branches[name]
		if !ok {
			f.fail(w, r, 404, "404 Branch Not Found")
			return
		}
		f.reply(w, 200, map[string]any{"name": name, "commit": map[string]string{"id": c}})
	case r.Method == "GET" && p == "/repository/tree":
		tree, ok := f.trees[f.commits[q.Get("ref")]]
		if !ok || q.Get("recursive") != "true" {
			f.fail(w, r, 404, "404 Tree Not Found")
			return
		}
		_ = tree
		all := f.treeEntries(f.commits[q.Get("ref")])
		if dir := q.Get("path"); dir != "" {
			var in []map[string]any
			for _, e := range all {
				if strings.HasPrefix(e["path"].(string), dir+"/") {
					in = append(in, e)
				}
			}
			all = in
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}
		lo, hi := (page-1)*f.page, page*f.page
		if lo > len(all) {
			lo = len(all)
		}
		if hi >= len(all) {
			hi = len(all)
		} else {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		// A Link header pointing elsewhere must never be followed.
		w.Header().Set("Link", `<https://evil.example/steal>; rel="next"`)
		f.reply(w, 200, all[lo:hi])
	case r.Method == "GET" && strings.HasPrefix(p, "/repository/blobs/") && strings.HasSuffix(p, "/raw"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/repository/blobs/"), "/raw")
		if _, ok := f.blobs[id]; !ok {
			f.fail(w, r, 404, "404 Blob Not Found")
			return
		}
		_, _ = w.Write(f.blobBytes(id))
	case r.Method == "POST" && p == "/repository/commits":
		var b struct {
			Branch   string `json:"branch"`
			StartSHA string `json:"start_sha"`
			Force    bool   `json:"force"`
			Message  string `json:"commit_message"`
			Actions  []struct {
				Action   string `json:"action"`
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
				Encoding string `json:"encoding"`
			} `json:"actions"`
		}
		if decodeBody(r, &b) != nil || b.Force {
			f.fail(w, r, 400, "bad commit")
			return
		}
		if _, ok := f.branches[b.Branch]; ok {
			f.fail(w, r, 400, "A branch called '"+b.Branch+"' already exists")
			return
		}
		baseTree, ok := f.trees[f.commits[b.StartSHA]]
		if !ok {
			f.fail(w, r, 400, "start_sha")
			return
		}
		t := map[string]string{}
		for k, v := range baseTree {
			t[k] = v
		}
		for _, a := range b.Actions {
			_, exists := t[a.FilePath]
			switch a.Action {
			case "create", "update":
				if (a.Action == "create") == exists {
					f.fail(w, r, 400, a.Action+" "+a.FilePath)
					return
				}
				data, err := base64.StdEncoding.DecodeString(a.Content)
				if err != nil || a.Encoding != "base64" {
					f.fail(w, r, 400, "content")
					return
				}
				t[a.FilePath] = f.putBlob(data)
			case "delete":
				if !exists {
					f.fail(w, r, 400, "delete "+a.FilePath)
					return
				}
				delete(t, a.FilePath)
			default:
				f.fail(w, r, 400, "action")
				return
			}
		}
		c := f.commit(f.putTree(t), b.Message)
		f.branches[b.Branch] = c
		f.reply(w, 201, map[string]any{"id": c})
	case r.Method == "POST" && p == "/merge_requests":
		f.createPR(w, r, "source_branch", "target_branch", "web_url", "/o/r/-/merge_requests/")
	default:
		f.fail(w, r, 404, "404 Not Found")
	}
}

// --- Gitea face ----------------------------------------------------------

func (f *fake) gitea(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/o/r")
	if p == r.URL.Path {
		f.fail(w, r, 404, "not found")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && p == "":
		f.reply(w, 200, map[string]any{"default_branch": f.def})
	case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
		name := strings.TrimPrefix(p, "/branches/")
		c, ok := f.branches[name]
		if !ok {
			f.fail(w, r, 404, "branch not found")
			return
		}
		f.reply(w, 200, map[string]any{"name": name, "commit": map[string]string{"id": c}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
		tree, ok := f.commits[strings.TrimPrefix(p, "/git/trees/")]
		if !ok || q.Get("recursive") != "true" {
			f.fail(w, r, 404, "tree not found")
			return
		}
		all := f.treeEntries(tree)
		page, _ := strconv.Atoi(q.Get("page"))
		lo, hi := (page-1)*f.page, page*f.page
		if lo > len(all) {
			lo = len(all)
		}
		if hi > len(all) {
			hi = len(all)
		}
		f.reply(w, 200, map[string]any{"sha": tree, "tree": all[lo:hi], "truncated": hi < len(all), "page": page, "total_count": len(all)})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/blobs/"):
		id := strings.TrimPrefix(p, "/git/blobs/")
		if _, ok := f.blobs[id]; !ok {
			f.fail(w, r, 404, "blob not found")
			return
		}
		b := f.blobBytes(id)
		f.reply(w, 200, map[string]any{"sha": id, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(b), "size": len(b)})
	case r.Method == "POST" && p == "/contents":
		var b struct {
			Branch    string `json:"branch"`
			NewBranch string `json:"new_branch"`
			ForcePush bool   `json:"force_push"`
			Message   string `json:"message"`
			Files     []struct {
				Operation, Path, Content, SHA string
			} `json:"files"`
		}
		if decodeBody(r, &b) != nil || b.ForcePush {
			f.fail(w, r, 422, "bad request")
			return
		}
		if _, ok := f.branches[b.NewBranch]; ok {
			f.fail(w, r, 422, "branch already exists")
			return
		}
		baseTree := f.trees[f.commits[f.branches[b.Branch]]]
		t := map[string]string{}
		for k, v := range baseTree {
			t[k] = v
		}
		for _, op := range b.Files {
			cur, exists := t[op.Path]
			switch op.Operation {
			case "create":
				if exists {
					f.fail(w, r, 422, "file exists")
					return
				}
			case "update", "delete":
				if !exists || cur != op.SHA {
					f.fail(w, r, 409, "sha does not match")
					return
				}
			default:
				f.fail(w, r, 422, "operation")
				return
			}
			if op.Operation == "delete" {
				delete(t, op.Path)
				continue
			}
			data, err := base64.StdEncoding.DecodeString(op.Content)
			if err != nil {
				f.fail(w, r, 422, "content")
				return
			}
			t[op.Path] = f.putBlob(data)
		}
		c := f.commit(f.putTree(t), b.Message)
		f.branches[b.NewBranch] = c
		f.reply(w, 201, map[string]any{"commit": map[string]string{"sha": c}})
	case r.Method == "POST" && p == "/pulls":
		f.createPR(w, r, "head", "base", "html_url", "/o/r/pulls/")
	default:
		f.fail(w, r, 404, "not found")
	}
}
