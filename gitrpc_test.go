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

// TestParseProgress covers the sideband-line parser: the phase is the text before
// the colon, an exact "(cur/total)" gives the truest fraction, a bare "NN%" is the
// fallback, and a line with neither reports -1 ("cannot tell") rather than 0%.
func TestParseProgress(t *testing.T) {
	for _, tc := range []struct {
		line      string
		wantPhase string
		wantFrac  float64
	}{
		{"Receiving objects:  45% (450/1000), 1.2 MiB | 500 KiB/s", "Receiving objects", 0.45},
		{"Counting objects: 100% (42/42), done.", "Counting objects", 1},
		{"Compressing objects:  50% (10/20)", "Compressing objects", 0.5},
		{"Resolving deltas:  25%", "Resolving deltas", 0.25},
		{"Enumerating objects: 128, done.", "Enumerating objects", -1},
		{"remote: Total 5 (delta 0)", "remote", -1},
		{"", "", -1},
		{"  (7/0) zero total", "", -1}, // a zero total is unusable, not a divide
	} {
		got := ParseProgress(tc.line)
		if got.Phase != tc.wantPhase {
			t.Errorf("ParseProgress(%q).Phase = %q, want %q", tc.line, got.Phase, tc.wantPhase)
		}
		if got.Fraction != tc.wantFrac {
			t.Errorf("ParseProgress(%q).Fraction = %v, want %v", tc.line, got.Fraction, tc.wantFrac)
		}
	}
}

// TestProgressReplyRoundTrip proves a progress notification survives the wire and
// is distinguishable from a terminal reply: its Progress field is non-nil (a
// terminal reply's is nil) while OK stays false.
func TestProgressReplyRoundTrip(t *testing.T) {
	p := ParseProgress("Receiving objects:  90% (900/1000)")
	got, err := DecodeReply(EncodeReply(ProgressReply(12, p)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 12 || got.Progress == nil {
		t.Fatalf("progress reply lost its marker: %+v", got)
	}
	if got.Progress.Fraction != 0.9 || got.Progress.Phase != "Receiving objects" {
		t.Fatalf("progress payload = %+v", *got.Progress)
	}
	// A terminal reply carries no Progress, so the main app never mistakes it for a
	// non-terminal notification.
	if term, _ := DecodeReply(EncodeReply(OKReply(12))); term.Progress != nil {
		t.Fatalf("a terminal reply must have nil Progress, got %+v", term.Progress)
	}
}
