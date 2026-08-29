// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package bytebuffer

import (
	"fmt"
	"io"
)

var _ interface {
	io.Reader
	io.ByteReader
	io.WriterTo

	io.Writer
	io.ByteWriter
	io.StringWriter
	io.ReaderFrom

	io.Seeker

	io.Closer
} = Null{}

// Null implements [io.Reader], [io.Writer], [io.Seeker], and [io.Closer] as no-ops.
type Null struct{}

// Read returns (0, [io.EOF]).
func (Null) Read(p []byte) (int, error) {
	return 0, io.EOF
}

// ReadByte returns (0, [io.EOF]).
func (Null) ReadByte() (byte, error) {
	return 0, io.EOF
}

// WriteTo does nothing and returns (0, nil).
func (Null) WriteTo(w io.Writer) (int64, error) {
	return 0, nil
}

// Write returns (len(p), nil).
func (Null) Write(p []byte) (int, error) {
	return len(p), nil
}

// WriteByte returns nil.
func (Null) WriteByte(b byte) error {
	return nil
}

// WriteString returns (len(s), nil).
func (Null) WriteString(s string) (int, error) {
	return len(s), nil
}

// ReadFrom reads bytes from r until [io.EOF] or error
// and returns the number of bytes read.
// Any error except [io.EOF] encountered during the read is also returned.
func (Null) ReadFrom(r io.Reader) (int64, error) {
	// [io.Discard] implements [io.ReaderFrom].
	return io.Copy(io.Discard, r)
}

// Seek implements [io.Seeker].
// If the offset is not zero, then Seek will return an error.
func (Null) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart && whence != io.SeekCurrent && whence != io.SeekEnd {
		return 0, fmt.Errorf("null seek: invalid whence = %d", whence)
	}
	if offset != 0 {
		return 0, fmt.Errorf("null seek: offset %d != 0", offset)
	}
	return 0, nil
}

// Close returns nil.
func (Null) Close() error {
	return nil
}
