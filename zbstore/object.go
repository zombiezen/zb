// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"context"
	"fmt"
	"io"

	"zb.256lights.llc/pkg/internal/detect"
	"zb.256lights.llc/pkg/internal/xio"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// An Object represents a handle to a zb store object.
// All methods on Object must be safe to call concurrently from multiple goroutines.
type Object interface {
	// WriteNAR writes the NAR serialization of the store object to w.
	WriteNAR(ctx context.Context, dst io.Writer) error
	// Info returns the metadata of the object.
	// The caller must not modify any fields in the returned [*ObjectInfo].
	Info() *ObjectInfo
}

// Blob is an in-memory implementation of the [Object] interface.
type Blob struct {
	// NAR is the blob's NAR serialization.
	NAR []byte
	// NARHash is an optional hash of NAR.
	NARHash nix.Hash
	// ExportTrailer is the blob's metadata.
	// Deriver is ignored.
	ExportTrailer
}

// WriteNAR writes blob.NAR to w.
func (blob *Blob) WriteNAR(ctx context.Context, dst io.Writer) error {
	_, err := dst.Write(blob.NAR)
	return err
}

// Info returns converts blob.ExportTrailer to [*ObjectInfo].
func (blob *Blob) Info() *ObjectInfo {
	return &ObjectInfo{
		StorePath:      blob.StorePath,
		NARSize:        int64(len(blob.NAR)),
		NARHash:        blob.NARHash,
		References:     blob.References,
		ContentAddress: blob.ContentAddress,
	}
}

