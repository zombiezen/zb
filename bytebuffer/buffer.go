// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

// Package bytebuffer provides a buffer type that implements [io.ReadWriteSeeker].
// It also provides an interface for creating byte buffers.
package bytebuffer

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Buffer implements the [io.Reader], [io.WriterTo], [io.Writer], [io.Seeker],
// and [io.ByteScanner] interfaces by reading from or writing to a byte slice.
// The zero value for Buffer operates like a Buffer of an empty slice.
type Buffer struct {
	s     []byte
	i     int64
	limit int
}

// New returns a new [Buffer] reading from and writing to b.
func New(p []byte) *Buffer {
	buf := new(Buffer)
	buf.Reset(p)
	return buf
}

// Reset resets the [Buffer] to be reading from and writing to b.
func (b *Buffer) Reset(p []byte) {
	*b = Buffer{s: p, limit: math.MaxInt}
}

// Size returns the length of the underlying byte slice.
func (b *Buffer) Size() int64 {
	return int64(len(b.s))
}

// Read implements the [io.Reader] interface.
func (b *Buffer) Read(p []byte) (n int, err error) {
	if b.i >= int64(len(b.s)) {
		return 0, io.EOF
	}
	n = copy(p, b.s[b.i:])
	b.i += int64(n)
	return n, nil
}

// ReadByte implements the [io.ByteReader] interface.
func (b *Buffer) ReadByte() (byte, error) {
	if b.i >= int64(len(b.s)) {
		return 0, io.EOF
	}
	bb := b.s[b.i]
	b.i++
	return bb, nil
}

// UnreadByte complements [*Buffer.ReadByte] in implementing the [io.ByteScanner] interface.
func (b *Buffer) UnreadByte() error {
	if b.i <= 0 {
		return errors.New("bytebuffer.Buffer.UnreadByte: at beginning of slice")
	}
	b.i--
	return nil
}

// Write implements the [io.Writer] interface.
// If Write would extend past the underlying byte slice's capacity,
// then Write allocates a new byte slice large enough to fit the new bytes.
// Write returns an error if and only if the byte slice length would exceed an int.
// If the offset is larger than the length of the underlying byte slice,
// then the intervening bytes are zero-filled.
func (b *Buffer) Write(p []byte) (n int, err error) {
	if b.limit > 0 && b.i > int64(b.limit-len(p)) {
		err = errTooLarge
		if b.i >= int64(b.limit) {
			return 0, err
		}
		p = p[:b.limit-int(b.i)]
	}

	switch {
	case b.i > int64(len(b.s)):
		b.s = append(append(b.s, make([]byte, int(b.i)-len(b.s))...), p...)
	case b.i+int64(len(p)) >= int64(len(b.s)):
		b.s = append(b.s[:b.i], p...)
	default:
		copy(b.s[b.i:], p)
	}
	b.i += int64(len(p))
	return len(p), err
}

// Seek implements the [io.Seeker] interface.
func (b *Buffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.i + offset
	case io.SeekEnd:
		abs = int64(len(b.s)) + offset
	default:
		return 0, errors.New("bytebuffer.Buffer.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("bytebuffer.Buffer.Seek: negative position")
	}
	b.i = abs
	return abs, nil
}

// WriteTo implements the [io.WriterTo] interface.
func (b *Buffer) WriteTo(w io.Writer) (n int64, err error) {
	if b.i >= int64(len(b.s)) {
		return 0, nil
	}
	p := b.s[b.i:]
	m, err := w.Write(p)
	if m > len(p) {
		panic("bytebuffer.Buffer.WriteTo: invalid Write count")
	}
	b.i += int64(m)
	n = int64(m)
	if m != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return
}

// Bytes returns a slice of the buffer's content.
// The slice is valid for use only until the next buffer modification
// (that is, only until the next call to a method like [*Buffer.Write], [*Buffer.Reset], or [*Buffer.Truncate]).
// The slice aliases the buffer content at least until the next buffer modification,
// so immediate changes to the slice will affect the result of future reads.
func (b *Buffer) Bytes() []byte {
	return b.s
}

// Truncate changes the size of the buffer.
// It does not change the I/O offset.
func (b *Buffer) Truncate(size int64) error {
	switch {
	case b.limit > 0 && size > int64(b.limit):
		return errTooLarge
	case size < 0:
		return fmt.Errorf("bytebuffer.Buffer.Truncate: negative size")
	case int(size) < len(b.s):
		b.s = b.s[:size]
	case int(size) > len(b.s):
		newSlice := make([]byte, size)
		copy(newSlice, b.s)
		b.s = newSlice
	}
	return nil
}

const defaultLimit = 1024 * 1024 * 1024 // 1 GiB

// BufferCreator is a [Creator] that returns buffers backed by memory.
type BufferCreator struct {
	// Limit specifies an maximum limit on size of buffers.
	// If Limit is zero, then a reasonable default limit is used.
	// If Limit is negative, then no limit is applied.
	Limit int
}

// CreateBuffer returns an in-memory buffer of the given size.
func (c BufferCreator) CreateBuffer(size int64) (ReadWriteSeekCloser, error) {
	limit := c.Limit
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0:
		limit = math.MaxInt
	}
	if limit > 0 && size > int64(limit) {
		return nil, fmt.Errorf("create buffer: %d bytes exceeds limit", size)
	}
	b := New(make([]byte, max(size, 0)))
	b.limit = limit
	return closeBuffer{b}, nil
}

type closeBuffer struct {
	*Buffer
}

func (cb closeBuffer) Close() error {
	return nil
}

var errTooLarge = errors.New("in-memory buffer too large")
