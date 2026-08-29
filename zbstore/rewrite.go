// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strconv"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/macho"
	"zb.256lights.llc/pkg/internal/uuid8"
	"zombiezen.com/go/nix"
)

// A Rewriter represents a modification to a store object to account for self-references.
// It must be safe to call methods on a Rewriter from multiple goroutines concurrently.
type Rewriter interface {
	// Rewrite returns the bytes to replace in the store object
	// starting at WriteOffset.
	// context is a reader over the bytes in the store object in ReadRange.
	Rewrite(newDigest string, context io.Reader) ([]byte, error)
	// WriteOffset returns the offset from the beginning of the NAR file
	// of the first byte that needs to be rewritten.
	WriteOffset() int64
	// ReadRange returns the offset from the beginning of the NAR file
	// of the first byte that needs to be read
	// to the last byte (exclusive) that needs to be read
	// to compute the rewrite.
	// If no context is required to compute the rewrite,
	// ReadOffset shall return the same value as WriteOffset for both start and end.
	// If ReadRange overlaps with WriteOffset,
	// the content of the bytes at WriteOffset are not defined and should be ignored.
	ReadRange() (start, end int64)
}

// SelfReference is a [Rewriter] that represents a self-reference.
type SelfReference interface {
	Rewriter

	// AppendReferenceText appends the textual representation of the self-reference location
	// as used in computing the content address to dst.
	// Such strings must not contain a "|" character.
	AppendReferenceText(dst []byte) ([]byte, error)
}

// Rewrite applies rewriters to f.
// newDigest is the string to replace self-references with.
// The length of newDigest must be the same as the length of the digest passed to [SourceSHA256ContentAddress].
// Rewrites are performed in the same order as they are present in the slice.
//
// f is treated like it starts at baseOffset bytes from the beginning of a NAR serialization.
// This can be used to apply a subset of rewrites from [SourceSHA256ContentAddress]
// to a particular file inside the store object.
func Rewrite(f io.ReadWriteSeeker, baseOffset int64, newDigest string, rewriters []Rewriter) error {
	if newDigest == "" {
		return fmt.Errorf("rewrite hash: digest empty")
	}
	if baseOffset < 0 {
		return fmt.Errorf("rewrite hash: negative base offset (%d)", baseOffset)
	}

	for len(rewriters) > 0 {
		readStart, readEnd := rewriters[0].ReadRange()
		if readStart < baseOffset {
			return fmt.Errorf("rewrite hash: rewrite read start (%d) < base offset (%d)", readStart, baseOffset)
		}
		if readEnd < readStart {
			return fmt.Errorf("rewrite hash: rewrite read start (%d) > rewrite read end (%d)", readStart, readEnd)
		}
		writeOffset := rewriters[0].WriteOffset()
		if writeOffset < baseOffset {
			return fmt.Errorf("rewrite hash: rewrite offset (%d) < base offset (%d)", writeOffset, baseOffset)
		}
		var b []byte
		if readStart == readEnd {
			var err error
			b, err = rewriters[0].Rewrite(newDigest, bytebuffer.Null{})
			if err != nil {
				return fmt.Errorf("rewrite hash: %v", err)
			}
		} else {
			if _, err := f.Seek(readStart-baseOffset, io.SeekStart); err != nil {
				return fmt.Errorf("rewrite hash: %v", err)
			}
			var err error
			b, err = rewriters[0].Rewrite(newDigest, io.LimitReader(f, readEnd-readStart))
			if err != nil {
				return fmt.Errorf("rewrite hash: %v", err)
			}
		}

		if len(b) > 0 {
			if _, err := f.Seek(writeOffset-baseOffset, io.SeekStart); err != nil {
				return fmt.Errorf("rewrite hash: %v", err)
			}
			if _, err := f.Write(b); err != nil {
				return fmt.Errorf("rewrite hash: %v", err)
			}
		}
		rewriters = rewriters[1:]
	}

	return nil
}

