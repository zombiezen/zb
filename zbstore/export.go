// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"slices"

	"zb.256lights.llc/pkg/sets"
)

const (
	exportObjectMarker  = "\x01\x00\x00\x00\x00\x00\x00\x00"
	exportTrailerMarker = "NIXE\x00\x00\x00\x00"
	exportEOFMarker     = "\x00\x00\x00\x00\x00\x00\x00\x00"
)

// An Exporter is a [Store] that can efficiently export objects.
// The [Export] function will use this interface if available.
// StoreExport must ignore paths not present in the store.
//
// StoreExport must be safe to call concurrently from multiple goroutines.
type Exporter interface {
	Store
	StoreExport(ctx context.Context, dst io.Writer, paths sets.Set[Path], opts *ExportOptions) error
}

// ExportOptions is the set of optional parameters to [Exporter.StoreExport].
type ExportOptions struct {
	// If ExcludeReferences is true, then only the paths given will be exported.
	// Otherwise, paths that are referenced by those store objects will also be included
	// and the store objects will be sorted in dependency order.
	ExcludeReferences bool

	// MaxConcurrency specifies the maximum number of objects to look up concurrently
	// if the store does not implement [BatchStore].
	// Values of MaxConcurrency less than 1 are treated as if 1 was given.
	MaxConcurrency int
}

// Export writes the store objects named by paths
// (and the transitive closure of objects they reference by default)
// to dst in `nix-store --export` format.
// If no paths are given, then an empty export will be written to dst
// without using store.
// Export will not return an error if a path does not exist in src.
//
// If store implements [Exporter], then Export will use store.Export to perform the export.
// Otherwise, Export will query the store for all the objects needed
// and serialize them to dst.
func Export(ctx context.Context, store Store, dst io.Writer, paths sets.Set[Path], opts *ExportOptions) error {
	if len(paths) == 0 {
		if _, err := io.WriteString(dst, exportEOFMarker); err != nil {
			return newExportError(nil, err)
		}
		return nil
	}
	if e, ok := store.(Exporter); ok {
		return e.StoreExport(ctx, dst, paths, opts)
	}

	maxConcurrency := 1
	if opts != nil && opts.MaxConcurrency > 1 {
		maxConcurrency = opts.MaxConcurrency
	}
	objects, err := orderedObjectBatch(ctx, store, slices.Values(slices.Sorted(paths.All())), maxConcurrency)
	if err != nil {
		return newExportError(slices.Sorted(paths.All()), err)
	}
	if excludeRefs := opts != nil && opts.ExcludeReferences; !excludeRefs {
		objects, err = expandClosure(ctx, store, objects, maxConcurrency)
		if err != nil {
			return newExportError(slices.Sorted(paths.All()), err)
		}
	}

	w := NewExportWriter(dst)
	for _, obj := range objects {
		if err := w.WriteObject(ctx, obj); err != nil {
			return newExportError(slices.Sorted(paths.All()), err)
		}
	}
	if err := w.Close(); err != nil {
		return newExportError(slices.Sorted(paths.All()), err)
	}

	return nil
}

func expandClosure(ctx context.Context, store Store, objects []Object, maxConcurrency int) ([]Object, error) {
	for {
		newObjects, err := orderedObjectBatch(ctx, store, missingReferences(objects), maxConcurrency)
		if err != nil {
			return nil, err
		}
		objects = append(objects, newObjects...)
		if len(newObjects) == 0 {
			break
		}
	}

	// Topologically sort new closure.
	for sortEnd := 0; sortEnd < len(objects); sortEnd++ {
		sorted := objects[:sortEnd]
		unsorted := objects[sortEnd:]
		i := slices.IndexFunc(unsorted, func(obj Object) bool {
			info := obj.Info()
			for ref := range info.References.Values() {
				if ref != info.StorePath && !objectsHavePath(slices.Values(sorted), ref) {
					return false
				}
			}
			return true
		})
		if i == -1 {
			// Keep objects with missing references at the end of the export
			// so others may succeed.
			break
		}
		// Move object to front of unsorted slice.
		unsorted[0], unsorted[i] = unsorted[i], unsorted[0]
	}

	return objects, nil
}

