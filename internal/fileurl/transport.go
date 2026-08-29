// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package fileurl

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/httpencoding"
	"zb.256lights.llc/pkg/internal/xhttp"
)

// Transport is an [http.RoundTripper] that serves GET, HEAD, and PUT requests
// to the local filesystem for "file://" URLs.
type Transport struct {
}

// RoundTrip implements [http.RoundTripper].
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	if req.URL.Scheme != Scheme {
		return nil, http.ErrSkipAltProtocol
	}

	switch req.Method {
	case "", http.MethodGet, http.MethodHead:
		return t.get(req), nil
	case http.MethodPut:
		return t.put(req), nil
	default:
		resp := errorResponse(req, "", http.StatusMethodNotAllowed)
		resp.Header["Allow"] = []string{"GET, HEAD, PUT"}
		return resp, nil
	}
}

func (t Transport) get(req *http.Request) *http.Response {
	path, err := ToPath(req.URL)
	if err != nil {
		return errorResponse(req, err.Error(), http.StatusNotFound)
	}
	return xhttp.ServeResponse(req, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, req)
			} else {
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if info.IsDir() {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		http.ServeContent(w, req, filepath.Base(path), info.ModTime(), f)
	}))
}

func (t Transport) put(req *http.Request) (resp *http.Response) {
	path, err := ToPath(req.URL)
	if err != nil {
		return errorResponse(req, err.Error(), http.StatusNotFound)
	}

	flag := os.O_WRONLY
	if ifMatch := req.Header.Values("If-Match"); len(ifMatch) == 0 {
		flag |= os.O_CREATE
	} else if len(ifMatch) != 1 || ifMatch[0] != "*" {
		return errorResponse(req, "If-Match not supported", http.StatusPreconditionFailed)
	} else if len(req.Header.Values("If-None-Match")) > 0 {
		return errorResponse(req, "If-Match and If-None-Match cannot be combined", http.StatusPreconditionFailed)
	}
	if req.Header.Get("If-None-Match") == "*" {
		flag |= os.O_EXCL
	}

	body, err := httpencoding.Decode(req.Body, req.Header.Get("Content-Encoding"))
	if err != nil {
		var code int
		if httpencoding.IsUnsupported(err) {
			code = http.StatusUnsupportedMediaType
		} else {
			code = http.StatusBadRequest
		}
		resp := errorResponse(req, err.Error(), code)
		resp.Header.Set("Accept-Encoding", httpencoding.Accept)
		return resp
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return errorResponse(req, "Could not create parent directory", http.StatusInternalServerError)
	}
	f, err := os.OpenFile(path, flag, 0o666)
	if err != nil {
		if flag&os.O_EXCL != 0 && errors.Is(err, os.ErrExist) {
			return errorResponse(req, "Already exists", http.StatusPreconditionFailed)
		}
		if flag&os.O_CREATE == 0 && errors.Is(err, os.ErrNotExist) {
			return errorResponse(req, "Does not exist", http.StatusPreconditionFailed)
		}
		if isDirectoryError(err) {
			resp := errorResponse(req, "Is a directory", http.StatusMethodNotAllowed)
			resp.Header.Set("Allow", http.MethodGet+","+http.MethodHead)
			return resp
		}
		return errorResponse(req, "Could not open file", http.StatusInternalServerError)
	}
	defer func() {
		f.Close()
		if flag&(os.O_CREATE|os.O_EXCL) == (os.O_CREATE|os.O_EXCL) && resp.StatusCode != http.StatusCreated {
			// If the request failed and we know that we created this file
			// during this request, then remove it.
			os.Remove(path)
		}
	}()

	if ifUnmodifiedSince := req.Header.Get("If-Unmodified-Since"); ifUnmodifiedSince != "" {
		if t, err := http.ParseTime(ifUnmodifiedSince); err == nil {
			info, err := f.Stat()
			if err != nil {
				return errorResponse(req, "Stat failed", http.StatusInternalServerError)
			}
			if info.ModTime().Truncate(time.Second).After(t.Truncate(time.Second)) {
				return errorResponse(req, "Modified since "+ifUnmodifiedSince, http.StatusPreconditionFailed)
			}
		}
	}
	if err := f.Truncate(0); err != nil {
		return errorResponse(req, "Copy failed", http.StatusInternalServerError)
	}
	if _, err := io.Copy(f, body); err != nil {
		return errorResponse(req, "Copy failed", http.StatusInternalServerError)
	}

	var modTime time.Time
	if info, err := f.Stat(); err != nil {
		modTime = info.ModTime()
	}
	if err := f.Close(); err != nil {
		return errorResponse(req, "Copy failed", http.StatusInternalServerError)
	}

	code := http.StatusNoContent
	if flag&(os.O_CREATE|os.O_EXCL) == (os.O_CREATE | os.O_EXCL) {
		code = http.StatusCreated
	}
	responseHeader := http.Header{
		"Date": {time.Now().UTC().Format(http.TimeFormat)},
	}
	if !modTime.IsZero() {
		responseHeader.Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	}
	return &http.Response{
		Request:    req,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		StatusCode: code,
		Status:     xhttp.Status(code),
		Header:     responseHeader,
		Body:       bytebuffer.Null{},
	}
}

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
		Status:        xhttp.Status(code),
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
