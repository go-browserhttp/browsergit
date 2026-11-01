// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package forge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPTransport is the native Transport, over an http.Client. Redirects are
// never followed: a redirect would carry the request, and with it the
// Authorization header, somewhere we did not choose.
type HTTPTransport struct {
	// Client is copied, never modified; nil means a 60 s timeout.
	Client *http.Client
}

func defaultTransport() Transport { return HTTPTransport{} }

// RoundTrip implements Transport.
func (t HTTPTransport) RoundTrip(ctx context.Context, r *Request) (*Response, error) {
	hc := http.Client{Timeout: 60 * time.Second}
	if t.Client != nil {
		hc = *t.Client
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}
	rsp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(rsp.Body, r.Limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > r.Limit {
		return nil, ErrBodyTooLarge
	}
	out := &Response{Status: rsp.StatusCode, Header: map[string]string{}, Body: data}
	for k := range rsp.Header {
		out.Header[strings.ToLower(k)] = rsp.Header.Get(k)
	}
	return out, nil
}
