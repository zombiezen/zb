// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/hal"
	"zb.256lights.llc/pkg/internal/multierror"
	"zb.256lights.llc/pkg/internal/xurl"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// PutObjectRequest is holds the arguments to [*Store.PutObject].
type PutObjectRequest struct {
	// StorePath is the path of the store object.
	// It must not be empty.
	StorePath zbstore.Path
	// Reference is the set of references the store object has to other store objects.
	References sets.Sorted[zbstore.Path]
	// ContentAddress is the store object's content-addressability assertion.
	// It must not be zero.
	ContentAddress zbstore.ContentAddress
	// GetNAR returns a stream of the store object in NAR format.
	// It must not be nil.
	GetNAR func() (io.ReadCloser, error)
	// NARSize is the size of the NAR serialization of the store object in bytes.
	// If NARSize is non-positive,
	// then it will be computed from the number of bytes read from NAR.
	// Passing this will enable additional checks.
	NARSize int64
}

// PutObject uploads a store object to the store
// or does nothing if the object already exists in the store.
// PutObject first searches for an existing .narinfo file for the store path.
// If none is found, then two PUT requests are made:
// the first to upload the NAR file
// and the second to upload the .narinfo file.
// The object is verified during transit,
// so if after writing the NAR file the content does not match the trailer,
// then the .narinfo file is never uploaded.
func (s *Store) PutObject(ctx context.Context, req *PutObjectRequest) error {
	if req.StorePath == "" {
		return permanentError{fmt.Errorf("upload: path not set")}
	}
	if req.ContentAddress.IsZero() {
		return permanentError{fmt.Errorf("upload %s: content address not set", req.StorePath)}
	}

	hr, err := s.discover(ctx)
	if err != nil {
		return fmt.Errorf("upload %s: %w", req.StorePath, err)
	}
	putInfoURLs, err := s.findExistingInfoForPut(ctx, hr, req)
	if !errors.Is(err, zbstore.ErrNotFound) {
		var ec2 multierror.Collector
		for err := range multierror.All(err) {
			ec2.Add(fmt.Errorf("upload %s: %w", req.StorePath, err))
		}
		return ec2.Error()
	}

	narLink, hasNARLink := hr.Links[narRelation].Get()
	if !hasNARLink {
		return permanentError{fmt.Errorf("upload %s: %s: link relation %s: not found", req.StorePath, s.URL.Redacted(), narRelation)}
	}
	params := struct {
		Base   string
		Digest string
	}{
		Base:   req.StorePath.Base(),
		Digest: req.StorePath.Digest(),
	}
	narURL, err := narLink.Expand(params)
	if err != nil {
		return permanentError{fmt.Errorf("upload %s: %s: link relation %s: %v",
			req.StorePath, s.URL.Redacted(), narRelation, err)}
	}
	narURL, err = resolveReference(s.URL, narURL)
	if err != nil {
		return permanentError{fmt.Errorf("upload %s: %s: link relation %s: %v",
			req.StorePath, s.URL.Redacted(), narRelation, err)}
	}

	grp := &narBodyGroup{
		f: req.GetNAR,
		info: zbstore.ObjectInfo{
			StorePath:      req.StorePath,
			NARSize:        req.NARSize,
			References:     req.References,
			ContentAddress: req.ContentAddress,
		},
		createTemp: s.CreateTemp,
	}
	const cacheControl = "max-age=2592000" // 1 week
	uploadNARRequest := &putRequest{
		url:           narURL,
		origin:        s.URL,
		contentLength: -1,
		contentType:   nar.MIMEType,
		cacheControl:  cacheControl,
		getContent:    grp.new,
		// Replacement is fine, even if the contents differ.
		// We want PutObject to be idempotent, especially if a previous operation failed.
		// If there is a URL collision and multiple distinct .narinfo files referencing it,
		// then the other ones will detect the differing content address.
		noReplace: false,
	}
	if grp.info.HasNARSize() {
		uploadNARRequest.contentLength = grp.info.NARSize
	}
	uploadNARError := put(ctx, s.client(), uploadNARRequest)
	copyResult, copyError := grp.wait()
	if uploadNARError != nil {
		err := fmt.Errorf("upload %s: %v", req.StorePath, uploadNARError)
		if isClientError(uploadNARError) {
			err = permanentError{err}
		}
		return err
	}
	if copyError != nil {
		return fmt.Errorf("upload %s: %v", req.StorePath, copyError)
	}

	var ec multierror.Collector
	for _, u := range putInfoURLs {
		relNARURL, err := xurl.Rel(u.url, narURL)
		if err != nil {
			ec.Add(err)
			continue
		}
		narinfoData, err := (&NARInfo{
			StorePath:   req.StorePath,
			References:  req.References,
			URL:         relNARURL.String(),
			Compression: NoCompression,
			CA:          req.ContentAddress,
			NARHash:     copyResult.narHash,
			NARSize:     copyResult.narSize,
		}).MarshalText()
		if err != nil {
			ec.Add(err)
			continue
		}
		uploadError := put(ctx, s.client(), &putRequest{
			url:    u.url,
			origin: s.URL,
			getContent: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(narinfoData)), nil
			},
			contentLength:  int64(len(narinfoData)),
			contentType:    NARInfoMIMEType,
			acceptEncoding: u.acceptEncoding,
			cacheControl:   cacheControl,
			noReplace:      true,
		})
		if uploadError == nil {
			if err := ec.Error(); err != nil {
				log.Warnf(ctx, "While uploading %s: %v", req.StorePath, err)
			}
			return nil
		}
		if isClientError(uploadError) {
			ec.Add(permanentError{uploadError})
		} else {
			ec.Add(uploadError)
			break
		}
	}

	var ec2 multierror.Collector
	for err := range multierror.All(ec.Error()) {
		ec2.Add(fmt.Errorf("upload %s: %w", req.StorePath, err))
	}
	return ec2.Error()
}

