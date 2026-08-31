// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"zb.256lights.llc/pkg/bytebuffer"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// An ObjectWriter can receive serialized zb store objects.
//
// If an ObjectWriter receives an [Object] identical to one it already has,
// it should ignore the new object and it should not return an error.
//
// Implementations must not retain the object argument.
type ObjectWriter interface {
	WriteObject(ctx context.Context, object Object) error
}

// InfoRecorder is an [ObjectWriter] that records the [*ObjectInfo] written to it.
// It optionally passes through its methods to another [ObjectWriter].
type InfoRecorder struct {
	Written      []*ObjectInfo
	ObjectWriter ObjectWriter
}

// WriteObject appends the object's info to rec.Paths
// and then calls WriteObject on its wrapped [ObjectWriter].
func (rec *InfoRecorder) WriteObject(ctx context.Context, object Object) error {
	info := object.Info().Clone()
	rec.Written = append(rec.Written, info)

	if rec.ObjectWriter != nil {
		if err := rec.ObjectWriter.WriteObject(ctx, object); err != nil {
			return err
		}
	}
	return nil
}

// An Importer can receive serialized zb store objects
// in the `nix-store --export` format.
// If StoreImport does not return an error,
// then it has read a valid `nix-store --export` from the [io.Reader],
// stopping at the end of the export.
//
// If an Importer receives an object identical to one it already has,
// it should ignore the new object and it should not return an error.
type Importer interface {
	ObjectWriter
	StoreImport(ctx context.Context, r io.Reader) error
}

// A BufferedImporter wraps an [ObjectWriter] to implement the [Importer] interface.
// The zero or nil [*BufferedImporter] can be used to read exports
// and verify their format without storing the NAR data.
type BufferedImporter struct {
	// ObjectWriter is the destination for calls to [*BufferedImporter.WriteObject] and [*BufferedImporter.StoreImport].
	// If ObjectWriter nil, then objects are ignored.
	ObjectWriter ObjectWriter
	// If HashType is not zero, then a hash of incoming objects is computed
	// if the import's [ObjectWriter] does not implement [Importer].
	HashType nix.HashType
	// BufferCreator is used to create buffers
	// to write received NAR data before a trailer is received.
	// Buffers will only be read if the import's [ObjectWriter] calls [Object.WriteNAR].
	//
	// [*BufferedImporter.StoreImport] attempts to reuse buffers.
	// If a buffer fails to seek to its start before [*BufferedImporter.StoreImport] writes a new NAR file to it,
	// then [*BufferedImporter.StoreImport] will close the buffer and create a new one.
	//
	// If BufferCreator is nil, then in-memory byte slices are used with reasonable limits.
	BufferCreator bytebuffer.Creator
}

// WriteObject calls WriteObject on the underlying [ObjectWriter]
// or does nothing if the importer or the [ObjectWriter] are nil.
func (importer *BufferedImporter) WriteObject(ctx context.Context, object Object) error {
	if importer == nil || importer.ObjectWriter == nil {
		return nil
	}
	return importer.ObjectWriter.WriteObject(ctx, object)
}

