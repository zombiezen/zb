// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/detect"
	"zb.256lights.llc/pkg/internal/macho"
	"zb.256lights.llc/pkg/internal/xio"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// A ContentAddress is a content-addressibility assertion.
type ContentAddress = nix.ContentAddress

// FixedCAOutputPath computes the path of a store object
// with the given directory, name, content address, and reference set.
func FixedCAOutputPath(dir Directory, name string, ca nix.ContentAddress, refs References) (Path, error) {
	if name == "" {
		return "", fmt.Errorf("compute fixed output path: name empty")
	}
	if err := ValidateContentAddress(ca, refs); err != nil {
		return "", fmt.Errorf("compute fixed output path for %s: %v", name, err)
	}
	h := ca.Hash()
	switch {
	case ca.IsText():
		return makeStorePath(dir, "text", h, name, refs)
	case IsSourceContentAddress(ca):
		return makeStorePath(dir, "source", h, name, refs)
	default:
		h2 := nix.NewHasher(nix.SHA256)
		h2.WriteString("fixed:out:")
		h2.WriteString(methodOfContentAddress(ca).prefix())
		h2.WriteString(h.Base16())
		h2.WriteString(":")
		return makeStorePath(dir, "output:out", h2.SumHash(), name, References{})
	}
}

// ValidateContentAddress checks whether the combination of the content address
// and set of references is one that will be accepted by a zb store.
// If not, it returns an error describing the issue.
func ValidateContentAddress(ca nix.ContentAddress, refs References) error {
	htype := ca.Hash().Type()
	isFixedOutput := ca.IsFixed() && !IsSourceContentAddress(ca)
	switch {
	case ca.IsZero():
		return fmt.Errorf("null content address")
	case ca.IsText() && htype != nix.SHA256:
		return fmt.Errorf("text must be content-addressed by %v (got %v)", nix.SHA256, htype)
	case refs.Self && ca.IsText():
		return fmt.Errorf("self-references not allowed in text")
	case !refs.IsEmpty() && isFixedOutput:
		return fmt.Errorf("references not allowed in fixed output")
	default:
		return nil
	}
}

// SelfReferenceAnalysis holds additional information about self-references
// computed by [SourceSHA256ContentAddress].
type SelfReferenceAnalysis struct {
	// Rewrites is the sequence of rewrites for the NAR serialization
	// required to account for self-reference digests.
	Rewrites []Rewriter
	// Paths is the set of paths in the NAR serialization that contain self-reference digests.
	// They should be in ascending order of ContentOffset.
	Paths []nar.Header
}

// HasSelfReferences reports whether the analysis is non-empty.
func (analysis *SelfReferenceAnalysis) HasSelfReferences() bool {
	return analysis != nil && (len(analysis.Rewrites) > 0 || len(analysis.Paths) > 0)
}

// Path returns the header for the given path.
func (analysis *SelfReferenceAnalysis) Path(name string) *nar.Header {
	i := slices.IndexFunc(analysis.Paths, func(hdr nar.Header) bool {
		return hdr.Path == name
	})
	if i < 0 {
		return nil
	}
	return &analysis.Paths[i]
}

// RewritesInRange returns a slice of analysis.Rewrites
// whose WriteOffset values are in the range [start, end).
func (analysis *SelfReferenceAnalysis) RewritesInRange(start, end int64) []Rewriter {
	if end < start || analysis == nil {
		return nil
	}

	// Usually the rewrites for a range are in a contiguous range.
	// Try that first.
	inRange := func(r Rewriter) bool {
		off := r.WriteOffset()
		return start <= off && off < end
	}
	i := slices.IndexFunc(analysis.Rewrites, inRange)
	if i < 0 {
		return nil
	}
	slice := analysis.Rewrites[i:]
	n := slices.IndexFunc(slice, func(r Rewriter) bool { return !inRange(r) })
	var remaining []Rewriter
	if n >= 0 {
		remaining = slice[n+1:]
		slice = slice[:n]
	}
	slice = slices.Clip(slice)

	// Check if there are more rewrites after the first not-in-range.
	// Since we clipped the slice, this will automatically allocate.
	if j := slices.IndexFunc(remaining, inRange); j >= 0 {
		slice = append(slice, remaining[j])
		for _, r := range remaining[j+1:] {
			if inRange(r) {
				slice = append(slice, r)
			}
		}
	}

	return slice
}

