// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

/*
Package althttp provides access to non-HTTP protocols such as "file://" URLs
and common cloud service storage providers
in the form of [http.RoundTripper] implementations that present an HTTP GET/PUT API.
*/
package althttp

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func errorResponse(req *http.Request, error string, code int) *http.Response {
	if error != "" {
		error += "\n"
	}
	return &http.Response{
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		StatusCode:    code,
		Status:        http.StatusText(code),
		ContentLength: int64(len(error)),
		Header: http.Header{
			"Content-Type":           {"text/plain; charset=utf-8"},
			"X-Content-Type-Options": {"nosniff"},
			"Content-Length":         {strconv.Itoa(len(error))},
			"Date":                   {time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: io.NopCloser(strings.NewReader(error)),
	}
}

// headerFieldCombiner is the string recommended by [Section 5.3 of RFC 9110]
// to be used to join multiple values of the same HTTP header field.
//
// [Section 5.3 of RFC 9110]: https://www.rfc-editor.org/rfc/rfc9110.html#section-5.3
const headerFieldCombiner = ", "