type urlRequestNegotiation struct {
	url *url.URL
	requestNegotiation
}

// findExistingInfoForPut checks whether there is an existing .narinfo file
// that is compatible with the [*PutObjectRequest].
// findExistingInfoForPut returns [zbstore.ErrNotFound] if the store does not have such a file.
// On any error, findExistingInfoForPut will return a list of .narinfo URLs that seem to accept PUTs.
func (s *Store) findExistingInfoForPut(ctx context.Context, discoveryDocument *hal.Resource, req *PutObjectRequest) (putURLs []*urlRequestNegotiation, err error) {
	var ec multierror.Collector
	hasInfoURLs := false
	for u := range s.narInfoURLs(&ec, discoveryDocument, req.StorePath) {
		hasInfoURLs = true
		remoteInfo, rneg, fetchError := s.fetchNARInfo(ctx, u)
		if fetchError == nil {
			if !ensureInfoMatches(&ec, req, u, remoteInfo) {
				if len(putURLs) == 0 {
					break
				}
				log.Warnf(ctx, "Found conflicting %s at %s, but have %d other URL(s) that are higher priority. While searching: %v",
					req.StorePath, u.Redacted(), len(putURLs), ec.Error())
				return putURLs, zbstore.ErrNotFound
			}
			if err := ec.Error(); err != nil {
				log.Warnf(ctx, "Found existing %s at %s. Skipping upload. While searching: %v",
					req.StorePath, u.Redacted(), err)
			} else {
				log.Debugf(ctx, "Found existing %s at %s. Skipping upload.", req.StorePath, u.Redacted())
			}
			return nil, nil
		}
		if rneg.isMethodAllowed(http.MethodPut) {
			putURLs = append(putURLs, &urlRequestNegotiation{
				url:                u,
				requestNegotiation: *rneg,
			})
		}
		if isNotFound(fetchError) {
			log.Debugf(ctx, "While uploading %s, as expected: %v", req.StorePath, fetchError)
		} else {
			ec.Add(fetchError)
		}
	}
	if !hasInfoURLs {
		ec.Add(permanentError{fmt.Errorf("%s: missing valid %s link", s.URL.Redacted(), narInfoRelation)})
	} else if len(putURLs) == 0 {
		ec.Add(permanentError{fmt.Errorf("%s: %s links do not permit %s", s.URL.Redacted(), narInfoRelation, http.MethodPut)})
	}
	if err := ec.Error(); err != nil {
		return putURLs, err
	}
	return putURLs, zbstore.ErrNotFound
}