// ContentAddressOptions holds optional parameters for [SourceSHA256ContentAddress].
type ContentAddressOptions struct {
	// Digest is the temporary path digest (as given by [Path.Digest])
	// Digest is used to detect self-references.
	// If the store object is known to not contain self-references,
	// Digest may be the empty string.
	Digest string
	// CreateTemp is called to create temporary storage
	// for parts of a store object that require multi-pass analysis.
	// If CreateTemp is nil, multi-pass analyses are performed in-memory.
	// This is generally not recommended, as the files can be large.
	CreateTemp bytebuffer.Creator
	// If Log is not nil, it is called to provide additional diagnostics about the analysis process.
	// The messages passed in are human-readable and should not be parsed by applications.
	Log func(string)
}

// SourceSHA256ContentAddress computes the content address of a "source" store object
// from its NAR serialization.
// See [IsSourceContentAddress] for an explanation of "source" store objects.
func SourceSHA256ContentAddress(sourceNAR io.Reader, opts *ContentAddressOptions) (nix.ContentAddress, *SelfReferenceAnalysis, error) {
	h := nix.NewHasher(nix.SHA256)
	analysis, err := filterNARForContentAddress(h, sourceNAR, opts)
	if err != nil {
		return nix.ContentAddress{}, nil, fmt.Errorf("compute source content address: %v", err)
	}
	ca := nix.RecursiveFileContentAddress(h.SumHash())
	return ca, analysis, nil
}