func orderedObjectBatch(ctx context.Context, store Store, paths iter.Seq[Path], maxConcurrency int) ([]Object, error) {
	pathSet := sets.Collect(paths)
	if pathSet.Len() == 0 {
		return nil, nil
	}
	batch, err := ObjectBatch(ctx, store, pathSet, maxConcurrency)
	if err != nil {
		return nil, err
	}
	result := make([]Object, 0, pathSet.Len())
	for path := range paths {
		i := slices.IndexFunc(batch, func(obj Object) bool {
			return obj.Info().StorePath == path
		})
		if i != -1 {
			result = append(result, batch[i])
		}
	}
	return result, nil
}

func missingReferences(objects []Object) iter.Seq[Path] {
	return func(yield func(Path) bool) {
		for _, obj := range objects {
			for ref := range obj.Info().References.Values() {
				if !objectsHavePath(slices.Values(objects), ref) {
					if !yield(ref) {
						return
					}
				}
			}
		}
	}
}

func objectsHavePath(objects iter.Seq[Object], path Path) bool {
	for obj := range objects {
		if obj.Info().StorePath == path {
			return true
		}
	}
	return false
}

// ExportTrailer holds metadata about a Nix store object
// used in the `nix-store --export` format.
type ExportTrailer struct {
	StorePath      Path
	References     sets.Sorted[Path]
	Deriver        Path
	ContentAddress ContentAddress
}

// Clone performs a deep copy of an [*ExportTrailer].
func (t *ExportTrailer) Clone() *ExportTrailer {
	if t == nil {
		return nil
	}
	t = new(*t)
	t.References = *t.References.Clone()
	return t
}

// An ExportWriter serializes zero or more NARs to a stream
// in `nix-store --export` format.
type ExportWriter struct {
	w          io.Writer
	trailerBuf []byte
	header     bool
	closed     bool
}

// NewExportWriter returns a new [ExportWriter] that writes to w.
// The caller is responsible for calling [ExportWriter.Close] on the returned exporter
// to finish the stream.
func NewExportWriter(w io.Writer) *ExportWriter {
	return &ExportWriter{w: w}
}

// WriteObject copies obj to the export.
func (ew *ExportWriter) WriteObject(ctx context.Context, obj Object) error {
	if err := ew.writeHeader(); err != nil {
		return err
	}
	if err := obj.WriteNAR(ctx, ew.w); err != nil {
		return err
	}
	if err := ew.Trailer(obj.Info().ExportTrailer()); err != nil {
		return err
	}
	return nil
}

// StoreImport implements [Importer] by copying r to the export.
// StoreImport performs some basic checks that r is a valid `nix-store --export` stream,
// but broadly copies r verbatim.
func (ew *ExportWriter) StoreImport(ctx context.Context, r io.Reader) error {
	if ew.closed {
		return fmt.Errorf("write to closed exporter")
	}
	if ew.header {
		return fmt.Errorf("copy export: missing trailer")
	}

	// Check that data starts with either [exportObjectMarker] or [exportEOFMarker].
	buf := make([]byte, 0, 32<<10)
	n, err := io.ReadAtLeast(r, buf[:cap(buf)], max(len(exportObjectMarker), len(exportEOFMarker))+1)
	buf = buf[:n]
	if bytes.HasPrefix(buf, []byte(exportEOFMarker)) {
		if len(buf) > len(exportEOFMarker) {
			return fmt.Errorf("copy export: export does not start with object marker")
		}
		// Ignore any error. We already read the EOF marker.
		return nil
	}
	if len(buf) < len(exportObjectMarker) {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = fmt.Errorf("copy export: %v", io.ErrUnexpectedEOF)
		}
		// [io.ReadAtLeast] guarantees that the error is non-nil.
		return err
	}
	if string(buf[:len(exportObjectMarker)]) != exportObjectMarker {
		return fmt.Errorf("copy export: export does not start with object marker")
	}

	for {
		// Copy up to the last len(exportEOFMarker) bytes.
		i := len(buf) - len(exportEOFMarker)
		if _, err := ew.w.Write(buf[:i]); err != nil {
			return err
		}
		buf = append(buf[:0], buf[i:]...)

		// Read at least one more byte.
		n, err := io.ReadAtLeast(r, buf[len(buf):cap(buf)], 1)
		buf = buf[:len(buf)+n]
		if err != nil {
			finalWriteSize := len(buf)
			if err == io.EOF {
				finalWriteSize -= len(exportEOFMarker)
				if bytes.HasSuffix(buf, []byte(exportEOFMarker)) {
					err = nil
				} else {
					err = fmt.Errorf("copy export: export does not end with EOF marker")
				}
			}
			if finalWriteSize > 0 {
				if _, writeError := ew.w.Write(buf[:finalWriteSize]); writeError != nil {
					return writeError
				}
			}
			return err
		}
	}
}

