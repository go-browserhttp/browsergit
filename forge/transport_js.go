// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

package forge

import (
	"context"
	"errors"
	"strings"
	"syscall/js"
)

// FetchTransport is the browser's Transport: the Fetch API, called directly.
//
//   - redirect "error": a redirect is never followed, so the request and its
//     Authorization header cannot be steered to another host;
//   - credentials "omit": no cookie of the forge rides along, so a call can
//     act only with the token, never with the person's forge session;
//   - cache "no-store" and referrerPolicy "no-referrer";
//   - the body is read chunk by chunk and abandoned past the limit.
type FetchTransport struct{}

func defaultTransport() Transport { return FetchTransport{} }

// await resolves a promise from a goroutine (never from the event loop's own
// turn: the op runs in a goroutine, see playground GitRun).
func await(p js.Value) (js.Value, error) {
	type res struct {
		v   js.Value
		err error
	}
	ch := make(chan res, 1)
	var ok, ko js.Func
	ok = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- res{v: a[0]}
		return nil
	})
	ko = js.FuncOf(func(_ js.Value, a []js.Value) any {
		msg := "fetch failed"
		if len(a) > 0 && a[0].Truthy() {
			if m := a[0].Get("message"); m.Type() == js.TypeString {
				msg = m.String()
			}
		}
		ch <- res{err: errors.New(msg)}
		return nil
	})
	p.Call("then", ok, ko)
	r := <-ch
	ok.Release()
	ko.Release()
	return r.v, r.err
}

// RoundTrip implements Transport.
func (FetchTransport) RoundTrip(ctx context.Context, r *Request) (out *Response, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, errors.New("fetch: the browser refused the request")
		}
	}()
	g := js.Global()
	headers := g.Get("Object").New()
	for k, v := range r.Header {
		headers.Set(k, v)
	}
	ac := g.Get("AbortController").New()
	opts := map[string]any{
		"method":         r.Method,
		"headers":        headers,
		"redirect":       "error",
		"credentials":    "omit",
		"cache":          "no-store",
		"referrerPolicy": "no-referrer",
		"signal":         ac.Get("signal"),
	}
	if r.Body != nil {
		b := g.Get("Uint8Array").New(len(r.Body))
		js.CopyBytesToJS(b, r.Body)
		opts["body"] = b
	}
	stop := context.AfterFunc(ctx, func() { ac.Call("abort") })
	defer stop()
	rsp, err := await(g.Call("fetch", r.URL, opts))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	out = &Response{Status: rsp.Get("status").Int(), Header: map[string]string{}}
	var each js.Func
	each = js.FuncOf(func(_ js.Value, a []js.Value) any {
		out.Header[strings.ToLower(a[1].String())] = a[0].String()
		return nil
	})
	rsp.Get("headers").Call("forEach", each)
	each.Release()
	body := rsp.Get("body")
	if body.IsNull() || body.IsUndefined() {
		return out, nil
	}
	reader := body.Call("getReader")
	for {
		chunk, err := await(reader.Call("read"))
		if err != nil {
			return nil, err
		}
		if chunk.Get("done").Bool() {
			break
		}
		v := chunk.Get("value")
		n := v.Get("length").Int()
		if int64(len(out.Body)+n) > r.Limit {
			ac.Call("abort")
			return nil, ErrBodyTooLarge
		}
		buf := make([]byte, n)
		js.CopyBytesToGo(buf, v)
		out.Body = append(out.Body, buf...)
	}
	return out, nil
}