// ensureInfoMatches reports whether the remote [NARInfo]
// matches an object we're about to upload.
// If it does not, then errors will be added to the [multierror.Collector].
func ensureInfoMatches(ec *multierror.Collector, req *PutObjectRequest, u *url.URL, remoteInfo *NARInfo) bool {
	matches := true
	if remoteInfo.StorePath != req.StorePath {
		ec.Add(permanentError{fmt.Errorf("%s: mismatched store path %s", u.Redacted(), remoteInfo.StorePath)})
		matches = false
	}
	if remoteInfo.CA.IsZero() {
		ec.Add(permanentError{fmt.Errorf("%s: missing content address", u.Redacted())})
		matches = false
	} else if !remoteInfo.CA.Equal(req.ContentAddress) {
		ec.Add(permanentError{fmt.Errorf("%s: content address = %v; expecting %v",
			u.Redacted(), remoteInfo.CA, req.ContentAddress)})
		matches = false
	}
	if req.NARSize > 0 && remoteInfo.NARSize != req.NARSize {
		ec.Add(permanentError{fmt.Errorf("%s: nar size = %d bytes; expecting %d bytes",
			u.Redacted(), remoteInfo.NARSize, req.NARSize)})
		matches = false
	}
	return matches
}

// A narBodyGroup is a collection of [*narBody] objects
// that share the same content
// and attempt to converge on a successful [*narCopyResult].
type narBodyGroup struct {
	f          func() (io.ReadCloser, error)
	info       zbstore.ObjectInfo
	createTemp bytebuffer.Creator

	mu     sync.Mutex
	cond   sync.Cond
	open   int
	result *narCopyResult
}

// A narCopyResult is the output of a read from a [*narBody] until its closing.
type narCopyResult struct {
	narSize     int64
	narHash     nix.Hash
	verifyError error
}

func (grp *narBodyGroup) init() {
	if grp.cond.L == nil {
		grp.cond.L = &grp.mu
	}
}

// new returns a new [*narBody] attached to the group.
// The caller is responsible for calling Close on the returned [io.ReadCloser].
func (grp *narBodyGroup) new() (io.ReadCloser, error) {
	nar, err := grp.f()
	if err != nil {
		return nil, err
	}

	grp.mu.Lock()
	grp.init()
	grp.open++
	grp.mu.Unlock()

	verifyWriter := make(chan io.Writer)
	verifyWriteDone := make(chan error)
	verifyDone := make(chan struct{})

	body := &narBody{
		group:           grp,
		nar:             nar,
		narHasher:       *nix.NewHasher(nix.SHA256),
		verifyWriteDone: verifyWriteDone,
		verifyDone:      verifyDone,
	}
	go func() {
		defer close(verifyDone)
		obj := &fakeObject{
			info:      grp.info,
			writer:    verifyWriter,
			writeDone: verifyWriteDone,
		}
		_, body.verifyError = zbstore.VerifyObject(context.Background(), io.Discard, obj, &zbstore.ContentAddressOptions{
			CreateTemp: grp.createTemp,
		})
	}()

	select {
	case body.verifyWriter = <-verifyWriter:
	case <-body.verifyDone:
	}
	return body, nil
}

