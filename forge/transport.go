// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

package forge

import (
	"context"
	"errors"
	"strings"
)

// Transport sends one request and returns the whole response. forge does not
// use net/http directly: in js/wasm, net/http drags crypto/tls and x509 into
// the module (about 8 MB) only to hand the request to fetch() in the end. The
// default is net/http natively (transport_other.go) and the Fetch API in the
// browser (transport_js.go); both refuse redirects and bound the body.
type Transport interface {
	RoundTrip(ctx context.Context, req *Request) (*Response, error)
}

// DefaultTransport is net/http natively and the Fetch API in the browser.
func DefaultTransport() Transport { return defaultTransport() }

// Request is one API call as the transport sees it.
type Request struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
	// Limit is the most body bytes to read; a longer body is ErrBodyTooLarge.
	Limit int64
	// SameOrigin sends the page's own cookies and nothing else: the browser's
	// Fetch is called with mode "same-origin" (a URL on another origin is
	// refused by the browser itself), credentials "same-origin" and referrer
	// policy "same-origin" (with "no-referrer" the browser would send
	// "Origin: null" on a POST). The default, false, is credentials "omit". Only
	// package gitd sets it; natively it changes nothing (the Client's cookie jar
	// decides).
	SameOrigin bool
}

// Response is the status, the headers (names in lower case) and the body.
type Response struct {
	Status int
	Header map[string]string
	Body   []byte
}

// Get returns a response header by name, case-insensitively.
func (r *Response) Get(name string) string { return r.Header[strings.ToLower(name)] }

// ErrBodyTooLarge is a body over Request.Limit.
var ErrBodyTooLarge = errors.New("forge: response body over the limit")

// statusText is the few reason phrases an error message may fall back on.
func statusText(code int) string {
	switch code {
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 413:
		return "Content Too Large"
	case 422:
		return "Unprocessable Content"
	case 429:
		return "Too Many Requests"
	}
	if code >= 300 && code < 400 {
		return "Redirect"
	}
	if code >= 500 {
		return "Server Error"
	}
	return "HTTP error"
}