// filterNARForContentAddress rewrites a NAR for computing the content address of a source store object.
func filterNARForContentAddress(dst io.Writer, sourceNAR io.Reader, opts *ContentAddressOptions) (analysis *SelfReferenceAnalysis, err error) {
	if opts == nil || opts.Digest == "" {
		// If there are no self-references, we only have to hash the NAR.
		if _, err := io.Copy(dst, sourceNAR); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(dst, "|"); err != nil {
			return nil, err
		}
		return new(SelfReferenceAnalysis), nil
	}

	caa := &contentAddressAnalyzer{ContentAddressOptions: *opts}
	if caa.CreateTemp == nil {
		caa.CreateTemp = bytebuffer.BufferCreator{}
	}
	var paths []nar.Header
	nr := nar.NewReader(sourceNAR)
	nw := nar.NewWriter(dst)
	digestReplacement := strings.Repeat("\x00", len(opts.Digest))
	for {
		hdr, err := nr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.Contains(hdr.Path, opts.Digest) {
			return nil, fmt.Errorf("path %s contains self-reference", hdr.Path)
		}
		if strings.Contains(hdr.LinkTarget, opts.Digest) {
			hdrClone := *hdr
			hdrClone.LinkTarget = ""
			paths = append(paths, hdrClone)
			for i := range indexSeq(hdr.LinkTarget, opts.Digest) {
				caa.rewrites = append(caa.rewrites, SelfReferenceOffset(hdr.ContentOffset+int64(i)))
			}
			hdr.LinkTarget = strings.ReplaceAll(hdr.LinkTarget, opts.Digest, digestReplacement)
		}
		if err := nw.WriteHeader(hdr); err != nil {
			return nil, err
		}

		if !hdr.Mode.IsRegular() {
			continue
		}
		initialRewritesLength := len(caa.rewrites)
		err = caa.filter(nw, hdr.ContentOffset, nr, hdr.Size)
		if len(caa.rewrites) > initialRewritesLength {
			paths = append(paths, *hdr)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := nw.Close(); err != nil {
		return nil, err
	}

	// This single pipe separator differentiates this content addressing algorithm
	// from Nix's implementation as of Nix commit 2ed075ffc0f4e22f6bc6c083ef7c84e77c687605.
	// I believe it to be more correct in avoiding potential hash collisions.
	if _, err := io.WriteString(dst, "|"); err != nil {
		return nil, err
	}

	var buf []byte
	for _, r := range caa.rewrites {
		selfRef, ok := r.(SelfReference)
		if !ok {
			continue
		}
		var err error
		buf = append(buf[:0], '|')
		buf, err = selfRef.AppendReferenceText(buf)
		if err != nil {
			return nil, err
		}
		if text := buf[1:]; bytes.ContainsAny(text, "|") {
			return nil, fmt.Errorf("self-reference serialization %q contains '|'", text)
		}
		dst.Write(buf)
	}
	return &SelfReferenceAnalysis{
		Paths:    paths,
		Rewrites: caa.rewrites,
	}, nil
}

type contentAddressAnalyzer struct {
	ContentAddressOptions

	// fileStart is the offset in bytes of the start of the file
	// relative to the beginning of the NAR file.
	fileStart int64
	// rewrites is the result slice.
	rewrites []Rewriter
}

// filter finds all the rewrites in the store object file
// and writes a version of the file to dst
// where all the rewritten sections are replaced with zero bytes.
// The new rewrites are appended to the caa.rewrites slice.
func (caa *contentAddressAnalyzer) filter(dst io.Writer, fileOffset int64, src io.Reader, srcSize int64) error {
	caa.fileStart = fileOffset

	buf := make([]byte, 1024)
	n, err := io.ReadAtLeast(src, buf, macho.MagicNumberSize)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	buf = buf[:n]
	digestReplacement := strings.Repeat("\x00", len(caa.Digest))
	hmr := detect.NewHashModuloReader(caa.Digest, digestReplacement, io.MultiReader(bytes.NewReader(buf), src))

	switch {
	case macho.IsSingleArchitecture(buf):
		return caa.zeroOutMachOCodeSignature(dst, 0, hmr, srcSize)
	case macho.IsUniversal(buf):
		return caa.zeroOutUniversalMachOCodeSignatures(dst, hmr, srcSize)
	default:
		return caa.copyWithSelfReferences(dst, hmr, srcSize)
	}
}

const maxMachOBufferSize = 4 * 1024 * 1024

func (caa *contentAddressAnalyzer) zeroOutUniversalMachOCodeSignatures(dst io.Writer, src *detect.HashModuloReader, srcSize int64) error {
	initialReferenceCount := src.ReferenceCount()

	// Read index.
	wc := new(xio.WriteCounter)
	sink := io.MultiWriter(wc, dst)
	entries, err := macho.ReadUniversalHeader(io.TeeReader(src, sink))
	caa.addNewSelfReferences(src, initialReferenceCount)
	if err != nil {
		caa.log("Potential universal Mach-O failed to parse: %v", err)
		_, err := io.Copy(sink, src)
		return err
	}

	// We want to rewrite as we go,
	// but there is no guarantee that the index is in sorted order.
	// TODO(maybe): Check for overlap before rewriting?
	slices.SortStableFunc(entries, func(ent1, ent2 macho.UniversalFileEntry) int {
		return cmp.Compare(ent1.Offset, ent2.Offset)
	})

	for len(entries) > 0 && int64(entries[0].Offset)+int64(entries[0].Size) <= srcSize {
		gap := int64(entries[0].Offset) - int64(*wc)
		if gap < 0 {
			// Skip any images that we've already advanced past.
			entries = entries[1:]
			continue
		}
		if gap > 0 {
			// Copy the gap bytes through verbatim.
			if err := caa.copyWithSelfReferences(sink, src, gap); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					err = nil
				}
				return err
			}
		}

		err := caa.zeroOutMachOCodeSignature(sink, int64(*wc), src, int64(entries[0].Size))
		if err != nil {
			return err
		}
		entries = entries[1:]
	}

	return caa.copyWithSelfReferences(sink, src, srcSize-int64(*wc))
}

const uuidSize = 16