// A SelfReferenceOffset is a [SelfReference]
// that represents a simple textual reference to the store path's digest.
// It is stored as an offset in bytes relative to the beginning of the NAR file.
type SelfReferenceOffset int64

// ReadRange returns (int64(offset), int64(offset)).
func (offset SelfReferenceOffset) ReadRange() (start, end int64) {
	return int64(offset), int64(offset)
}

// WriteOffset returns the offset as an int64.
func (offset SelfReferenceOffset) WriteOffset() int64 {
	return int64(offset)
}

// Rewrite returns newDigest.
func (offset SelfReferenceOffset) Rewrite(newDigest string, context io.Reader) ([]byte, error) {
	return []byte(newDigest), nil
}

// AppendReferenceText implements [SelfReference]
// by appending the decimal representation of the offset to dst.
func (offset SelfReferenceOffset) AppendReferenceText(dst []byte) ([]byte, error) {
	if offset < 0 {
		return dst, fmt.Errorf("append self-reference: offset is negative")
	}
	return strconv.AppendInt(dst, int64(offset), 10), nil
}

// MachOUUIDRewrite is a [Rewriter]
// that replaces the LC_UUID command value
// with one based on the hash of the data from ImageStart to CodeEnd.
type MachOUUIDRewrite struct {
	// ImageStart is the offset in bytes that the Mach-O file starts
	// relative to the beginning of the NAR file.
	ImageStart int64
	// UUIDStart is the offset in bytes that the Mach-O UUID starts
	// relative to the beginning of the NAR file.
	UUIDStart int64
	// CodeEnd is the offset in bytes to the first byte of the code signature section
	// relative to the beginning of the NAR file.
	// CodeEnd is also the end of the image signature range.
	CodeEnd int64
}

// ReadRange returns (rewrite.ImageStart, rewrite.CodeEnd).
func (rewrite *MachOUUIDRewrite) ReadRange() (start, end int64) {
	return rewrite.ImageStart, rewrite.CodeEnd
}

// WriteOffset returns rewrite.UUIDStart.
func (rewrite *MachOUUIDRewrite) WriteOffset() int64 {
	return rewrite.UUIDStart
}

// Rewrite is equivalent to rewrite.Sign(nil, context).
func (rewrite *MachOUUIDRewrite) Rewrite(newDigest string, context io.Reader) ([]byte, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, context, rewrite.UUIDStart-rewrite.ImageStart); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("compute mach-o UUID: %v", err)
	}

	// Hash the existing UUID as null bytes
	// but skip the existing data.
	const uuidSize = 16
	var buf [uuidSize]byte
	h.Write(buf[:])
	if _, err := io.ReadFull(context, buf[:]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("compute mach-o UUID: %v", err)
	}

	if _, err := io.CopyN(h, context, rewrite.CodeEnd-(rewrite.UUIDStart+uuidSize)); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("compute mach-o UUID: %v", err)
	}

	u := uuid8.FromBytes(h.Sum(nil))
	return u[:], nil
}

// MachOSignatureRewrite is a [Rewriter]
// that represents a recomputation of an ad hoc Mach-O code signature
// due to self references inside the executable section.
// A multi-architecture Mach-O file may contain multiple rewrites:
// one for each architecture.
type MachOSignatureRewrite struct {
	// ImageStart is the offset in bytes that the Mach-O file starts
	// relative to the beginning of the NAR file.
	ImageStart int64
	// CodeEnd is the offset in bytes to the first byte of the code signature section
	// relative to the beginning of the NAR file.
	// CodeEnd is also the end of the image signature range.
	CodeEnd int64
	// PageSize is the number of bytes to hash for each hash slot.
	// A value <=1 indicates "infinity",
	// meaning that the entirety of the image signature range
	// turns into a single signature.
	PageSize int
	// HashType is the hash algorithm in use.
	HashType nix.HashType
	// HashOffset is the offset in bytes of the hash slot element at index zero
	// relative to the beginning of the NAR file.
	HashOffset int64
}

