// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

package browsergit

// The in-browser half of the e2e proof. Compiled to GOOS=js GOARCH=wasm
// and run under Node with the argv0 spoof (see e2e_test.go), this drives
// the REAL WHATWG Fetch RoundTripper — the browser transport — through
// the full clone → read → write → commit → push cycle against a live
// CORS-enabled git origin. The native orchestrator then witnesses the
// pushed commit + file in the bare repo. This file never builds on the
// native lane, so it contributes nothing to native line coverage; its
// evidence is the console output + the witness repo.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestWasmFetchCycle(t *testing.T) {
	base := os.Getenv("BROWSERGIT_BASE_URL")
	if base == "" {
		t.Skip("BROWSERGIT_BASE_URL unset — run via the e2e orchestrator")
	}
	c := New(Options{
		BaseURL:  base,
		Token:    os.Getenv("BROWSERGIT_TOKEN"),
		Author:   "wasm client",
		Email:    "wasm@browser.local",
		Provider: "generic",
	})
	ctx := context.Background()

	repo, err := c.Clone(ctx, os.Getenv("BROWSERGIT_REPO"), "main", 1)
	if err != nil {
		t.Fatalf("CLONE_FAIL: %v", err)
	}
	t.Log("CLONE_OK (Fetch RoundTripper)")

	seed, err := repo.ReadFile("README.md")
	if err != nil || !strings.HasPrefix(string(seed), "# seed") {
		t.Fatalf("READ_FAIL: %q %v", seed, err)
	}
	t.Logf("READ_OK README.md (%d bytes)", len(seed))

	if err := repo.WriteFile("from-wasm.txt", []byte(WasmMarker)); err != nil {
		t.Fatalf("WRITE_FAIL: %v", err)
	}
	hash, err := repo.Commit("wasm client commit over Fetch")
	if err != nil || hash == "" {
		t.Fatalf("COMMIT_FAIL: %q %v", hash, err)
	}
	t.Logf("COMMIT_OK %s", hash)

	if err := repo.Push(ctx); err != nil {
		t.Fatalf("PUSH_FAIL: %v", err)
	}
	// This exact line is what the orchestrator greps for.
	t.Logf("WASM_CYCLE_OK head=%s", hash)
}
