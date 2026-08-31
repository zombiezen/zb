// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"slices"

	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
)

var _ interface {
	Store
	BatchStore
	Importer
	RandomAccessStore
	RealizationFetcher
	Exporter
} = Null{}

// Null is an implementation of [Store] and [RealizationFetcher]
// that contains no objects or realizations.
type Null struct{}

// Object implements [Store].
func (Null) Object(ctx context.Context, path Path) (Object, error) {
	return nil, fmt.Errorf("fetch %s: %w", path, ErrNotFound)
}

// ObjectBatch implements [BatchStore].
func (Null) ObjectBatch(ctx context.Context, storePaths sets.Set[Path]) ([]Object, error) {
	return nil, nil
}

// WriteObject implements [ObjectWriter] by doing nothing and returning nil.
func (Null) WriteObject(ctx context.Context, object Object) error {
	return nil
}

// StoreImport implements [Importer] by reading an export from r
// and returning the first non-[io.EOF] error encountered.
func (Null) StoreImport(ctx context.Context, r io.Reader) error {
	return (*BufferedImporter)(nil).StoreImport(ctx, r)
}

// FetchRealizations implements [RealizationFetcher].
func (Null) FetchRealizations(ctx context.Context, derivationHash nix.Hash) (RealizationMap, error) {
	return RealizationMap{}, nil
}

// WriteRealizations does nothing and returns nil.
func (Null) WriteRealizations(ctx context.Context, realizations RealizationMap) error {
	return nil
}

// StoreFS implements [RandomAccessStore].
func (Null) StoreFS(ctx context.Context, dir Directory) fs.FS {
	return nullFS{}
}

// StoreExport implements [Exporter].
func (Null) StoreExport(ctx context.Context, dst io.Writer, paths sets.Set[Path], opts *ExportOptions) error {
	if paths.Len() == 0 {
		if _, err := io.WriteString(dst, exportEOFMarker); err != nil {
			return newCopyError(nil, err)
		}
		return nil
	}
	return newCopyError(slices.Sorted(paths.All()), ErrNotFound)
}

// nullFS is an [fs.FS] implementation that always returns an [fs.ErrNotExist] error.
type nullFS struct{}

// Open implements [fs.FS].
func (nullFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{
		Op:   "open",
		Path: name,
		Err:  fs.ErrNotExist,
	}
}
