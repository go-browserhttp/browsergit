// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

package browsergit

// WasmMarker is the exact content the wasm client writes + pushes so the
// native e2e orchestrator can witness it round-tripping through the
// browser Fetch RoundTripper. Kept in an untagged test file so both the
// js cycle test and the native orchestrator reference the same literal.
const WasmMarker = "written by go-git over the browser Fetch RoundTripper\n"
