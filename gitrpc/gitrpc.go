// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package gitrpc is the message protocol shared by the playground's main app and
// its off-thread git worker. The heavy remote-git client (go-git, via
// internal/browsergit) lives ONLY in the worker binary (cmd/git-worker); the main
// playground.wasm never imports it, so go-git is dead-code-eliminated out of the
// base download. The two halves talk over the Web Worker postMessage channel with
// the JSON request/reply envelopes defined here.
//
// A [Request] carries an op name (one of the Op* constants) and a flat [Args]
// bag; a [Reply] carries the op's result or a stable error [Code] plus detail.
// The wire form is a JSON string in both directions so the encode/decode, the op
// dispatch (in the worker) and the code→error mapping (in the main app) are all
// pure Go, testable natively without a browser. This package imports nothing that
// pulls in go-git, so it is safe to depend on from the main app.
package gitrpc

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Op names. Each is one remote-git operation the worker can perform on the
// long-lived session it holds across calls.
const (
	OpClone     = "clone"     // clone a remote into the worker's memory
	OpRestore   = "restore"   // reopen a repository saved by an earlier visit
	OpForget    = "forget"    // drop the saved copy, so the next visit clones afresh
	OpList      = "list"      // list the working-tree file paths
	OpReadFile  = "readFile"  // read one working-tree file
	OpWriteFile = "writeFile" // write one working-tree file (no commit)
	OpStatus    = "status"    // branch + ahead/behind + dirty snapshot
	OpCommit    = "commit"    // write a file then commit it
	OpStage     = "stage"     // write a file then stage it (git add, no commit)
	OpPull      = "pull"      // fast-forward against origin, refresh contents
	OpPush      = "push"      // push the tracked branch to origin
	OpLog       = "log"       // recent commits, newest first
)

// Error codes. Every worker failure maps onto exactly one of these, so the main
// app branches on a stable string instead of a go-git internal it cannot import.
const (
	CodeAuth           = "auth"             // credential rejected (401/403)
	CodeNonFastForward = "non-fast-forward" // push rejected, remote moved on
	CodeRepoNotFound   = "repo-not-found"   // missing/unexported repository
	CodeTransport      = "transport"        // network/CORS/other origin failure
	CodeNotExist       = "not-exist"        // working-tree file absent
	CodeNoRepo         = "no-repo"          // op needs a clone that has not happened
	CodeBadRequest     = "bad-request"      // malformed request / unknown op
)

// Args is the flat argument bag for a [Request]; each op reads the fields it
// needs and ignores the rest.
type Args struct {
	URL     string `json:"url,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Token   string `json:"token,omitempty"`
	Author  string `json:"author,omitempty"`
	Email   string `json:"email,omitempty"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// Request is one RPC call from the main app to the worker. ID correlates the
// reply; it is assigned by the transport and echoed back verbatim.
type Request struct {
	ID   int    `json:"id"`
	Op   string `json:"op"`
	Args Args   `json:"args,omitempty"`
}

// Change is one dirty working-tree entry: its slash-relative Path and a single
// UI status label ("untracked" | "modified" | "deleted" | "staged"), exactly
// browsergit's classify() vocabulary. The sidebar's per-file badge column reads
// these; a clean tree carries none.
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Status is the branch/divergence snapshot a mutating op returns so the main app
// never needs a separate round-trip to render the panel's status line. Changes
// is the per-file dirty list the workspace sidebar badges each row from;
// DirtyFile is kept as the len(Changes) count the compact status line shows.
type Status struct {
	Branch    string   `json:"branch"`
	Ahead     int      `json:"ahead"`
	Behind    int      `json:"behind"`
	Clean     bool     `json:"clean"`
	DirtyFile int      `json:"dirtyFile"`
	Changes   []Change `json:"changes,omitempty"`
}

// Commit is one log line: the abbreviated hash, subject and author.
type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
}

// Progress is one server-side sideband progress update, forwarded from the
// worker to the main app DURING a long clone/pull (before the terminal [Reply]).
// Line is the raw remote line ("Receiving objects:  45% (450/1000)"); Phase is
// its label ("Receiving objects"); Fraction is the parsed [0,1] completion, or
// -1 when the line carries no measurable ratio (so the UI slides an
// indeterminate bar rather than claiming a dishonest 0%). See [ParseProgress].
type Progress struct {
	Phase    string  `json:"phase,omitempty"`
	Line     string  `json:"line,omitempty"`
	Fraction float64 `json:"fraction"`
}

