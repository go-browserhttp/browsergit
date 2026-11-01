// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
)

// Deliberately not shaped like a real token: a realistic secret in a test file
// is one copy-paste away from being treated as a real one.
const planted = "PLANTED-TOKEN-PLACEHOLDER-0123456789"

func TestForget(t *testing.T) {
	tok := New(planted)
	cp := tok
	cp.Forget()
	if !tok.IsZero() || tok.Reveal() != "" {
		t.Fatal("Forget on a copy must forget for every copy")
	}
	Token{}.Forget() // no panic
}

func TestRevealAndZero(t *testing.T) {
	if !New("  ").IsZero() || New("").Reveal() != "" || (Token{}).Reveal() != "" {
		t.Fatal("blank must give the zero token")
	}
	tok := New("  " + planted + "\n")
	if tok.IsZero() || tok.Reveal() != planted {
		t.Fatalf("Reveal = %q", tok.Reveal())
	}
}

// holder carries a token the two ways a caller might: exported and not.
type holder struct {
	Name  string
	Tok   Token
	inner Token
	ptr   *Token
}

func TestNoFmtVerbRevealsTheToken(t *testing.T) {
	tok := New(planted)
	h := holder{Name: "n", Tok: tok, inner: tok, ptr: &tok}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v"}
	for _, v := range verbs {
		for _, arg := range []any{tok, &tok, h, &h, []Token{tok}, map[string]Token{"k": tok}} {
			out := fmt.Sprintf(v, arg)
			if strings.Contains(out, planted) || strings.Contains(out, planted[:12]) {
				t.Errorf("fmt %s of %T leaked: %s", v, arg, out)
			}
		}
	}
	// The planted positive: the check above can see the value when it is there.
	if !strings.Contains(fmt.Sprintf("%v", struct{ S string }{planted}), planted) {
		t.Fatal("control failed: fmt should print a plain string field")
	}
}

func TestLogAndErrorsDoNotRevealTheToken(t *testing.T) {
	tok := New(planted)
	var buf bytes.Buffer
	l := log.New(&buf, "", 0)
	l.Printf("token=%v holder=%+v", tok, holder{Tok: tok, inner: tok})
	err := fmt.Errorf("auth with %v failed: %w", tok, errors.New("x"))
	for _, s := range []string{buf.String(), err.Error()} {
		if strings.Contains(s, planted) {
			t.Fatalf("leaked: %s", s)
		}
	}
}

func TestEncodersRefuse(t *testing.T) {
	tok := New(planted)
	if _, err := json.Marshal(struct{ T Token }{tok}); !errors.Is(err, ErrNotSerializable) {
		t.Fatalf("json: %v", err)
	}
	if _, err := json.Marshal(map[Token]int{tok: 1}); err == nil {
		t.Fatal("json map key must refuse")
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(struct{ T Token }{tok}); err == nil {
		t.Fatal("gob must refuse")
	}
	if _, err := tok.MarshalText(); !errors.Is(err, ErrNotSerializable) {
		t.Fatal("text must refuse")
	}
	if _, err := tok.MarshalBinary(); !errors.Is(err, ErrNotSerializable) {
		t.Fatal("binary must refuse")
	}
	if strings.Contains(buf.String(), planted) {
		t.Fatal("gob wrote the value before failing")
	}
	// A struct that does not carry the token in a serialized field is fine.
	if _, err := json.Marshal(struct {
		T Token `json:"-"`
	}{tok}); err != nil {
		t.Fatalf("json:\"-\" must stay serializable: %v", err)
	}
}

func TestRedact(t *testing.T) {
	tok := New(planted)
	echoed := []string{
		"bad credentials " + planted,
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("git:"+planted)),
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+planted)),
		"Authorization: Basic " + base64.RawURLEncoding.EncodeToString([]byte("x-access-token:"+planted)),
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(planted)),
	}
	for _, s := range echoed {
		got := Redact(s, tok)
		if strings.Contains(got, planted) || got == s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
		if !strings.Contains(got, Redacted) {
			t.Errorf("Redact(%q) lost the marker: %q", s, got)
		}
	}
	if got := Redact("abc a b", New("ab")); got != "abc a b" {
		t.Fatalf("a short token must not shred text: %q", got)
	}
	if got := Redact("plain", Token{}); got != "plain" {
		t.Fatal(got)
	}
}

func FuzzRedact(f *testing.F) {
	f.Add("prefix PLACEHOLDER suffix", "PLACEHOLDER")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, s, tok string) {
		got := Redact(s, New(tok))
		v := New(tok).Reveal()
		if len(v) >= 4 && strings.Contains(got, v) && !strings.Contains(Redacted, v) {
			t.Fatalf("Redact left %q in %q", v, got)
		}
	})
}