// ReadRange returns (rewrite.ImageStart, rewrite.CodeEnd).
func (rewrite *MachOSignatureRewrite) ReadRange() (start, end int64) {
	return rewrite.ImageStart, rewrite.CodeEnd
}

// WriteOffset returns rewrite.HashOffset.
func (rewrite *MachOSignatureRewrite) WriteOffset() int64 {
	return rewrite.HashOffset
}

// Rewrite is equivalent to rewrite.Sign(nil, context).
func (rewrite *MachOSignatureRewrite) Rewrite(newDigest string, context io.Reader) ([]byte, error) {
	return rewrite.Sign(nil, context)
}

// CodeLimit returns the number of bytes that need to be hashed.
func (rewrite *MachOSignatureRewrite) CodeLimit() uint64 {
	return uint64(rewrite.codeLimit())
}

func (rewrite *MachOSignatureRewrite) codeLimit() int64 {
	return rewrite.CodeEnd - rewrite.ImageStart
}

// HashSlots returns the number of hashes that this rewrite will produce.
func (rewrite *MachOSignatureRewrite) HashSlots() int {
	codeLimit := rewrite.codeLimit()
	if codeLimit < 0 {
		return 0
	}
	return int(hashSlotsForCodeLimit(uint64(codeLimit), uint64(rewrite.PageSize)))
}

func hashSlotsForCodeLimit(codeLimit uint64, pageSize uint64) int64 {
	if pageSize <= 1 {
		return 1
	}
	return int64((codeLimit + (pageSize - 1)) / pageSize)
}

// HashSize returns the total size of all hash slots in bytes.
func (rewrite *MachOSignatureRewrite) HashSize() int {
	return rewrite.HashSlots() * rewrite.HashType.Size()
}

// IsPageSizeInfinite reports whether the entirety of the image signature range
// turns into a single signature.
func (rewrite *MachOSignatureRewrite) IsPageSizeInfinite() bool {
	return rewrite.PageSize <= 1
}

// Sign computes the code signature hash slots for the Mach-O image
// and appends them to dst.
// code should be a reader that starts at rewrite.ImageStart.
// Sign will not read more than [*MachOSignatureRewrite.CodeLimit] bytes from code.
func (rewrite *MachOSignatureRewrite) Sign(dst []byte, code io.Reader) ([]byte, error) {
	codeLimit := rewrite.codeLimit()
	if codeLimit < 0 {
		return dst, fmt.Errorf("sign mach-o binary: code end (%d) is before image start (%d)", rewrite.CodeEnd, rewrite.ImageStart)
	}
	if !rewrite.IsPageSizeInfinite() && hashSlotsForCodeLimit(uint64(codeLimit), uint64(rewrite.PageSize)) > math.MaxInt/int64(rewrite.HashType.Size()) {
		return dst, fmt.Errorf("sign mach-o binary: hash slot size too large")
	}

	h := nix.NewHasher(rewrite.HashType)
	if rewrite.IsPageSizeInfinite() {
		if _, err := io.CopyN(h, code, codeLimit); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return dst, fmt.Errorf("sign mach-o binary: %w", err)
		}
		dst = h.Sum(dst)
		return dst, nil
	}

	for n := int64(0); n < codeLimit; {
		pageSize := min(int64(rewrite.PageSize), codeLimit-n)
		if _, err := io.CopyN(h, code, pageSize); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return dst, fmt.Errorf("sign mach-o binary: %w", err)
		}
		codeLimit -= pageSize
		dst = h.Sum(dst)
		h.Reset()
	}
	return dst, nil
}

type rewritableCodeDirectory struct {
	macho.CodeDirectory
	hashSlotsStart int
	hashSlotsEnd   int
}