// wait pauses the goroutine until all [*narBody] objects created by [*narBodyGroup.new] are closed
// and returns the first successful [*narCopyResult],
// or an error if no such result exists.
func (grp *narBodyGroup) wait() (*narCopyResult, error) {
	grp.mu.Lock()
	grp.init()
	for grp.open > 0 {
		grp.cond.Wait()
	}
	defer grp.mu.Unlock()

	if grp.result == nil {
		return nil, fmt.Errorf("internal error: nar never copied")
	}
	return grp.result, nil
}

// narBody is an [io.ReadCloser] that wraps another [io.ReadCloser]
// to collect the file size, compute the file hash, and verify the NAR data as a store object
// as the data is read.
// A narBody is part of a [*narBodyGroup] where it publishes its results
// after [*narBody.Close] is called.
type narBody struct {
	group     *narBodyGroup
	nar       io.ReadCloser
	readError error
	narSize   int64
	narHasher nix.Hasher

	verifyWriter    io.Writer
	verifyWriteDone chan<- error

	verifyDone  <-chan struct{}
	verifyError error
}

func (body *narBody) Read(p []byte) (int, error) {
	if body.readError != nil {
		return 0, body.readError
	}
	if len(p) == 0 {
		return 0, nil
	}

	var remaining int64 = -1
	if body.group.info.HasNARSize() {
		remaining = body.group.info.NARSize - body.narSize
		if remaining < 0 {
			// Defensive programming: we already read past the expected end.
			// Shouldn't hit this case.
			body.readError = errNARTooLarge
			return 0, body.readError
		}
		// We need to read at most 1 byte past the expected end
		// in order to detect [errNARTooLarge].
		if int64(len(p)) > remaining {
			p = p[:remaining+1]
		}
	}
	var n int
	n, body.readError = body.nar.Read(p)
	if body.group.info.HasNARSize() {
		if int64(n) > remaining {
			n = int(remaining)
			body.readError = errNARTooLarge
		} else if body.readError == io.EOF && int64(n) < remaining {
			body.readError = io.ErrUnexpectedEOF
		}
	}

	body.narSize += int64(n)
	body.narHasher.Write(p[:n])
	if body.verifyWriter != nil {
		body.verifyWriter.Write(p[:n])
	}
	return n, body.readError
}

var errNARTooLarge = errors.New("nar too large")

func (body *narBody) Close() error {
	err := body.nar.Close()

	if body.group.info.HasNARSize() && body.narSize < body.group.info.NARSize {
		body.verifyWriteDone <- errors.New("nar content closed early")
	} else {
		body.verifyWriteDone <- nil
	}
	<-body.verifyDone
	if body.verifyError == nil && body.group.info.HasNARSize() && body.narSize != body.group.info.NARSize {
		body.verifyError = fmt.Errorf("nar size = %d bytes (advertised %d bytes)", body.narSize, body.group.info.NARSize)
	}

	body.group.mu.Lock()
	if body.group.result == nil || body.narSize > body.group.result.narSize ||
		body.narSize == body.group.result.narSize && body.verifyError == nil {
		body.group.result = &narCopyResult{
			narSize:     body.narSize,
			narHash:     body.narHasher.SumHash(),
			verifyError: body.verifyError,
		}
	}
	body.group.open--
	if body.group.open <= 0 {
		body.group.cond.Broadcast()
	}
	body.group.mu.Unlock()

	return err
}

type fakeObject struct {
	info      zbstore.ObjectInfo
	writer    chan<- io.Writer
	writeDone <-chan error
}

func (obj *fakeObject) Info() *zbstore.ObjectInfo {
	return &obj.info
}

func (obj *fakeObject) WriteNAR(ctx context.Context, dst io.Writer) error {
	if obj.writer == nil {
		return fmt.Errorf("already written")
	}
	obj.writer <- dst
	obj.writer = nil
	return <-obj.writeDone
}
