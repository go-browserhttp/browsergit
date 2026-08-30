// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

package browsergit

import (
	"context"
	"strings"
	"testing"
)

// A credential belongs in the Authorization header. Put one in a remote URL and
// go-git records that URL in .git/config — so it lands in the repository, and
// from there in every copy of the repository, including the one the browser
// keeps. These tests pin that it cannot happen.
//
// The fixtures are deliberately NOT shaped like real tokens: a realistic secret
// in a test file is one copy-paste away from being treated as a real one.

const testCredential = "CREDENTIAL-PLACEHOLDER"

func TestASnapshotCannotCarryACredential(t *testing.T) {
	root := t.TempDir()
	seedBareRepo(t, root, "demo", map[string]string{"a.tex": "x"})
	srv := startOrigin(t, root, "")

	// A reader pasting a remote that carries their credential — which is what
	// the panel's URL field accepts.
	withCred := strings.Replace(srv.URL, "http://", "http://user:"+testCredential+"@", 1)
	c := New(Options{BaseURL: withCred})
	repo, err := c.Clone(context.Background(), "demo.git", "main", 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// The repository's own config is where it would land first.
	cfg, err := repo.ReadFile(".git/config")
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if strings.Contains(string(cfg), testCredential) {
		t.Errorf(".git/config carries the credential:\n%s", cfg)
	}

	// And therefore in anything that stores the repository.
	snap, err := repo.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if strings.Contains(string(snap), testCredential) {
		t.Error("the snapshot carries the credential; storing it would persist the secret past the session")
	}

	// Stripping it must not break the clone: the credential moved to the header,
	// so the repository is still fully usable.
	files, err := repo.List()
	if err != nil || len(files) == 0 {
		t.Fatalf("the clone came back empty after stripping the credential: %v %v", files, err)
	}
}

func TestSplitCredential(t *testing.T) {
	for _, tc := range []struct {
		name             string
		url, token       string
		wantURL, wantTok string
	}{
		{
			name: "password becomes the token",
			url:  "https://user:" + testCredential + "@forge.example/o/r.git",
			//
			wantURL: "https://forge.example/o/r.git", wantTok: testCredential,
		},
		{
			name:    "username-only is the token-as-username form",
			url:     "https://" + testCredential + "@forge.example/o/r.git",
			wantURL: "https://forge.example/o/r.git", wantTok: testCredential,
		},
		{
			name:  "an explicit token wins over one in the URL",
			url:   "https://user:" + testCredential + "@forge.example/o/r.git",
			token: "typed-by-the-reader",
			// The URL is still stripped: the point is that nothing carries a
			// credential onward, not merely that the right one is used.
			wantURL: "https://forge.example/o/r.git", wantTok: "typed-by-the-reader",
		},
		{
			name:    "a clean URL is left exactly as it is",
			url:     "https://forge.example/o/r.git",
			wantURL: "https://forge.example/o/r.git",
		},
		{
			name:    "an empty URL is not invented",
			url:     "",
			wantURL: "",
		},
		{
			name:    "something that is not a URL is left alone",
			url:     "owner/repo.git",
			wantURL: "owner/repo.git",
		},
		{
			name:    "an unparseable URL is left alone rather than mangled",
			url:     "http://[::1]:namedport/x",
			wantURL: "http://[::1]:namedport/x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotTok := SplitCredential(tc.url, tc.token)
			if gotURL != tc.wantURL {
				t.Errorf("url = %q, want %q", gotURL, tc.wantURL)
			}
			if gotTok != tc.wantTok {
				t.Errorf("token = %q, want %q", gotTok, tc.wantTok)
			}
			if strings.Contains(gotURL, testCredential) {
				t.Errorf("the cleaned URL still carries the credential: %q", gotURL)
			}
		})
	}
}

func TestNewStripsTheCredentialFromEveryClient(t *testing.T) {
	// The split happens at New, the one door every caller comes through, so no
	// code path can smuggle a credential past it.
	c := New(Options{BaseURL: "https://user:" + testCredential + "@forge.example/o/r.git"})
	if strings.Contains(c.repoURL(""), testCredential) {
		t.Errorf("the client's remote URL still carries the credential: %q", c.repoURL(""))
	}
	if c.opts.Token != testCredential {
		t.Errorf("token = %q, want the credential moved into it (and thus into the header)", c.opts.Token)
	}
	if auth := c.auth(); auth == nil {
		t.Fatal("no auth method was built, so the moved credential would never be sent")
	}
}