// findRewritableCodeDirectory parses a code signature [macho.SuperBlob]
// and checks whether it fits the format of what can be rewritten.
func findRewritableCodeDirectory(signatureBytes []byte) (*rewritableCodeDirectory, error) {
	const (
		superBlobMinSize = 12
		blobIndexSize    = 8
	)

	signatureBlob := new(macho.SuperBlob)
	if err := signatureBlob.UnmarshalBinary(signatureBytes); err != nil {
		return nil, err
	}
	if signatureBlob.Magic != macho.CodeSignatureMagicEmbeddedSignature {
		return nil, fmt.Errorf("signature not a %v", signatureBlob.Magic)
	}
	if len(signatureBlob.Blobs) != 1 {
		return nil, fmt.Errorf("blob count %d", len(signatureBlob.Blobs))
	}
	if got, want := signatureBlob.Blobs[0].Type, macho.SuperBlobCodeDirectorySlot; got != want {
		return nil, fmt.Errorf("slot is %v instead of %v", got, want)
	}
	if got, want := signatureBlob.Blobs[0].Blob.Magic, macho.CodeSignatureMagicCodeDirectory; got != want {
		return nil, fmt.Errorf("slot is %v instead of %v", got, want)
	}

	// codeDirectoryOffset is immediately after the super blob header.
	// [*macho.SuperBlob.UnmarshalBinary] guarantees that there are no gaps between blobs.
	// Since we verify there is a single blob, it will appear at this offset.
	const codeDirectoryOffset = superBlobMinSize + blobIndexSize
	codeDirectoryBlob := signatureBytes[codeDirectoryOffset:]

	result := new(rewritableCodeDirectory)
	if err := result.UnmarshalBinary(codeDirectoryBlob); err != nil {
		return nil, err
	}
	if result.Flags != macho.CodeSignatureAdHoc|macho.CodeSignatureLinkerSigned {
		return nil, fmt.Errorf("unsupported flags")
	}
	if result.SpecialSlotCount != 0 {
		return nil, fmt.Errorf("special hash slots not supported")
	}
	if pageSize, ok := result.PageSize.Bytes(); !ok {
		return nil, fmt.Errorf("page size (%v) too large", result.PageSize)
	} else if got, want := int64(result.HashSlotCount()), hashSlotsForCodeLimit(result.CodeLimit, pageSize)+int64(result.SpecialSlotCount); got != want {
		return nil, fmt.Errorf("found %d hash slots but expected %d based on codeLimit=%d and pageSize=%v", got, want, result.CodeLimit, result.PageSize)
	}
	hashOffset := binary.BigEndian.Uint32(codeDirectoryBlob[16:])
	result.hashSlotsStart = codeDirectoryOffset + int(hashOffset)
	result.hashSlotsEnd = result.hashSlotsStart + len(result.HashData)
	return result, nil
}

func (cd *rewritableCodeDirectory) toRewrite(imageStart int64) (*MachOSignatureRewrite, error) {
	if imageStart < 0 {
		return nil, fmt.Errorf("negative image start")
	}
	codeEnd := imageStart + int64(cd.CodeLimit)
	rewrite := &MachOSignatureRewrite{
		ImageStart: imageStart,
		CodeEnd:    codeEnd,
	}
	if cd.PageSize != 0 {
		pageSize, ok := cd.PageSize.Bytes()
		if !ok || pageSize > math.MaxInt {
			return nil, fmt.Errorf("page size (%v) too large", cd.PageSize)
		}
		rewrite.PageSize = int(pageSize)
	}
	switch cd.HashType {
	case macho.HashTypeSHA1:
		rewrite.HashType = nix.SHA1
	case macho.HashTypeSHA256:
		rewrite.HashType = nix.SHA256
	default:
		return nil, fmt.Errorf("unsupported hash type %v", cd.HashType)
	}
	rewrite.HashOffset = codeEnd + int64(cd.hashSlotsStart)
	return rewrite, nil
}
