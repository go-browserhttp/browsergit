// Copyright (c) the go-browserhttp authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package forge

import "net/http"

// setFetchOptions is a no-op natively: the pseudo-headers the js transport
// reads are not valid HTTP header names, and the client's CheckRedirect
// already refuses redirects.
func setFetchOptions(*http.Request) {}
