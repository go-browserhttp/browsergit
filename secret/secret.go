// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package secret holds a forge credential so that it can be used in exactly one
// place, an Authorization header, and nowhere else.
//
// A [Token] prints as "[redacted]" under every fmt verb, refuses to be written
// by encoding/json, encoding (text), encoding/gob and encoding (binary), and
// does not hold its value at all: it holds a handle into a package registry, so
// a struct that carries one in an unexported field prints a number, never the
// value. The only way to read it is [Token.Reveal], which a reviewer can grep
// for; [Token.Forget] drops it at sign-out.
//
// This is a seatbelt, not a vault: a token lives in the process memory of the
// tab or server that uses it. What it removes is the accidental paths, the
// log line, the error message, the saved draft, the share link, the export.
package secret

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// Redacted is what a Token prints as.
const Redacted = "[redacted]"

// ErrNotSerializable is returned by every encoder hook of a Token. A token
// that reaches a serializer is a bug in the caller: failing loudly is the
// point, a silent placeholder would hide where it leaked from.
var ErrNotSerializable = errors.New("secret: a token cannot be serialized")

// The values live in a package registry keyed by a number. A Token holds only a
// pointer to its number, so nothing reflection can walk from a Token, fmt's
// fallback paths included (they print an unexported field's pointee without
// calling its methods), ever reaches the value.
var (
	registry sync.Map // uint64 -> string
	nextID   atomic.Uint64
)

type handle struct{ id uint64 }

// Token is a forge credential. The zero value is the absent token. Copies share
// the value; [Token.Forget] on any copy forgets it for all.
type Token struct{ h *handle }

// New wraps s, trimming surrounding white space. An empty s gives the zero Token.
func New(s string) Token {
	s = strings.TrimSpace(s)
	if s == "" {
		return Token{}
	}
	h := &handle{id: nextID.Add(1)}
	registry.Store(h.id, s)
	// When the last copy is unreachable, the value goes with it.
	runtime.AddCleanup(h, func(id uint64) { registry.Delete(id) }, h.id)
	return Token{h: h}
}

// IsZero reports whether no token is held (or it was forgotten).
func (t Token) IsZero() bool { return t.Reveal() == "" }

// Reveal returns the credential. Call it only where the value goes on the wire.
func (t Token) Reveal() string {
	if t.h == nil {
		return ""
	}
	v, _ := registry.Load(t.h.id)
	s, _ := v.(string)
	return s
}

// Forget drops the value now, for every copy: sign-out calls it so that a
// token is gone from memory, not merely unreferenced.
func (t Token) Forget() {
	if t.h != nil {
		registry.Delete(t.h.id)
	}
}

// String implements fmt.Stringer.
func (t Token) String() string { return Redacted }

// GoString implements fmt.GoStringer.
func (t Token) GoString() string { return "secret.Token(" + Redacted + ")" }

// Format implements fmt.Formatter, so %x, %q, %d and friends cannot reach the value.
func (t Token) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// MarshalJSON refuses.
func (Token) MarshalJSON() ([]byte, error) { return nil, ErrNotSerializable }

// MarshalText refuses.
func (Token) MarshalText() ([]byte, error) { return nil, ErrNotSerializable }

// MarshalBinary refuses.
func (Token) MarshalBinary() ([]byte, error) { return nil, ErrNotSerializable }

// GobEncode refuses.
func (Token) GobEncode() ([]byte, error) { return nil, ErrNotSerializable }

// basicUsers are the user names forges pair with a token in HTTP Basic auth;
// an echoed Authorization header carries base64("<user>:<token>").
var basicUsers = []string{"git", "oauth2", "x-access-token", "token", ""}

// Redact replaces every occurrence of each token in s, raw or inside a Basic
// authorization value, with [Redacted]. Use it on any text that came back from
// a server before it reaches an error, a log or the screen: a misbehaving
// server or proxy can echo the request headers.
func Redact(s string, tokens ...Token) string {
	for _, t := range tokens {
		v := t.Reveal()
		if len(v) < 4 {
			// A token this short is not a real credential, and replacing every
			// occurrence of one or two characters would shred the message.
			continue
		}
		s = strings.ReplaceAll(s, v, Redacted)
		for _, u := range basicUsers {
			pair := v
			if u != "" {
				pair = u + ":" + v
			}
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				s = strings.ReplaceAll(s, enc.EncodeToString([]byte(pair)), Redacted)
			}
		}
	}
	return s
}