// VerifyObject copies a store object's content to an [io.Writer],
// returning an error if the content does not match the [*ObjectInfo].
// opts.Digest is ignored: obj.Info().StorePath.Digest() will always be used.
// VerifyObject will call obj.WriteNAR at most once.
func VerifyObject(ctx context.Context, dst io.Writer, obj Object, opts *ContentAddressOptions) (size int64, err error) {
	info := obj.Info()
	defer func(path Path) {
		if err != nil {
			err = fmt.Errorf("verify %s: %v", path, err)
		}
	}(info.StorePath)

	writers := make([]io.Writer, 0, 6)
	closers := make([]io.Closer, 0, 5)
	nsv := newNARSyntaxVerifier()
	writers = append(writers, nsv)
	closers = append(closers, nsv)
	cav, err := newContentAddressVerifier(info.ContentAddress, info.StorePath, &info.References, opts)
	if err != nil {
		return 0, err
	}
	writers = append(writers, cav)
	closers = append(closers, cav)
	if ht := info.NARHash.Type(); ht.IsValid() {
		v := narHashVerifier{
			Hasher: nix.NewHasher(ht),
			want:   info.NARHash,
		}
		writers = append(writers, v)
		closers = append(closers, v)
	}
	if info.References.Len() > 0 {
		v := newReferencesVerifier(&info.References)
		writers = append(writers, v)
		closers = append(closers, v)
	}
	wc := &writeCounter{w: dst}
	if info.HasNARSize() {
		v := &narSizeVerifier{want: info.NARSize}
		writers = append(writers, v, newLimitedWriter(wc, info.NARSize))
		closers = append(closers, v)
	} else {
		writers = append(writers, wc)
	}

	firstError := obj.WriteNAR(ctx, io.MultiWriter(writers...))
	for _, c := range closers {
		if err := c.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return wc.n, firstError
}

type contentAddressResult struct {
	contentAddress ContentAddress
	err            error
}

type contentAddressVerifier struct {
	w    *io.PipeWriter
	refs References
	ch   <-chan contentAddressResult

	wantContentAddress ContentAddress
	wantPath           Path
}

func newContentAddressVerifier(wantContentAddress ContentAddress, wantPath Path, refs *sets.Sorted[Path], opts *ContentAddressOptions) (*contentAddressVerifier, error) {
	storeRefs := MakeReferences(wantPath, refs)
	if err := ValidateContentAddress(wantContentAddress, storeRefs); err != nil {
		return nil, err
	}

	var f func(r io.Reader, ht nix.HashType, isText bool) (ContentAddress, error)
	switch {
	case IsSourceContentAddress(wantContentAddress) && wantContentAddress.Hash().Type() == nix.SHA256:
		var digest string
		if storeRefs.Self {
			digest = wantPath.Digest()
		}
		opts = contentAddressOptionsWithDigest(opts, digest)
		f = func(r io.Reader, _ nix.HashType, _ bool) (ContentAddress, error) {
			ca, _, err := SourceSHA256ContentAddress(r, opts)
			return ca, err
		}
	case IsSourceContentAddress(wantContentAddress):
		// Future-proofing in case we add new algorithms but don't update backends.
		return nil, fmt.Errorf("unsupported source content address %v", wantContentAddress.Hash().Type())
	case wantContentAddress.IsRecursiveFile():
		f = computeRecursiveFileContentAddress
	default:
		f = computeFlatFileContentAddress
	}

	pr, pw := io.Pipe()
	done := make(chan contentAddressResult)
	go func() {
		defer pr.Close()
		ht := wantContentAddress.Hash().Type()
		isText := wantContentAddress.IsText()
		var result contentAddressResult
		result.contentAddress, result.err = f(pr, ht, isText)
		done <- result
	}()

	return &contentAddressVerifier{
		w:                  pw,
		refs:               storeRefs,
		ch:                 done,
		wantContentAddress: wantContentAddress,
		wantPath:           wantPath,
	}, nil
}

func computeRecursiveFileContentAddress(r io.Reader, ht nix.HashType, _ bool) (ContentAddress, error) {
	h := nix.NewHasher(ht)
	if _, err := io.Copy(h, r); err != nil {
		return ContentAddress{}, err
	}
	return nix.RecursiveFileContentAddress(h.SumHash()), nil
}

func computeFlatFileContentAddress(r io.Reader, ht nix.HashType, isText bool) (ContentAddress, error) {
	nr := nar.NewReader(r)
	hdr, err := nr.Next()
	if err != nil {
		return ContentAddress{}, err
	}
	if !hdr.Mode.IsRegular() {
		return ContentAddress{}, fmt.Errorf("not a flat file")
	}
	if hdr.Mode&0o111 != 0 {
		return ContentAddress{}, fmt.Errorf("must not be executable")
	}
	h := nix.NewHasher(ht)
	if _, err := io.Copy(h, nr); err != nil {
		return ContentAddress{}, err
	}
	var computed ContentAddress
	if isText {
		computed = nix.TextContentAddress(h.SumHash())
	} else {
		computed = nix.FlatFileContentAddress(h.SumHash())
	}
	if _, err := nr.Next(); err == nil {
		return ContentAddress{}, fmt.Errorf("more than a single file (bug in NAR reader?)")
	} else if err != io.EOF {
		return ContentAddress{}, err
	}
	return computed, nil
}

func (v *contentAddressVerifier) Write(p []byte) (n int, err error) {
	v.w.Write(p)
	// Even if the pipe is closed, report successful.
	return len(p), nil
}

func (v *contentAddressVerifier) Close() error {
	v.w.Close()
	result := <-v.ch
	if result.err != nil {
		return result.err
	}
	if !v.wantContentAddress.Equal(result.contentAddress) {
		return fmt.Errorf("%v does not match content (computed %v)", v.wantContentAddress, result.contentAddress)
	}
	dir := v.wantPath.Dir()
	name := v.wantPath.Name()
	gotPath, err := FixedCAOutputPath(dir, name, result.contentAddress, v.refs)
	if err != nil {
		return err
	}
	if gotPath != v.wantPath {
		return fmt.Errorf("does not match computed path %s", gotPath)
	}
	return nil
}

type narSizeVerifier struct {
	xio.WriteCounter
	want int64
}

func (v *narSizeVerifier) Close() error {
	if int64(v.WriteCounter) != v.want {
		return fmt.Errorf("nar size = %d bytes (expected %d bytes)", v.WriteCounter, v.want)
	}
	return nil
}

// writeCounter records the number of bytes that have successfully been written to w.
type writeCounter struct {
	w io.Writer
	n int64
}

func (wc *writeCounter) Write(p []byte) (n int, err error) {
	n, err = wc.w.Write(p)
	wc.n += int64(n)
	return n, err
}

type narHashVerifier struct {
	*nix.Hasher
	want nix.Hash
}

func (v narHashVerifier) Close() error {
	got := v.SumHash()
	if !got.Equal(v.want) {
		return fmt.Errorf("%v does not match content (computed %v)", v.want, got)
	}
	return nil
}

type referencesVerifier struct {
	*detect.RefFinder
	want *sets.Sorted[Path]
}

func newReferencesVerifier(want *sets.Sorted[Path]) referencesVerifier {
	return referencesVerifier{
		want: want,
		RefFinder: detect.NewRefFinder(func(yield func(string) bool) {
			for ref := range want.Values() {
				if !yield(ref.Digest()) {
					return
				}
			}
		}),
	}
}

func (v referencesVerifier) Close() error {
	found := v.Found()
	for i, want := range v.want.All() {
		if i >= found.Len() || want.Digest() != found.At(i) {
			return fmt.Errorf("content does not reference %s", want)
		}
	}
	return nil
}

type narSyntaxVerifier struct {
	*io.PipeWriter
	done <-chan error
}

func newNARSyntaxVerifier() narSyntaxVerifier {
	pr, pw := io.Pipe()
	done := make(chan error)
	go func() {
		defer close(done)
		nr := nar.NewReader(pr)
		for {
			if _, err := nr.Next(); err != nil {
				if err == io.EOF {
					pr.Close()
					// If we received [io.EOF], then [narSyntaxVerifier.Close] was called.
					// It's a valid NAR, since [nar.Reader] checks to the end.
					done <- nil
				} else {
					pr.CloseWithError(err)
					done <- err
				}
				return
			}
		}
	}()

	return narSyntaxVerifier{PipeWriter: pw, done: done}
}

func (v narSyntaxVerifier) Close() error {
	v.PipeWriter.Close()
	err := <-v.done
	return err
}

type limitedWriter struct {
	w     io.Writer
	limit int64 // max bytes initially (for error message)
	n     int64 // max bytes remaining
	err   error
}

func newLimitedWriter(w io.Writer, limit int64) *limitedWriter {
	return &limitedWriter{
		w:     w,
		limit: limit,
		n:     limit,
	}
}

func (l *limitedWriter) Write(p []byte) (n int, err error) {
	if len(p) == 0 || l.err != nil {
		return 0, l.err
	}
	overflow := int64(len(p)) > l.n
	if l.n > 0 {
		if overflow {
			p = p[:l.n]
		}
		n, l.err = l.w.Write(p)
		l.n -= int64(n)
	}
	if l.err == nil && overflow {
		l.err = fmt.Errorf("nar content too large (>%d bytes)", l.limit)
	}
	return n, l.err
}

func contentAddressOptionsWithDigest(opts *ContentAddressOptions, wantDigest string) *ContentAddressOptions {
	if opts == nil && wantDigest == "" || opts != nil && opts.Digest == wantDigest {
		return opts
	}
	if opts == nil {
		opts = new(ContentAddressOptions)
	} else {
		opts = new(*opts)
	}
	opts.Digest = wantDigest
	return opts
}
