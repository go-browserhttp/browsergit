// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

// Package gittest runs a real smart-HTTP git origin for tests: bare
// repositories served by git's own git-http-backend over CGI behind an
// httptest.Server. It is fixture code and uses the git CLI; nothing shipped
// imports it. It mirrors the root package's testserver_test.go, exported so the
// remote and forge tests can share it.
package gittest

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Backend locates git-http-backend, skipping the test when git is missing.
func Backend(t testing.TB) string {
	t.Helper()
	if p, err := exec.LookPath("git-http-backend"); err == nil {
		return p
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("git-http-backend not found at %s: %v", p, err)
	}
	return p
}

// Git runs git in dir with a fixed identity and no user configuration.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@test",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write writes content at dir/rel, creating parents.
func Write(t testing.TB, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Seed creates root/name.git, push-enabled, with one commit on main holding
// files, and returns the bare repository path.
func Seed(t testing.TB, root, name string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(root, name+".git")
	Git(t, root, "init", "--bare", "-b", "main", bare)
	Git(t, bare, "config", "http.receivepack", "true")
	work := filepath.Join(root, name+"-seed")
	Git(t, root, "init", "-b", "main", work)
	for p, c := range files {
		Write(t, work, p, c)
	}
	Git(t, work, "add", "-A")
	Git(t, work, "commit", "-m", "seed")
	Git(t, work, "remote", "add", "origin", bare)
	Git(t, work, "push", "origin", "main")
	return bare
}

// Advance commits one file to branch main of bare through a throwaway clone.
func Advance(t testing.TB, root, bare, file, content string) {
	t.Helper()
	work, err := os.MkdirTemp(root, "advance-")
	if err != nil {
		t.Fatal(err)
	}
	Git(t, root, "clone", "-q", bare, work)
	Write(t, work, file, content)
	Git(t, work, "add", "-A")
	Git(t, work, "commit", "-m", "advance "+file)
	Git(t, work, "push", "-q", "origin", "main")
}

// Origin is a running smart-HTTP origin.
type Origin struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string // the Authorization header of every request, in order
}

// Auth returns the Authorization headers seen so far.
func (o *Origin) Auth() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.auth...)
}

// Serve serves every repository under root. When token is non-empty every
// request must carry Basic auth whose password is token, or gets 401.
func Serve(t testing.TB, root, token string) *Origin {
	t.Helper()
	h := &cgi.Handler{
		Path: Backend(t),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	o := &Origin{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.auth = append(o.auth, r.Header.Get("Authorization"))
		o.mu.Unlock()
		if token != "" {
			if _, pass, ok := r.BasicAuth(); !ok || pass != token {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(o.Close)
	return o
}
