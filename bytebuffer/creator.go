// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package bytebuffer

import (
	"io"
	"os"
)

// ReadWriteSeekCloser is an interface that groups the Read, Write, Seek, and Close methods.
type ReadWriteSeekCloser interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
}

// A Creator can create temporary byte buffers.
// The [ReadWriteSeekCloser] returned from CreateBuffer
// must be of the given size and start with its offset at 0.
// If the size passed to CreateBuffer is less than 1,
// it indicates that the caller does not know how many bytes will be written to it.
// A returned [ReadWriteSeekCloser] values might not permit writes past the ends of its returned buffers.
// If this is the case, passing a size less than 1 to CreateBuffer
// should return an error.
type Creator interface {
	CreateBuffer(size int64) (ReadWriteSeekCloser, error)
}

// CreateFunc is a function that implements [Creator].
type CreateFunc func(size int64) (ReadWriteSeekCloser, error)

// CreateBuffer implements [Creator] by calling f.
func (f CreateFunc) CreateBuffer(size int64) (ReadWriteSeekCloser, error) {
	return f(size)
}

// TempFileCreator implements [Creator] with [os.CreateTemp].
// The fields of TempFileCreator are given as arguments to [os.CreateTemp].
type TempFileCreator struct {
	Dir     string
	Pattern string
}

// CreateBuffer creates a new temporary file of the given size.
// The returned file will be removed when calling Close.
func (tfc TempFileCreator) CreateBuffer(size int64) (ReadWriteSeekCloser, error) {
	f, err := os.CreateTemp(tfc.Dir, tfc.Pattern)
	if err != nil {
		return nil, err
	}
	if size >= 1 {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, err
		}
	}
	return removeOnCloseFile{f}, nil
}

type removeOnCloseFile struct {
	*os.File
}

func (f removeOnCloseFile) Close() error {
	closeError := f.File.Close()
	removeError := os.Remove(f.Name())
	if closeError != nil {
		return closeError
	}
	if removeError != nil {
		return removeError
	}
	return nil
}