// zeroOutMachOCodeSignature copies the data from src to dst,
// appending [SelfReferenceOffset] values to caa.rewrites as it goes.
// If and only if src can be parsed as a single-architecture Mach-O file
// containing self-references
// with an ad hoc code signature
// and at most one LC_UUID command,
// then zeroOutMachOCodeSignature will zero out the code signature's hash slots
// (and LC_UUID command, if present).
// and append a [*MachOSignatureRewrite] and possibly a [*MachOUUIDRewrite] to caa.rewrites.
// zeroOutMachOCodeSignature will only return an error if there is an I/O error.
//
// srcStart is the offset of the first byte that zeroOutMachOCodeSignature is processing
// relative to the beginning of the NAR file archive member.
func (caa *contentAddressAnalyzer) zeroOutMachOCodeSignature(dst io.Writer, srcStart int64, src *detect.HashModuloReader, srcSize int64) error {
	// Read Mach-O header.
	initialReferenceCount := src.ReferenceCount()
	limitedSource := io.LimitReader(src, srcSize)
	counter := new(xio.WriteCounter)
	header, err := macho.ReadFileHeader(io.TeeReader(limitedSource, io.MultiWriter(counter, dst)))
	headerEnd := int64(*counter)
	if err != nil {
		caa.log("Potential Mach-O file failed to parse: %v", err)
		caa.addNewSelfReferences(src, initialReferenceCount)
		return caa.copyWithSelfReferences(dst, src, srcSize-headerEnd)
	}

	// From here on out, we need buffering.
	// The LC_UUID command may need to be zeroed, so we need to do two passes.
	tempFile, err := caa.CreateTemp.CreateBuffer(srcSize - headerEnd)
	if err != nil {
		return fmt.Errorf("parse mach-o file: create temporary file: %v", err)
	}
	defer func() {
		if closeError := tempFile.Close(); closeError != nil {
			caa.log("Closing temporary file: %v", closeError)
		}
	}()
	abandonRewrite := func(bufferEnd int64) error {
		caa.addNewSelfReferences(src, initialReferenceCount)
		if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek in content address temporary file: %v", err)
		}
		_, err = io.CopyN(dst, tempFile, bufferEnd-headerEnd)
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return caa.copyWithSelfReferences(dst, src, srcSize-bufferEnd)
	}

	// Scan the command region for a potential code signature.
	commandReader := header.NewCommandReader(io.TeeReader(limitedSource, io.MultiWriter(counter, tempFile)))
	textSegment, signatureCommand, uuidOffset, scanError := scanMachOCommands(header.ByteOrder, commandReader)
	commandEnd := int64(*counter)
	var codeEnd int64
	if scanError == nil {
		codeEnd = int64(signatureCommand.DataOffset)
		switch {
		case codeEnd < commandEnd:
			scanError = fmt.Errorf("signature offset (%d) is before segments (%d)", codeEnd, commandEnd)
		case codeEnd > srcSize-macho.CodeSignatureBlobMinSize:
			scanError = fmt.Errorf("signature offset (%d) extends beyond file (%d)", codeEnd, srcSize)
		}
	}
	if scanError != nil {
		caa.log("Potential Mach-O file failed to parse: %v", scanError)
		return abandonRewrite(commandEnd)
	}

	// Copy all data until the beginning of the signature region.
	// If we don't have any self-references before the signature,
	// then we don't have to rewrite anything.
	if _, err := io.CopyN(tempFile, src, codeEnd-commandEnd); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		caa.addNewSelfReferences(src, initialReferenceCount)
		return err
	}
	if src.ReferenceCount() == initialReferenceCount {
		caa.log("Mach-O file did not contain self-references. No rewriting.")
		return abandonRewrite(int64(signatureCommand.DataOffset))
	}

	// Read signature blob.
	signatureRewrite, signatureEnd, signatureReadError := readCodeSignature(
		caa.fileStart+srcStart,
		signatureCommand.DataOffset,
		io.LimitReader(io.TeeReader(src, tempFile), srcSize-codeEnd),
		textSegment,
	)
	if signatureReadError != nil {
		caa.log("Potential Mach-O file failed to parse: code signature: %v", signatureReadError)
		return abandonRewrite(signatureEnd)
	}

	// Record rewrites and zero out bytes.
	hashSize := signatureRewrite.HashSize()
	caa.log("Mach-O signature rewrite for %v at NAR bytes [%#x, %#x)", header.CPUType, signatureRewrite.HashOffset, signatureRewrite.HashOffset+int64(hashSize))
	caa.addNewSelfReferences(src, initialReferenceCount)
	if uuidOffset != 0 {
		caa.rewrites = append(caa.rewrites, &MachOUUIDRewrite{
			ImageStart: signatureRewrite.ImageStart,
			CodeEnd:    signatureRewrite.CodeEnd,
			UUIDStart:  caa.fileStart + srcStart + headerEnd + uuidOffset,
		})
	}
	caa.rewrites = append(caa.rewrites, signatureRewrite)
	if uuidOffset != 0 {
		if err := clearAt(tempFile, uuidOffset, uuidSize); err != nil {
			return fmt.Errorf("zero out mach-o UUID in temporary file: %v", err)
		}
	}
	if err := clearAt(tempFile, signatureRewrite.HashOffset-(caa.fileStart+srcStart+headerEnd), hashSize); err != nil {
		return fmt.Errorf("zero out mach-o code signature in temporary file: %v", err)
	}

	// Copy bytes from tempFile (and any remaining from src) to dst.
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek in content address temporary file: %v", err)
	}
	if _, err := io.CopyN(dst, tempFile, signatureEnd-headerEnd); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if err := caa.copyWithSelfReferences(dst, src, srcSize-signatureEnd); err != nil {
		return err
	}
	return nil
}