func (ew *ExportWriter) writeHeader() error {
	if ew.closed {
		return fmt.Errorf("write to closed exporter")
	}
	if ew.header {
		return nil
	}
	if _, err := io.WriteString(ew.w, exportObjectMarker); err != nil {
		return err
	}
	ew.header = true
	return nil
}

// Write writes bytes of a store object to the exporter's underlying writer.
func (ew *ExportWriter) Write(p []byte) (int, error) {
	if err := ew.writeHeader(); err != nil {
		return 0, err
	}
	return ew.w.Write(p)
}

// ReadFrom implements [io.ReaderFrom]
// by copying store object data from r to the exporter's underlying writer.
func (ew *ExportWriter) ReadFrom(r io.Reader) (int64, error) {
	if err := ew.writeHeader(); err != nil {
		return 0, err
	}
	return io.Copy(ew.w, r)
}

// Trailer marks the end of a store object in the stream.
// Subsequent calls to [ExportWriter.Write] will be part of a new store object.
func (ew *ExportWriter) Trailer(t *ExportTrailer) error {
	if ew.closed {
		return fmt.Errorf("write nix store export trailer: write to closed exporter")
	}
	if !ew.header {
		return fmt.Errorf("write nix store export trailer: NAR not yet written")
	}
	ew.header = false

	ew.trailerBuf = ew.trailerBuf[:0]
	ew.trailerBuf = append(ew.trailerBuf, exportTrailerMarker...)
	ew.trailerBuf = appendNARString(ew.trailerBuf, string(t.StorePath))
	ew.trailerBuf = binary.LittleEndian.AppendUint64(ew.trailerBuf, uint64(t.References.Len()))
	for _, ref := range t.References.All() {
		ew.trailerBuf = appendNARString(ew.trailerBuf, string(ref))
	}
	ew.trailerBuf = appendNARString(ew.trailerBuf, string(t.Deriver))
	if t.ContentAddress.IsZero() {
		ew.trailerBuf = binary.LittleEndian.AppendUint64(ew.trailerBuf, 0)
	} else {
		// Nix 1.X used this field to store RSA-based signatures.
		// Nix 2.0 onwards ignore this field, so we use it to inject a content addressability assertion.
		ew.trailerBuf = binary.LittleEndian.AppendUint64(ew.trailerBuf, 1)
		ew.trailerBuf = appendNARString(ew.trailerBuf, t.ContentAddress.String())
	}

	if _, err := ew.w.Write(ew.trailerBuf); err != nil {
		return err
	}
	return nil
}

// Close writes the footer of the export to the exporter's underlying writer.
// Close returns an error if a store object has been written
// but [ExportWriter.Trailer] has not been called.
// Close does not close the underlying writer.
func (ew *ExportWriter) Close() error {
	if ew.closed {
		return fmt.Errorf("close nar exporter: exporter already closed")
	}
	if ew.header {
		return fmt.Errorf("close nar exporter: missing trailer")
	}

	_, err := io.WriteString(ew.w, exportEOFMarker)
	if err != nil {
		return err
	}
	return nil
}

const stringAlign = 8

func appendNARString(dst []byte, s string) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(len(s)))
	dst = append(dst, s...)
	if off := len(s) % stringAlign; off != 0 {
		for i := 0; i < stringAlign-off; i++ {
			dst = append(dst, 0)
		}
	}
	return dst
}

func newExportError(paths []Path, err error) error {
	switch len(paths) {
	case 0:
		return fmt.Errorf("export store objects: %w", err)
	case 1:
		return fmt.Errorf("export %s: %w", paths[0], err)
	default:
		return fmt.Errorf("export %s (+%d more): %w", paths[0], len(paths)-1, err)
	}
}
