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

import "encoding/json"

// Op names. Each is one remote-git operation the worker can perform on the
// long-lived session it holds across calls.
const (
	OpClone     = "clone"     // clone a remote into the worker's memory
	OpList      = "list"      // list the working-tree file paths
	OpReadFile  = "readFile"  // read one working-tree file
	OpWriteFile = "writeFile" // write one working-tree file (no commit)
	OpStatus    = "status"    // branch + ahead/behind + dirty snapshot
	OpCommit    = "commit"    // write a file then commit it
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

// Status is the branch/divergence snapshot a mutating op returns so the main app
// never needs a separate round-trip to render the panel's status line.
type Status struct {
	Branch    string `json:"branch"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	Clean     bool   `json:"clean"`
	DirtyFile int    `json:"dirtyFile"`
}

// Commit is one log line: the abbreviated hash, subject and author.
type Commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
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
	Status   *Status           `json:"status,omitempty"`
	Log      []Commit          `json:"log,omitempty"`
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