func readCodeSignature(imageStart int64, signatureStart uint32, src io.Reader, textSegment *macho.SegmentCommand) (signatureRewrite *MachOSignatureRewrite, signatureEnd int64, err error) {
	signatureBytes, err := readCodeSignatureBlob(src)
	signatureEnd = int64(signatureStart) + int64(len(signatureBytes))
	if err != nil {
		return nil, signatureEnd, err
	}
	magic := macho.CodeSignatureMagic(binary.BigEndian.Uint32(signatureBytes))
	if magic != macho.CodeSignatureMagicEmbeddedSignature {
		return nil, signatureEnd, fmt.Errorf("code signature super blob type = %v", magic)
	}
	cd, err := findRewritableCodeDirectory(signatureBytes)
	if err != nil {
		return nil, signatureEnd, err
	}
	if cd.CodeLimit != uint64(signatureStart) {
		return nil, signatureEnd, fmt.Errorf("codeLimit (%#x) != code signature offset (%#x)", cd.CodeLimit, signatureStart)
	}
	// TODO(maybe): Validate __TEXT segment fields.
	signatureRewrite, err = cd.toRewrite(imageStart)
	if err != nil {
		return nil, signatureEnd, err
	}
	return signatureRewrite, signatureEnd, nil
}

// readCodeSignatureBlob reads the bytes of a serialized [macho.CodeSignatureBlob] from an [io.Reader].
// Partially read blobs will be returned.
func readCodeSignatureBlob(src io.Reader) ([]byte, error) {
	var header [macho.CodeSignatureBlobMinSize]byte
	if n, err := io.ReadFull(src, header[:]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return header[:n], err
	}
	size := binary.BigEndian.Uint32(header[4:])
	if size < macho.CodeSignatureBlobMinSize || size > maxMachOBufferSize {
		return header[:], fmt.Errorf("read mach-o code signature blob: invalid size %d", size)
	}

	blobBytes := make([]byte, size)
	copy(blobBytes, header[:])
	n, err := io.ReadFull(src, blobBytes[len(header):])
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return blobBytes[:len(header)+n], err
}

