// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"zombiezen.com/go/nix/nar"
)

// An Importer can receive serialized zb store objects
// in the `nix-store --export` format.
// If an Importer receives an object identical one it already has,
// it should ignore the new object and it should not return an error.
type Importer interface {
	StoreImport(ctx context.Context, r io.Reader) error
}

// A NARReceiver processes multiple NAR files.
// After the NAR file has been written to the receiver,
// ReceiveNAR is called to provide metadata about the written NAR file.
// Subsequent writes will be for a new NAR file.
// If the Write method of a NARReceiver returns an error,
// the NARReceiver should not receive further calls.
type NARReceiver interface {
	io.Writer
	ReceiveNAR(trailer *ExportTrailer)
}

// ReceiveExport processes a stream of NARs in `nix-store --export` format,
// returning the first error encountered.
//
// ReceiveExport will not read beyond the end of the export,
// so there may still be data remaining in r after a call to ReceiveExport.
func ReceiveExport(receiver NARReceiver, r io.Reader) error {
	buf := make([]byte, len(exportObjectMarker))
	ew := &errWriter{w: receiver}
	for {
		if _, err := readFull(r, buf[:len(exportObjectMarker)]); err != nil {
			return err
		}
		if string(buf[:len(exportEOFMarker)]) == exportEOFMarker {
			return nil
		}
		if string(buf[:len(exportObjectMarker)]) != exportObjectMarker {
			return fmt.Errorf("invalid object separator %x", buf[:])
		}

		nr := nar.NewReader(io.TeeReader(r, ew))
		nr.AllowTrailingData()
		for {
			_, err := nr.Next()
			if ew.err != nil {
				// Always pass through writer errors verbatim.
				return ew.err
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
			return fmt.Errorf("invalid trailer start %x", buf[:])
		}

		t := new(ExportTrailer)
		var err error
		buf, err = readNARString(r, buf[:0])
		if err != nil {
			return fmt.Errorf("read store path: %w", err)
		}
		t.StorePath = Path(buf)

		buf = buf[:0]
		nrefs, err := readUint64(r, &buf)
		if err != nil {
			return fmt.Errorf("read references: %w", err)
		}
		if nrefs > 100_000 {
			return fmt.Errorf("read references: too many references (%d)", nrefs)
		}
		t.References.Grow(int(nrefs))
		for range nrefs {
			var err error
			buf, err = readNARString(r, buf[:0])
			if err != nil {
				return fmt.Errorf("read references: %w", err)
			}
			t.References.Add(Path(buf))
		}

		buf, err = readNARString(r, buf[:0])
		if err != nil {
			return fmt.Errorf("read deriver: %w", err)
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
				return fmt.Errorf("read content address assertion: %v", err)
			}
			if err := t.ContentAddress.UnmarshalText(buf); err != nil {
				return fmt.Errorf("read content address assertion: %v", err)
			}
		default:
			return fmt.Errorf("invalid end of object marker %x", x)
		}

		receiver.ReceiveNAR(t)
	}
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

type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) Write(p []byte) (int, error) {
	if ew.err != nil {
		return 0, ew.err
	}
	var n int
	n, ew.err = ew.w.Write(p)
	return n, ew.err
}
