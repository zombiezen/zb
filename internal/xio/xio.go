// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

// Package xio provides I/O utilities.
package xio

import (
	"io"
	"sync"
)

// A WriteCounter counts the number of bytes written to it.
type WriteCounter int64

// Write increments wc by len(p).
func (wc *WriteCounter) Write(p []byte) (n int, err error) {
	*wc += WriteCounter(len(p))
	return len(p), nil
}

// WriteString increments wc by len(s).
func (wc *WriteCounter) WriteString(s string) (n int, err error) {
	*wc += WriteCounter(len(s))
	return len(s), nil
}

// ReadFuncCloser is an [io.ReadCloser] that calls a custom close function.
type ReadFuncCloser struct {
	io.Reader
	CloseFunc func() error
}

// Close calls rfc.CloseFunc.
func (rfc ReadFuncCloser) Close() error {
	if rfc.CloseFunc == nil {
		return nil
	}
	return rfc.CloseFunc()
}

type onceCloser struct {
	f func() error
}

// CloseOnce returns an [io.Closer] that calls c at most once.
func CloseOnce(c io.Closer) io.Closer {
	return &onceCloser{f: sync.OnceValue(c.Close)}
}

func (oc *onceCloser) Close() error {
	return oc.f()
}

var zeroBytes [1024]byte

// WriteZero writes n zero bytes to w.
func WriteZero(w io.Writer, n int64) (int64, error) {
	var written int64
	for written < n {
		nn, err := w.Write(zeroBytes[:min(int64(len(zeroBytes)), n-written)])
		written += int64(nn)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
