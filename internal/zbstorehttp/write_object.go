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
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// WriteObject uploads a store object to the store
// or does nothing if the object already exists in the store.
// WriteObject first searches for an existing .narinfo file for the store path.
// If none is found, then two PUT requests are made:
// the first to upload the NAR file
// and the second to upload the .narinfo file.
// The object is verified during transit,
// so if after writing the NAR file the content does not match the trailer,
// then the .narinfo file is never uploaded.
func (s *Store) WriteObject(ctx context.Context, obj zbstore.Object) error {
	info := obj.Info()
	if info.StorePath == "" {
		return fmt.Errorf("upload: path not set")
	}
	if info.ContentAddress.IsZero() {
		return fmt.Errorf("upload %s: content address not set", info.StorePath)
	}

	return retry(ctx, "upload "+string(info.StorePath), func() error {
		hr, err := s.discover(ctx)
		if err != nil {
			return err
		}
		putInfoURLs, err := s.findExistingInfoForPut(ctx, hr, info)
		if !errors.Is(err, zbstore.ErrNotFound) {
			return err
		}

		narLink, hasNARLink := hr.Links[narRelation].Get()
		if !hasNARLink {
			return &url.Error{
				Op:  "discover",
				URL: s.URL.Redacted(),
				Err: fmt.Errorf("link relation %s: not found", narRelation),
			}
		}
		params := struct {
			Base   string
			Digest string
		}{
			Base:   info.StorePath.Base(),
			Digest: info.StorePath.Digest(),
		}
		narURL, err := narLink.Expand(params)
		if err != nil {
			return &url.Error{
				Op:  "discover",
				URL: s.URL.Redacted(),
				Err: fmt.Errorf("link relation %s: %v", narRelation, err),
			}
		}
		narURL, err = resolveReference(s.URL, narURL)
		if err != nil {
			return &url.Error{
				Op:  "discover",
				URL: s.URL.Redacted(),
				Err: fmt.Errorf("link relation %s: %v", narRelation, err),
			}
		}

		log.Infof(ctx, "Uploading %s to %s...", info.StorePath, narURL.Redacted())
		grp := &narBodyGroup{
			object:     obj,
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
		if info.HasNARSize() {
			uploadNARRequest.contentLength = info.NARSize
		}
		uploadNARError := put(ctx, s.client(), uploadNARRequest)
		copyResult, copyError := grp.wait()
		if uploadNARError != nil {
			if !isClientError(uploadNARError) {
				uploadNARError = temporaryError{uploadNARError}
			}
			return uploadNARError
		}
		if copyError != nil {
			return copyError
		}

		var ec multierror.Collector
		for _, u := range putInfoURLs {
			relNARURL, err := xurl.Rel(u.url, narURL)
			if err != nil {
				ec.Add(err)
				continue
			}
			narinfoData, err := (&NARInfo{
				StorePath:   info.StorePath,
				References:  info.References,
				URL:         relNARURL.String(),
				Compression: NoCompression,
				CA:          info.ContentAddress,
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
			switch {
			case uploadError == nil:
				if err := ec.Error(); err != nil {
					log.Warnf(ctx, "While uploading %s: %v", info.StorePath, err)
				}
				return nil
			case isClientError(uploadError):
				ec.Add(uploadError)
			default:
				ec.Add(temporaryError{uploadError})
				return ec.Error()
			}
		}
		return ec.Error()
	})
}

type urlRequestNegotiation struct {
	url *url.URL
	requestNegotiation
}

// findExistingInfoForPut checks whether there is an existing .narinfo file
// that is compatible with the [*PutObjectRequest].
// findExistingInfoForPut returns [zbstore.ErrNotFound] if the store does not have such a file.
// On any error, findExistingInfoForPut will return a list of .narinfo URLs that seem to accept PUTs.
func (s *Store) findExistingInfoForPut(ctx context.Context, discoveryDocument *hal.Resource, info *zbstore.ObjectInfo) (putURLs []*urlRequestNegotiation, err error) {
	var ec multierror.Collector
	hasInfoURLs := false
	for u := range s.narInfoURLs(&ec, discoveryDocument, info.StorePath) {
		hasInfoURLs = true
		remoteInfo, rneg, fetchError := s.fetchNARInfo(ctx, u)
		if fetchError == nil {
			if !ensureInfoMatches(&ec, info, u, remoteInfo) {
				if len(putURLs) == 0 {
					break
				}
				log.Warnf(ctx, "Found conflicting %s at %s, but have %d other URL(s) that are higher priority. While searching: %v",
					info.StorePath, u.Redacted(), len(putURLs), ec.Error())
				return putURLs, zbstore.ErrNotFound
			}
			if err := ec.Error(); err != nil {
				log.Warnf(ctx, "Found existing %s at %s. Skipping upload. While searching: %v",
					info.StorePath, u.Redacted(), err)
			} else {
				log.Debugf(ctx, "Found existing %s at %s. Skipping upload.", info.StorePath, u.Redacted())
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
			log.Debugf(ctx, "While uploading %s, as expected: %v", info.StorePath, fetchError)
		} else {
			ec.Add(fetchError)
		}
	}
	if !hasInfoURLs {
		ec.Add(&url.Error{
			Op:  "discover",
			URL: s.URL.Redacted(),
			Err: fmt.Errorf("missing valid %s link", narInfoRelation),
		})
	} else if len(putURLs) == 0 {
		ec.Add(&url.Error{
			Op:  "discover",
			URL: s.URL.Redacted(),
			Err: fmt.Errorf("%s links do not permit %s", narInfoRelation, http.MethodPut),
		})
	}
	if err := ec.Error(); err != nil {
		return putURLs, err
	}
	return putURLs, zbstore.ErrNotFound
}

// ensureInfoMatches reports whether the remote [NARInfo]
// matches an object we're about to upload.
// If it does not, then errors will be added to the [multierror.Collector].
func ensureInfoMatches(ec *multierror.Collector, info *zbstore.ObjectInfo, u *url.URL, remoteInfo *NARInfo) bool {
	matches := true
	if remoteInfo.StorePath != info.StorePath {
		ec.Add(&url.Error{
			Op:  "read",
			URL: u.Redacted(),
			Err: fmt.Errorf("mismatched store path %s", remoteInfo.StorePath),
		})
		matches = false
	}
	if remoteInfo.CA.IsZero() {
		ec.Add(&url.Error{
			Op:  "read",
			URL: u.Redacted(),
			Err: fmt.Errorf("missing content address"),
		})
		matches = false
	} else if !remoteInfo.CA.Equal(info.ContentAddress) {
		ec.Add(&url.Error{
			Op:  "read",
			URL: u.Redacted(),
			Err: fmt.Errorf("content address = %v; expecting %v", remoteInfo.CA, info.ContentAddress),
		})
		matches = false
	}
	if info.HasNARSize() && remoteInfo.NARSize != info.NARSize {
		ec.Add(&url.Error{
			Op:  "read",
			URL: u.Redacted(),
			Err: fmt.Errorf("nar size = %d bytes; expecting %d bytes", remoteInfo.NARSize, info.NARSize),
		})
		matches = false
	}
	return matches
}

// A narBodyGroup is a collection of [*narBody] objects
// that share the same content
// and attempt to converge on a successful [*narCopyResult].
type narBodyGroup struct {
	object     zbstore.Object
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
	grp.mu.Lock()
	grp.init()
	grp.open++
	grp.mu.Unlock()

	pr, pw := io.Pipe()
	verifyDone := make(chan struct{})
	body := &narBody{
		group:      grp,
		nar:        pr,
		narHasher:  *nix.NewHasher(nix.SHA256),
		verifyDone: verifyDone,
	}
	go func() {
		defer close(verifyDone)
		_, body.verifyError = zbstore.VerifyObject(context.Background(), pw, grp.object, &zbstore.ContentAddressOptions{
			CreateTemp: grp.createTemp,
		})
		pw.CloseWithError(body.verifyError)
	}()
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
	nar       *io.PipeReader
	readError error
	narSize   int64
	narHasher nix.Hasher

	verifyDone  <-chan struct{}
	verifyError error
}

func (body *narBody) Read(p []byte) (n int, err error) {
	if body.readError != nil {
		return 0, body.readError
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, body.readError = body.nar.Read(p)
	body.narSize += int64(n)
	body.narHasher.Write(p[:n])
	return n, body.readError
}

func (body *narBody) Close() error {
	err := body.nar.Close()
	<-body.verifyDone

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
