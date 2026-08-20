// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

// A test-only smart-HTTP git origin: a real bare repository served by
// git's own git-http-backend over CGI, fronted by an httptest.Server that
// adds the permissive CORS headers a sovereign proxy / CORS-enabled
// Forgejo would add, plus an optional BasicAuth gate so the auth-failure
// path can be exercised. This mirrors the feasibility study's gitserver
// exactly; it is fixture code, so it uses the git CLI freely — the
// shipped browsergit package itself is pure go-git with no exec.

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitBackend locates git-http-backend, skipping the test if git is not
// installed (never on CI ubuntu / macOS-with-Xcode, both of which ship
// git). Returns the absolute backend path.
func gitBackend(t *testing.T) string {
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

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@test",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// seedBareRepo creates <root>/<name>.git as a push-enabled bare repo
// seeded with one commit on `main` containing README.md, and returns its
// absolute path. seedFiles maps extra path->content committed alongside.
func seedBareRepo(t *testing.T, root, name string, seedFiles map[string]string) string {
	t.Helper()
	bare := filepath.Join(root, name+".git")
	runGit(t, root, "init", "--bare", "-b", "main", bare)
	// git-http-backend only serves receive-pack (push) when this is set.
	runGit(t, bare, "config", "http.receivepack", "true")

	work := filepath.Join(root, name+"-seed")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-b", "main")
	writeSeed(t, work, "README.md", "# seed\n")
	for p, c := range seedFiles {
		writeSeed(t, work, p, c)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "origin", "main")
	return bare
}

func writeSeed(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitAndPushExternal advances the given bare repo's main branch by one
// commit made through a throwaway clone, simulating another client having
// pushed. Used to force the non-fast-forward path.
func commitAndPushExternal(t *testing.T, root, bare, filename, content string) {
	t.Helper()
	work := filepath.Join(root, "external-"+filename)
	runGit(t, root, "clone", bare, work)
	writeSeed(t, work, filename, content)
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "external "+filename)
	runGit(t, work, "push", "origin", "main")
}

// gitOrigin is a running smart-HTTP git server over a project root.
type gitOrigin struct {
	*httptest.Server
	root string
}

// startOrigin serves everything under root via git-http-backend with CORS
// headers. If requiredToken is non-empty, every request must carry
// BasicAuth whose password equals it, else 401 — this drives the auth
// path. The URL for a repo is srv.URL + "/" + name + ".git".
func startOrigin(t *testing.T, root, requiredToken string) *gitOrigin {
	t.Helper()
	backend := gitBackend(t)
	h := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Git-Protocol,Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Type,Content-Length")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if requiredToken != "" {
			_, pass, ok := r.BasicAuth()
			if !ok || pass != requiredToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &gitOrigin{Server: srv, root: root}
}