// ParseProgress turns one raw git sideband progress line into a [Progress]. The
// git server writes lines like "Counting objects: 100% (42/42), done." and
// "Receiving objects:  45% (450/1000), 1.2 MiB | 500 KiB/s"; the label before
// the first colon is the phase, and a "(cur/total)" group gives the truest
// fraction (a plain "NN%" is the fallback). A line with neither reports
// Fraction -1 — "cannot tell" — never a made-up 0%.
func ParseProgress(line string) Progress {
	line = strings.TrimSpace(line)
	p := Progress{Line: line, Fraction: -1}
	if line == "" {
		return p
	}
	if i := strings.IndexByte(line, ':'); i > 0 {
		p.Phase = strings.TrimSpace(line[:i])
	}
	// Prefer the exact (cur/total) count where the server sends one.
	if open := strings.IndexByte(line, '('); open >= 0 {
		if shut := strings.IndexByte(line[open:], ')'); shut > 0 {
			inner := line[open+1 : open+shut]
			if slash := strings.IndexByte(inner, '/'); slash > 0 {
				cur, e1 := strconv.Atoi(strings.TrimSpace(inner[:slash]))
				total, e2 := strconv.Atoi(strings.TrimSpace(inner[slash+1:]))
				if e1 == nil && e2 == nil && total > 0 {
					f := float64(cur) / float64(total)
					if f > 1 {
						f = 1
					}
					p.Fraction = f
					return p
				}
			}
		}
	}
	// Fall back to a bare "NN%" token.
	if pct := strings.IndexByte(line, '%'); pct > 0 {
		start := pct
		for start > 0 && (line[start-1] == ' ' || line[start-1] >= '0' && line[start-1] <= '9') {
			start--
		}
		if n, err := strconv.Atoi(strings.TrimSpace(line[start:pct])); err == nil && n >= 0 {
			f := float64(n) / 100
			if f > 1 {
				f = 1
			}
			p.Fraction = f
		}
	}
	return p
}

// Reply is the worker's answer to a [Request]. On success OK is true and the
// op-specific fields are populated; on failure OK is false and Code (plus the
// human Error detail) explain why. Ready is set only on the one-shot boot message
// the worker posts once its wasm has instantiated.
type Reply struct {
	ID       int               `json:"id"`
	OK       bool              `json:"ok"`
	Ready    bool              `json:"ready,omitempty"`
	Code     string            `json:"code,omitempty"`
	Error    string            `json:"error,omitempty"`
	Files    []string          `json:"files,omitempty"`
	Contents map[string]string `json:"contents,omitempty"`
	Content  string            `json:"content,omitempty"`
	HasRepo  bool              `json:"hasRepo,omitempty"`
	// Restored answers [OpRestore] alone: whether a saved repository was found
	// and reopened. A false Restored with OK true is not a failure — it means
	// this browser has never held this repository, and the caller should clone.
	Restored bool     `json:"restored,omitempty"`
	Status   *Status  `json:"status,omitempty"`
	Log      []Commit `json:"log,omitempty"`
	// Progress, when non-nil, makes this a NON-TERMINAL progress notification for
	// the in-flight op ID rather than its result: the main app forwards it to the
	// UI and keeps waiting for the real reply. It is absent (nil) on every terminal
	// reply, so a main app that does not know the field simply never sees one — the
	// field is backward-compatible in both directions.
	Progress *Progress `json:"progress,omitempty"`
}

// EncodeRequest renders req to its JSON wire string. The Request shape is always
// JSON-safe, so marshalling never fails.
func EncodeRequest(req Request) string {
	b, _ := json.Marshal(req)
	return string(b)
}

// DecodeRequest parses a request wire string, returning an error only on
// malformed JSON.
func DecodeRequest(s string) (Request, error) {
	var req Request
	err := json.Unmarshal([]byte(s), &req)
	return req, err
}

// EncodeReply renders reply to its JSON wire string. The Reply shape is always
// JSON-safe, so marshalling never fails.
func EncodeReply(reply Reply) string {
	b, _ := json.Marshal(reply)
	return string(b)
}

// DecodeReply parses a reply wire string, returning an error only on malformed
// JSON.
func DecodeReply(s string) (Reply, error) {
	var reply Reply
	err := json.Unmarshal([]byte(s), &reply)
	return reply, err
}

// ReadyReply is the boot message the worker posts once it is able to serve
// requests. It carries no ID (the main app matches it by the Ready flag).
func ReadyReply() Reply { return Reply{OK: true, Ready: true} }

// OKReply builds a successful reply for id.
func OKReply(id int) Reply { return Reply{ID: id, OK: true} }

// ErrorReply builds a failed reply for id with a stable code and detail.
func ErrorReply(id int, code, detail string) Reply {
	return Reply{ID: id, OK: false, Code: code, Error: detail}
}

// ProgressReply builds a NON-TERMINAL progress notification for the in-flight op
// id. It is posted before the terminal reply; the main app routes it to the op's
// progress callback by ID and keeps waiting for the result. OK is left false —
// the Progress field, not OK, is what marks it non-terminal.
func ProgressReply(id int, p Progress) Reply {
	return Reply{ID: id, Progress: &p}
}
