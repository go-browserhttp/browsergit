// Copyright (c) the go-tex authors.
// SPDX-License-Identifier: BSD-3-Clause

package gitrpc

import (
	"reflect"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	req := Request{ID: 7, Op: OpClone, Args: Args{URL: "https://forge/o/r.git", Branch: "main", Token: "tok", Author: "Ada", Email: "ada@x", Limit: 5}}
	got, err := DecodeRequest(EncodeRequest(req))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, req)
	}
}

func TestReplyRoundTrip(t *testing.T) {
	reply := Reply{
		ID:       9,
		OK:       true,
		Files:    []string{"main.tex", "a/b.tex"},
		Contents: map[string]string{"main.tex": "body"},
		Content:  "one file",
		HasRepo:  true,
		Status:   &Status{Branch: "main", Ahead: 2, Behind: 1, Clean: false, DirtyFile: 3},
		Log:      []Commit{{Hash: "abc1234", Subject: "seed", Author: "Ada"}},
	}
	got, err := DecodeReply(EncodeReply(reply))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, reply) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, reply)
	}
}

func TestDecodeRequestError(t *testing.T) {
	if _, err := DecodeRequest("{not json"); err == nil {
		t.Fatal("expected a decode error on malformed request JSON")
	}
}

func TestDecodeReplyError(t *testing.T) {
	if _, err := DecodeReply("nope"); err == nil {
		t.Fatal("expected a decode error on malformed reply JSON")
	}
}

func TestReplyConstructors(t *testing.T) {
	if r := ReadyReply(); !r.OK || !r.Ready {
		t.Fatalf("ReadyReply = %+v", r)
	}
	if r := OKReply(3); r.ID != 3 || !r.OK {
		t.Fatalf("OKReply = %+v", r)
	}
	r := ErrorReply(4, CodeAuth, "denied")
	if r.ID != 4 || r.OK || r.Code != CodeAuth || r.Error != "denied" {
		t.Fatalf("ErrorReply = %+v", r)
	}
}
