// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

// Package storetest provides utilities for interacting with the zb store in tests.
package storetest

import (
	stdcmp "cmp"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-cmp/cmp"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

// BlobSlice implements [zbstore.Store] and [zbstore.Importer]
// for a slice of [*zbstore.Blob].
// The zero value is an empty store.
type BlobSlice []*zbstore.Blob

// Object implements [zbstore.Store].
func (slice BlobSlice) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	// Iterate in forward order because WriteObject is supposed to no-op after a successful write.
	for _, blob := range slice {
		if blob.StorePath == path {
			return blob, nil
		}
	}
	return nil, fmt.Errorf("open %s: %w", path, zbstore.ErrNotFound)
}

// WriteObject copies the object to a [*zbstore.Blob]
// and appends it to the slice.
func (slice *BlobSlice) WriteObject(ctx context.Context, object zbstore.Object) error {
	w := blobWriter{adder: slice}
	buf, err := w.CreateBuffer(-1)
	if err != nil {
		return err
	}
	if err := object.WriteNAR(ctx, buf); err != nil {
		return err
	}
	return w.WriteObject(ctx, object)
}

func (slice *BlobSlice) addBlob(blob *zbstore.Blob) {
	*slice = append(*slice, blob)
}

// StoreImport implements [zbstore.Importer]
// by appending the objects unmarshaled from r into the slice.
func (slice *BlobSlice) StoreImport(ctx context.Context, r io.Reader) error {
	return addBlobs(ctx, slice, r)
}

type blobAdder interface {
	addBlob(*zbstore.Blob)
}

func addBlobs(ctx context.Context, adder blobAdder, r io.Reader) error {
	w := &blobWriter{adder: adder}
	return (&zbstore.BufferedImporter{
		ObjectWriter:  w,
		BufferCreator: w,
	}).StoreImport(ctx, r)
}

type blobWriter struct {
	adder  blobAdder
	buffer *singleUseBuffer
}

func (w *blobWriter) CreateBuffer(size int64) (bytebuffer.ReadWriteSeekCloser, error) {
	w.buffer = new(singleUseBuffer)
	return w.buffer, nil
}

// WriteObject appends a new object to the slice,
// using the byte slice from the buffer in the last call to [*blobSliceWriter.CreateBuffer] for the NAR
// instead of calling object.WriteNAR.
func (w *blobWriter) WriteObject(ctx context.Context, object zbstore.Object) error {
	if w.buffer == nil {
		return errors.New("store import did not create a buffer")
	}
	info := object.Info()
	w.adder.addBlob(&zbstore.Blob{
		NAR:           w.buffer.Bytes(),
		NARHash:       info.NARHash,
		ExportTrailer: *info.ExportTrailer(),
	})

	w.buffer.done = true
	w.buffer = nil
	return nil
}

type singleUseBuffer struct {
	bytebuffer.Buffer
	done bool
}

func (bb *singleUseBuffer) check() error {
	if bb.done {
		return errors.New("buffer in use")
	}
	return nil
}

func (bb *singleUseBuffer) Read(p []byte) (int, error) {
	if err := bb.check(); err != nil {
		return 0, err
	}
	return bb.Buffer.Read(p)
}

func (bb *singleUseBuffer) Write(p []byte) (int, error) {
	if err := bb.check(); err != nil {
		return 0, err
	}
	return bb.Buffer.Write(p)
}

func (bb *singleUseBuffer) Seek(offset int64, whence int) (int64, error) {
	if err := bb.check(); err != nil {
		return 0, err
	}
	return bb.Buffer.Seek(offset, whence)
}

func (bb *singleUseBuffer) Close() error {
	return nil
}

// TransformSortedSet returns a [cmp.Option]
// that converts sorted sets into slices.
func TransformSortedSet[E stdcmp.Ordered]() cmp.Option {
	return cmp.Transformer("TransformSortedSet", func(s sets.Sorted[E]) []E {
		list := make([]E, s.Len())
		for i := range list {
			list[i] = s.At(i)
		}
		return list
	})
}