// StoreImport copies a stream of objects in `nix-store --export` format
// from an [io.Reader] to the underlying [ObjectWriter],
// returning the first error encountered.
// If the underlying [ObjectWriter] implements [Importer],
// then this is equivalent to calling StoreImport on it.
// Otherwise, StoreImport will write call WriteObject after each export trailer is encountered,
// buffering the objects using its [bytebuffer.Creator].
// If StoreImport does not return an error,
// then it has read a valid `nix-store --export` from the [io.Reader],
// stopping at the end of the export.
func (importer *BufferedImporter) StoreImport(ctx context.Context, r io.Reader) error {
	var createTemp bytebuffer.Creator
	var hasher *nix.Hasher
	if importer != nil {
		if dst, ok := importer.ObjectWriter.(Importer); ok {
			return dst.StoreImport(ctx, r)
		}
		if importer.BufferCreator != nil {
			createTemp = importer.BufferCreator
		} else if importer.ObjectWriter != nil {
			createTemp = bytebuffer.BufferCreator{}
		}
		if importer.ObjectWriter != nil && importer.HashType != 0 {
			hasher = nix.NewHasher(importer.HashType)
		}
	}

	buf := make([]byte, len(exportObjectMarker))
	var scratch bytebuffer.ReadWriteSeekCloser
	defer func() {
		if scratch != nil {
			scratch.Close()
		}
	}()
	for {
		if _, err := readFull(r, buf[:len(exportObjectMarker)]); err != nil {
			return err
		}
		if string(buf[:len(exportEOFMarker)]) == exportEOFMarker {
			return nil
		}
		if string(buf[:len(exportObjectMarker)]) != exportObjectMarker {
			return fmt.Errorf("import store objects: invalid object separator %x", buf[:])
		}

		var w *importWriter
		narReaderSource := r
		if createTemp != nil {
			if scratch == nil {
				var err error
				scratch, err = createTemp.CreateBuffer(-1)
				if err != nil {
					return fmt.Errorf("import store objects: %v", err)
				}
			}
			w = &importWriter{
				w:      scratch,
				hasher: hasher,
			}
			narReaderSource = io.TeeReader(r, w)
		}

		nr := nar.NewReader(narReaderSource)
		nr.AllowTrailingData()
		for {
			_, err := nr.Next()
			if w != nil && w.err != nil {
				// Get writer error directly instead of through NAR reader.
				return fmt.Errorf("import store objects: buffering: %v", w.err)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}

		if _, err := readFull(r, buf[:len(exportTrailerMarker)]); err != nil {
			return err
		}
		if string(buf[:len(exportTrailerMarker)]) != exportTrailerMarker {
			return fmt.Errorf("import store objects: invalid trailer start %x", buf[:])
		}

		var t ExportTrailer
		var err error
		buf, err = readNARString(r, buf[:0])
		if err != nil {
			return fmt.Errorf("import store objects: read store path: %w", err)
		}
		t.StorePath = Path(buf)

		buf = buf[:0]
		nrefs, err := readUint64(r, &buf)
		if err != nil {
			return fmt.Errorf("import store objects: %s: read references: %w", t.StorePath, err)
		}
		if nrefs > 100_000 {
			return fmt.Errorf("import store objects: %s: read references: too many references (%d)", t.StorePath, nrefs)
		}
		t.References.Grow(int(nrefs))
		for range nrefs {
			var err error
			buf, err = readNARString(r, buf[:0])
			if err != nil {
				return fmt.Errorf("import store objects: %s: read references: %w", t.StorePath, err)
			}
			t.References.Add(Path(buf))
		}

		buf, err = readNARString(r, buf[:0])
		if err != nil {
			return fmt.Errorf("import store objects: %s: read deriver: %w", t.StorePath, err)
		}
		t.Deriver = Path(buf)

		buf = buf[:0]
		x, err := readUint64(r, &buf)
		if err != nil {
			return err
		}
		switch x {
		case 0:
			// No content address assertion or signatures.
		case 1:
			buf, err = readNARString(r, buf[:0])
			if err != nil {
				return fmt.Errorf("import store objects: %s: read content address assertion: %v", t.StorePath, err)
			}
			if err := t.ContentAddress.UnmarshalText(buf); err != nil {
				return fmt.Errorf("import store objects: %s: read content address assertion: %v", t.StorePath, err)
			}
		default:
			return fmt.Errorf("import store objects: %s: invalid end of object marker %x", t.StorePath, x)
		}

		if importer != nil && importer.ObjectWriter != nil {
			obj := &importObject{
				r:             make(chan io.ReadSeeker, 1),
				narSize:       w.size,
				ExportTrailer: t,
			}
			obj.r <- scratch
			if hasher != nil {
				obj.narHash = hasher.SumHash()
			}
			if err := importer.ObjectWriter.WriteObject(ctx, obj); err != nil {
				// Return WriteObject errors verbatim.
				return err
			}
		}

		// Attempt to reuse the buffer and reclaim storage space.
		// If this fails, then attempt to create a new buffer on the next loop.
		if scratch != nil {
			truncateIfPossible(scratch, 0)
			if _, err := scratch.Seek(0, io.SeekStart); err != nil {
				scratch.Close()
				scratch = nil
			}
		}

		if hasher != nil {
			hasher.Reset()
		}
	}
}

type importWriter struct {
	w      io.Writer
	hasher *nix.Hasher
	size   int64
	err    error
}

func (w *importWriter) Write(p []byte) (n int, err error) {
	if w.err != nil {
		return 0, w.err
	}
	n, w.err = w.w.Write(p)
	if w.hasher != nil {
		w.hasher.Write(p[:n])
	}
	w.size += int64(n)
	return n, w.err
}

type importObject struct {
	r       chan io.ReadSeeker
	narHash nix.Hash
	narSize int64
	ExportTrailer
}

func (obj *importObject) Info() *ObjectInfo {
	return &ObjectInfo{
		StorePath:      obj.StorePath,
		NARHash:        obj.narHash,
		NARSize:        obj.narSize,
		References:     obj.References,
		ContentAddress: obj.ContentAddress,
	}
}

func (obj *importObject) WriteNAR(ctx context.Context, dst io.Writer) error {
	select {
	case r := <-obj.r:
		defer func() { obj.r <- r }()
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("write nar for %s: %v", obj.StorePath, err)
		}
		_, err := io.CopyN(dst, r, obj.narSize)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("write nar for %s: %v", obj.StorePath, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("write nar for %s: %w", obj.StorePath, ctx.Err())
	}
}

func truncateIfPossible(f io.ReadWriteSeeker, size int64) error {
	t, ok := f.(interface{ Truncate(size int64) error })
	if !ok {
		return nil
	}
	return t.Truncate(size)
}

// readNARString reads a NAR-style string from r
// and appends it to the given byte slice.
// NAR strings start with an unsigned 64-bit little endian length
// and are padded to 8-byte alignment.
func readNARString(r io.Reader, buf []byte) ([]byte, error) {
	start := len(buf)
	n, err := readUint64(r, &buf)
	buf = buf[:start] // drop length from buffer
	if err != nil {
		return buf, err
	}
	if n > 4096 {
		return buf, fmt.Errorf("nar string too large (%d bytes)", n)
	}
	readSize := padStringSize(int(n))
	buf = slices.Grow(buf, readSize)
	if _, err := readFull(r, buf[start:start+readSize]); err != nil {
		return buf, err
	}
	return buf[:start+int(n)], nil
}

func readUint64(r io.Reader, buf *[]byte) (uint64, error) {
	if buf == nil {
		buf = new([]byte)
	}
	*buf = slices.Grow(*buf, 8)
	newEnd := len(*buf) + 8
	readBuf := (*buf)[len(*buf):newEnd]
	if _, err := readFull(r, readBuf); err != nil {
		return 0, err
	}
	*buf = (*buf)[:newEnd]
	return binary.LittleEndian.Uint64(readBuf), nil
}

// padStringSize returns the smallest integer >= n
// that is evenly divisible by [stringAlign].
func padStringSize(n int) int {
	return (n + stringAlign - 1) &^ (stringAlign - 1)
}

// readFull is the same as [io.ReadFull]
// except it never returns [io.EOF]:
// it will instead return [io.ErrUnexpectedEOF] if no bytes were read before EOF.
func readFull(r io.Reader, p []byte) (int, error) {
	n, err := io.ReadFull(r, p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

type discardBuffer struct{}

func (discardBuffer) Read(p []byte) (int, error) {
	return 0, errors.New("cannot read from discard buffer")
}

func (discardBuffer) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discardBuffer) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 || whence != io.SeekStart {
		return 0, errors.New("cannot seek in discard buffer")
	}
	return 0, nil
}

func (discardBuffer) Close() error {
	return nil
}
