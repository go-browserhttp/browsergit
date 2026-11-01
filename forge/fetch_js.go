// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

package forge

import "net/http"

// setFetchOptions passes Fetch API options through Go's js/wasm transport,
// which reads these pseudo-headers and does not send them (see
// net/http/roundtrip_js.go: jsFetchCreds, jsFetchRedirect):
//
//   - credentials "omit": no cookie of the forge rides along, so an API call
//     cannot act with the person's forge session, only with the token;
//   - redirect "error": the browser does not follow a redirect at all, so the
//     request (and its Authorization header) cannot be steered elsewhere.
func setFetchOptions(req *http.Request) {
	req.Header.Set("js.fetch:credentials", "omit")
	req.Header.Set("js.fetch:redirect", "error")
}
