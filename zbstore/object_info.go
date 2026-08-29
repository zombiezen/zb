// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"encoding"
	"fmt"
	"io"
	"strconv"

	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
)

var _ interface {
	encoding.TextAppender
	encoding.TextMarshaler
	encoding.TextUnmarshaler
} = (*ObjectInfo)(nil)

// ObjectInfo is the metadata of an [*Object].
type ObjectInfo struct {
	// StorePath is the absolute path of this store object
	// (e.g. "/opt/zb/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1").
	StorePath Path
	// NARHash is a hash of the store object as an uncompressed .nar file.
	// If it is zero, then it is unknown.
	NARHash nix.Hash
	// NARSize is the size of the uncompressed .nar file in bytes.
	// If non-positive, then it is unknown.
	NARSize int64
	// References is the set of store objects that this store object references.
	References sets.Sorted[Path]
	// ContentAddress is the content-addressability assertion
	// that the store path is derived from.
	ContentAddress ContentAddress
}

// HasNARSize reports whether the object's NAR size is known.
func (info *ObjectInfo) HasNARSize() bool {
	return info.NARSize > 0
}

// ExportTrailer creates an [*ExportTrailer] from [*ObjectInfo].
func (info *ObjectInfo) ExportTrailer() *ExportTrailer {
	return &ExportTrailer{
		StorePath:      info.StorePath,
		References:     *info.References.Clone(),
		ContentAddress: info.ContentAddress,
	}
}

// Clone performs a deep copy of an [*ObjectInfo].
func (info *ObjectInfo) Clone() *ObjectInfo {
	if info == nil {
		return nil
	}
	info = new(*info)
	info.References = *info.References.Clone()
	return info
}

// AppendText implements [encoding.TextAppender]
// by appending a condensed version of a .narinfo file to dst.
// Any zero values are omitted except for store path.
func (info *ObjectInfo) AppendText(dst []byte) ([]byte, error) {
	dst = append(dst, "StorePath: "...)
	dst = append(dst, info.StorePath...)
	if !info.NARHash.IsZero() {
		dst = append(dst, "\nNarHash: "...)
		dst = append(dst, info.NARHash.Base32()...)
	}
	if info.NARSize > 0 {
		dst = append(dst, "\nNarSize: "...)
		dst = strconv.AppendInt(dst, info.NARSize, 10)
	}
	if info.References.Len() > 0 {
		dst = append(dst, "\nReferences:"...)
		for ref := range info.References.Values() {
			dst = append(dst, ' ')
			dst = append(dst, ref.Base()...)
		}
	}
	if !info.ContentAddress.IsZero() {
		dst = append(dst, "\nCA: "...)
		dst = append(dst, info.ContentAddress.String()...)
	}
	dst = append(dst, '\n')
	return dst, nil
}

// MarshalText implements [encoding.TextMarshaler]
// by calling info.AppendText(nil).
func (info *ObjectInfo) MarshalText() ([]byte, error) {
	return info.AppendText(nil)
}

// UnmarshalText implements [encoding.TextUnmarshaler]
// by parsing a .narinfo file format.
// Unrecognized keys are ignored.
func (info *ObjectInfo) UnmarshalText(src []byte) (err error) {
	*info = ObjectInfo{}
	defer func() {
		if err != nil {
			if info.StorePath != "" {
				err = fmt.Errorf("unmarshal store object info: %s: %v", info.StorePath, err)
			} else {
				err = fmt.Errorf("unmarshal store object info: %v", err)
			}
		}
	}()

	var references []byte
	hasReferences := false
	for len(src) > 0 {
		i := bytes.IndexAny(src, ":\n")
		if i < 0 || src[i] == '\n' {
			if i < 0 {
				i = len(src)
			}
			for _, b := range src[:i] {
				if b != ' ' && b != '\t' {
					return fmt.Errorf("non-empty line without ':'")
				}
			}
			i++
			if i >= len(src) {
				break
			}
			src = src[i:]
			continue
		}
		if i+len(": ") > len(src) {
			return io.ErrUnexpectedEOF
		}
		key := string(src[:i])
		if src[i+1] != ' ' {
			return fmt.Errorf("%s: space must follow ':'", key)
		}
		src = src[i+len(": "):]

		var value []byte
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			value = src[:i]
			src = src[i+1:]
		} else {
			value = src
			src = nil
		}

		switch key {
		case "StorePath":
			if info.StorePath != "" {
				return fmt.Errorf("duplicate StorePath")
			}
			if len(value) == 0 {
				return fmt.Errorf("empty StorePath")
			}
			var err error
			info.StorePath, err = ParsePath(string(value))
			if err != nil {
				return err
			}
		case "NarHash":
			if !info.NARHash.IsZero() {
				return fmt.Errorf("duplicate NarHash")
			}
			if err := info.NARHash.UnmarshalText(value); err != nil {
				return fmt.Errorf("NarHash: %v", err)
			}
		case "NarSize":
			if info.NARSize > 0 {
				return fmt.Errorf("duplicate NarSize")
			}
			var err error
			info.NARSize, err = strconv.ParseInt(string(value), 10, 64)
			if err != nil {
				return fmt.Errorf("NarSize: %v", err)
			}
			if info.NARSize <= 0 {
				return fmt.Errorf("NarSize is non-positive")
			}
		case "References":
			if hasReferences {
				return fmt.Errorf("duplicate References")
			}
			references = value
			hasReferences = true
		case "CA":
			if !info.ContentAddress.IsZero() {
				return fmt.Errorf("duplicate CA")
			}
			if err := info.ContentAddress.UnmarshalText(value); err != nil {
				return fmt.Errorf("CA: %v", err)
			}
		}
	}

	if info.StorePath == "" {
		return fmt.Errorf("store path empty")
	}
	if len(references) > 0 {
		info.References.Clear()
		info.References.Grow(len(references))
		for w := range bytes.FieldsSeq(references) {
			ref, err := info.StorePath.Dir().Object(string(w))
			if err != nil {
				//lint:ignore ST1005 Matching the field name.
				return fmt.Errorf("References: %v", err)
			}
			info.References.Add(ref)
		}
	}

	return nil
}

// Equal reports whether info and info2 are equivalent.
func (info *ObjectInfo) Equal(info2 *ObjectInfo) bool {
	if info == nil || info2 == nil {
		return info == nil && info2 == nil
	}
	if info.StorePath != info2.StorePath ||
		info.HasNARSize() != info2.HasNARSize() ||
		(!info.HasNARSize() || info.NARSize != info2.NARSize) ||
		!info.NARHash.Equal(info2.NARHash) ||
		!info.ContentAddress.Equal(info2.ContentAddress) ||
		info.References.Len() != info2.References.Len() {
		return false
	}
	for i, ref := range info.References.All() {
		if info2.References.At(i) != ref {
			return false
		}
	}
	return true
}