// scanMachOCommands searches the load commands necessary for code signing.
// If there is not exactly one __TEXT segment,
// exactly one LC_CODE_SIGNATURE command,
// and at most one LC_UUID command,
// then scanMachOCommands returns an error.
// If there is exactly one LC_UUID command, then uuidOffset will be a non-zero value
// with the offset in bytes from the start of the commands
// where the UUID bytes are stored.
func scanMachOCommands(byteOrder binary.ByteOrder, commands *macho.CommandReader) (textSegment *macho.SegmentCommand, signatureCommand *macho.LinkeditDataCommand, uuidOffset int64, err error) {
	const textSegmentName = "__TEXT"
	buf := make([]byte, macho.LoadCommandMinSize)
	pos := int64(0)
	for commands.Next() {
		buf = buf[:macho.LoadCommandMinSize]
		if _, err := io.ReadFull(commands, buf); err != nil {
			return nil, nil, 0, err
		}
		c, ok := commands.Command()
		if !ok {
			return nil, nil, 0, fmt.Errorf("internal error: missing command despite already reading bytes")
		}
		size, ok := commands.Size()
		if !ok {
			return nil, nil, 0, fmt.Errorf("invalid command size")
		}

		switch c {
		case macho.LoadCmdSegment, macho.LoadCmdSegment64:
			if size > maxMachOBufferSize {
				return nil, nil, 0, fmt.Errorf("%v command too large", c)
			}
			buf = slices.Grow(buf, int(size-macho.LoadCommandMinSize))
			buf = buf[:size]
			if _, err := io.ReadFull(commands, buf[macho.LoadCommandMinSize:]); err != nil {
				return nil, nil, 0, err
			}
			newSegment := new(macho.SegmentCommand)
			if err := newSegment.UnmarshalMachO(byteOrder, buf); err != nil {
				return nil, nil, 0, err
			}
			if newSegment.Name() == textSegmentName {
				if textSegment != nil {
					return nil, nil, 0, fmt.Errorf("multiple " + textSegmentName + " sections")
				}
				textSegment = newSegment
			}
		case macho.LoadCmdCodeSignature:
			if signatureCommand != nil {
				return nil, nil, 0, fmt.Errorf("multiple %v commands", c)
			}
			if size > maxMachOBufferSize {
				return nil, nil, 0, fmt.Errorf("command too large")
			}
			buf = slices.Grow(buf, int(size-macho.LoadCommandMinSize))
			buf = buf[:size]
			if _, err := io.ReadFull(commands, buf[macho.LoadCommandMinSize:]); err != nil {
				return nil, nil, 0, err
			}
			signatureCommand = new(macho.LinkeditDataCommand)
			if err := signatureCommand.UnmarshalMachO(byteOrder, buf); err != nil {
				return nil, nil, 0, err
			}
		case macho.LoadCmdUUID:
			if uuidOffset != 0 {
				return nil, nil, 0, fmt.Errorf("multiple %v commands", c)
			}
			if want := uint32(macho.LoadCommandMinSize + uuidSize); size != want {
				return nil, nil, 0, fmt.Errorf("wrong size for %v command (got %d, expected %d)", c, size, want)
			}
			uuidOffset = pos + 8
		}

		pos += int64(size)
	}
	if err := commands.Err(); err != nil {
		return nil, nil, 0, err
	}
	if textSegment == nil {
		return nil, nil, 0, fmt.Errorf("missing " + textSegmentName + " segment")
	}
	if signatureCommand == nil {
		return nil, nil, 0, fmt.Errorf("missing %v command", macho.LoadCmdCodeSignature)
	}
	return textSegment, signatureCommand, uuidOffset, nil
}

func (caa *contentAddressAnalyzer) copyWithSelfReferences(dst io.Writer, src *detect.HashModuloReader, n int64) error {
	initialCount := src.ReferenceCount()
	_, err := io.CopyN(dst, src, n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	caa.addNewSelfReferences(src, initialCount)
	return err
}

func (caa *contentAddressAnalyzer) addNewSelfReferences(src *detect.HashModuloReader, lastCount int) {
	caa.rewrites = slices.Grow(caa.rewrites, src.ReferenceCount()-lastCount)
	for off := range src.Offsets(lastCount) {
		caa.rewrites = append(caa.rewrites, SelfReferenceOffset(caa.fileStart+off))
	}
}

func (caa *contentAddressAnalyzer) log(format string, args ...any) {
	if caa != nil && caa.Log != nil {
		caa.Log(fmt.Sprintf(format, args...))
	}
}

// IsSourceContentAddress reports whether the given content address describes a "source" store object.
// "Source" store objects are those that are hashed by their NAR serialization
// and do not have a fixed (non-SHA-256) hash.
// This typically means source files imported using the "path" function,
// but can also mean content-addressed build artifacts.
func IsSourceContentAddress(ca nix.ContentAddress) bool {
	return ca.IsRecursiveFile() && ca.Hash().Type() == nix.SHA256
}

type contentAddressMethod int8

const (
	textIngestionMethod contentAddressMethod = 1 + iota
	flatFileIngestionMethod
	recursiveFileIngestionMethod
)

func methodOfContentAddress(ca nix.ContentAddress) contentAddressMethod {
	switch {
	case ca.IsText():
		return textIngestionMethod
	case ca.IsRecursiveFile():
		return recursiveFileIngestionMethod
	default:
		return flatFileIngestionMethod
	}
}

func (m contentAddressMethod) prefix() string {
	switch m {
	case textIngestionMethod:
		return "text:"
	case flatFileIngestionMethod:
		return ""
	case recursiveFileIngestionMethod:
		return "r:"
	default:
		panic("unknown content address method")
	}
}

func clearAt(w io.WriteSeeker, off int64, n int) error {
	if _, err := w.Seek(off, io.SeekStart); err != nil {
		return err
	}
	_, err := xio.WriteZero(w, int64(n))
	return err
}

func indexSeq(s, substr string) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := 0; ; {
			j := strings.Index(s[i:], substr)
			if j < 0 {
				break
			}
			if !yield(i + j) {
				break
			}
			i += j + len(substr)
		}
	}
}
