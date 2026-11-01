// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package browsergit

// Deterministic headless e2e that proves the browser Fetch transport
// end-to-end. It is opt-in (BROWSERGIT_WASM_E2E=1) because it needs a
// Node runtime and the js/wasm toolchain; the default `go test` lane runs
// only the native suite. When enabled it:
//
//  1. seeds + serves a CORS-enabled git origin (git-http-backend),
//  2. compiles the package's js cycle test to a wasm binary,
//  3. runs it under Node through the argv0 spoof from the feasibility
//     study — Object.create(process) with argv0 rewritten so Go's
//     net/http does NOT set jsFetchDisabled and therefore takes the REAL
//     WHATWG Fetch RoundTripper (the browser path) instead of the dead
//     Node socket path,
//  4. witnesses the pushed commit + file by re-cloning the bare repo
//     with a native client.
//
// This is the CI-runnable form of the study's wasmclone + run_fetch.js.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runFetchJS is the argv0 spoof, verbatim in spirit from the study's
// run_fetch.js: wrap the real process so its argv0 is a non-"node"
// string, defeating Go's jsFetchDisabled heuristic, then delegate to the
// standard wasm_exec_node.js named by WASM_EXEC.
const runFetchJS = `// argv0 spoof: force Go's js/wasm net/http onto the real Fetch RoundTripper.
const real = process;
const wrapper = Object.create(Object.getPrototypeOf(real));
for (const k of Reflect.ownKeys(real)) {
  if (k === 'argv0') continue;
  Object.defineProperty(wrapper, k, Object.getOwnPropertyDescriptor(real, k));
}
Object.defineProperty(wrapper, 'argv0', { value: 'browserproof', enumerable: true, configurable: true });
globalThis.process = wrapper;
require(process.env.WASM_EXEC);
`

func TestWasmFetchE2E(t *testing.T) {
	if os.Getenv("BROWSERGIT_WASM_E2E") == "" {
		t.Skip("set BROWSERGIT_WASM_E2E=1 to run the Node-fetch wasm e2e (needs node + js/wasm toolchain)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not found: %v", err)
	}
	wasmExec := filepath.Join(runtime.GOROOT(), "lib", "wasm", "wasm_exec_node.js")
	if _, err := os.Stat(wasmExec); err != nil {
		t.Skipf("wasm_exec_node.js not found at %s: %v", wasmExec, err)
	}

	// 1. Seed + serve a CORS-enabled origin.
	root := t.TempDir()
	seedBareRepo(t, root, "e2e", nil)
	srv := startOrigin(t, root, "")

	// 2. Compile the js cycle test to a wasm binary. The build runs in
	// the package dir (the test's cwd), where the js-tagged cycle test
	// lives; all !js test files are excluded by build constraints.
	tmp := t.TempDir()
	wasmBin := filepath.Join(tmp, "browsergit.test.wasm")
	build := exec.Command("go", "test", "-c", "-o", wasmBin, ".")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wasm test: %v\n%s", err, out)
	}

	// 3. Write the spoof shim + run the wasm test under Node.
	shim := filepath.Join(tmp, "run_fetch.js")
	if err := os.WriteFile(shim, []byte(runFetchJS), 0o644); err != nil {
		t.Fatal(err)
	}
	run := exec.Command(node, shim, wasmBin, "-test.run", "TestWasmFetchCycle", "-test.v")
	run.Env = append(os.Environ(),
		"WASM_EXEC="+wasmExec,
		"BROWSERGIT_BASE_URL="+srv.URL,
		"BROWSERGIT_REPO=e2e.git",
	)
	out, err := run.CombinedOutput()
	t.Logf("=== wasm/node output ===\n%s", out)
	if err != nil {
		t.Fatalf("node wasm run failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "WASM_CYCLE_OK") || !strings.Contains(got, "CLONE_OK (Fetch RoundTripper)") {
		t.Fatalf("wasm cycle did not report success; output above")
	}
	if !strings.Contains(got, "PASS") {
		t.Fatalf("wasm test did not PASS; output above")
	}

	// 4. Witness: a fresh native clone must see the file the wasm client
	// pushed over Fetch. This proves the push reached the bare origin.
	c := New(Options{BaseURL: srv.URL})
	witness, err := c.Clone(context.Background(), "e2e.git", "main", 1)
	if err != nil {
		t.Fatalf("witness clone: %v", err)
	}
	data, err := witness.ReadFile("from-wasm.txt")
	if err != nil || string(data) != WasmMarker {
		t.Fatalf("witness read from-wasm.txt = %q, %v; want %q — push did NOT land", data, err, WasmMarker)
	}
	t.Log("WITNESS_OK: pushed file present in the bare origin")
}
