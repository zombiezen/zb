// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package xhttp

import (
	"io"
	"net/http"
	"strconv"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/xio"
)

// ServeResponse calls h.ServeHTTP and converts the result into an [*http.Response].
func ServeResponse(req *http.Request, h http.Handler) *http.Response {
	headerWritten := make(chan int)
	var rbody io.ReadCloser
	var wbody io.WriteCloser
	serveDone := make(chan struct{})
	if req.Method == http.MethodHead {
		rbody = xio.ReadFuncCloser{
			Reader: bytebuffer.Null{},
			CloseFunc: func() error {
				<-serveDone
				return nil
			},
		}
		wbody = discardCloser{}
	} else {
		var pr *io.PipeReader
		pr, wbody = io.Pipe()
		rbody = xio.ReadFuncCloser{
			Reader: pr,
			CloseFunc: func() error {
				err := pr.Close()
				<-serveDone
				return err
			},
		}
	}
	header := make(http.Header)
	go func() {
		defer func() {
			wbody.Close()
			close(serveDone)
		}()
		w := &pipedResponseWriter{
			headerWritten: headerWritten,
			header:        header,
			body:          wbody,
		}
		h.ServeHTTP(w, req)
	}()

	statusCode := <-headerWritten
	resp := &http.Response{
		Request:    req,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Status:     Status(statusCode),
		StatusCode: statusCode,
		Header:     header,
		Body:       rbody,
	}
	if values := header.Values("Content-Length"); len(values) == 1 {
		if n, err := strconv.ParseUint(values[0], 10, 63); err == nil {
			resp.ContentLength = int64(n)
		} else {
			resp.ContentLength = -1
		}
	} else {
		resp.ContentLength = -1
	}
	return resp
}

type pipedResponseWriter struct {
	body io.Writer

	headerWritten chan<- int
	header        http.Header
}

func (w *pipedResponseWriter) Header() http.Header {
	return w.header
}

func (w *pipedResponseWriter) WriteHeader(statusCode int) {
	if w.headerWritten != nil {
		// Prevent future changes to w.Header() from racing with reads.
		w.header = w.header.Clone()

		w.headerWritten <- statusCode
		close(w.headerWritten)
		w.headerWritten = nil
	}
}

func (w *pipedResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discardCloser) WriteString(s string) (int, error) {
	return len(s), nil
}

func (discardCloser) ReadFrom(r io.Reader) (n int64, err error) {
	return io.Copy(io.Discard, r)
}

func (discardCloser) Close() error {
	return nil
}
