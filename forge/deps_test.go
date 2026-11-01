// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package forge

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTheBrowserBuildDoesNotLinkNetHTTP pins the Fetch transport: in js/wasm,
// net/http brings crypto/tls and x509 (about 8 MB) only to call fetch().
func TestTheBrowserBuildDoesNotLinkNetHTTP(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "github.com/go-browserhttp/browsergit/forge")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v\n%s", err, out)
	}
	deps := "\n" + string(out)
	if !strings.Contains(deps, "\nsyscall/js\n") {
		t.Fatal("control: the js build should use syscall/js")
	}
	for _, banned := range []string{"\nnet/http\n", "\ncrypto/tls\n", "\ncrypto/x509\n"} {
		if strings.Contains(deps, banned) {
			t.Errorf("the js/wasm build of forge links %s", strings.TrimSpace(banned))
		}
	}
}

// TestForgeDoesNotLinkGoGit pins the package split: a page that only talks REST
// must not pay for go-git (about 13 MB of js/wasm). Linking it again, through
// remote or a new import, fails here.
func TestForgeDoesNotLinkGoGit(t *testing.T) {
	// The control: smarthttp does link go-git, and the check must see it.
	if out, err := exec.Command("go", "list", "-deps", "github.com/go-browserhttp/browsergit/smarthttp").CombinedOutput(); err == nil && !strings.Contains(string(out), "github.com/go-git/") {
		t.Fatal("control: smarthttp should link go-git; the check cannot see it")
	}
	for _, pkg := range []string{"github.com/go-browserhttp/browsergit/forge", "github.com/go-browserhttp/browsergit/remote"} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Skipf("go list unavailable: %v\n%s", err, out)
		}
		deps := string(out)
		if !strings.Contains(deps, "github.com/go-browserhttp/browsergit/secret") && pkg != "github.com/go-browserhttp/browsergit/remote" {
			t.Fatalf("control: %s should depend on secret", pkg)
		}
		for _, banned := range []string{"github.com/go-git/", "github.com/go-browserhttp/browsergit\n"} {
			if strings.Contains(deps, banned) {
				t.Errorf("%s links %q", pkg, strings.TrimSpace(banned))
			}
		}
	}
}
