// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

// Package zbstorehttp provides a client of the [zb binary cache protocol].
//
// [zb binary cache protocol]: https://zb.256lights.llc/binary-cache/
package zbstorehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/hal"
	"zb.256lights.llc/pkg/internal/httpencoding"
	"zb.256lights.llc/pkg/internal/multierror"
	"zb.256lights.llc/pkg/internal/xhttp"
	"zb.256lights.llc/pkg/internal/xtime"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
)

var _ interface {
	zbstore.Store
	zbstore.RealizationFetcher
} = (*Store)(nil)

const (
	narRelation         = "https://zb-build.dev/api/rel/nar"
	narInfoRelation     = "https://zb-build.dev/api/rel/narinfo"
	realizationRelation = "https://zb-build.dev/api/rel/realization"
)

// A Store implements [zbstore.Store] and [zbstore.RealizationFetcher]
// using the [Binary Cache Protocol].
//
// [Binary Cache Protocol]: https://zb.256lights.llc/binary-cache/
type Store struct {
	// URL is the URL of the binary cache discovery document.
	// This must be non-nil or the store's methods will return errors.
	URL *url.URL
	// Store methods use HTTPClient to make HTTP requests.
	// It is recommended to use a client that performs caching.
	// If HTTPClient is nil, then [http.DefaultClient] is used.
	HTTPClient Client
	// CreateTemp is called to create temporary storage for uploading.
	// If CreateTemp is nil, uploads will store NAR files in memory.
	// This is generally not recommended, as the files can be large.
	CreateTemp bytebuffer.Creator
	// RealizationsCacheControl is the Cache-Control header value to use
	// when uploading a realizations document.
	RealizationsCacheControl string
}

func (s *Store) client() Client {
	if s.HTTPClient == nil {
		return http.DefaultClient
	}
	return s.HTTPClient
}

func (s *Store) discover(ctx context.Context) (*hal.Resource, error) {
	if s.URL == nil {
		return nil, fmt.Errorf("get discovery document: url missing")
	}

	res, err := fetch(ctx, s.client(), &fetchRequest{
		url:    s.URL,
		accept: "application/hal+json,application/json;q=0.9,text/*;q=0.8,*/*;q=0.7",
	})
	if err != nil {
		if isClientError(err) {
			return nil, fmt.Errorf("get discovery document: %v", err)
		} else {
			return nil, temporaryError{fmt.Errorf("get discovery document: %v", err)}
		}
	}
	hr := new(hal.Resource)
	if err := jsonv2.Unmarshal(res.body, hr); err != nil {
		return nil, fmt.Errorf("get discovery document: %v", &url.Error{
			Op:  "parse",
			URL: s.URL.Redacted(),
			Err: err,
		})
	}
	return hr, nil
}

// Object fetches the .narinfo resource for the store object at the given path.
func (s *Store) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	var object zbstore.Object
	err := retry(ctx, "stat "+string(path), func() error {
		hr, err := s.discover(ctx)
		if err != nil {
			return err
		}
		var ec multierror.Collector
		for u := range s.narInfoURLs(&ec, hr, path) {
			info, _, err := s.fetchNARInfo(ctx, u)
			if err == nil {
				object = &httpObject{
					base:   u,
					client: s.client(),
					info:   info,
				}
				return nil
			}
			if isNotFound(err) {
				log.Debugf(ctx, "NAR info not found: %v", err)
			} else {
				ec.Add(err)
			}
		}
		err = ec.Error()
		if err == nil {
			err = zbstore.ErrNotFound
		}
		return err
	})
	return object, err
}

func (s *Store) narInfoURLs(ec *multierror.Collector, discoveryDocument *hal.Resource, path zbstore.Path) iter.Seq[*url.URL] {
	return s.expandLinks(ec, discoveryDocument, narInfoRelation, struct {
		Base   string
		Digest string
	}{
		Base:   path.Base(),
		Digest: path.Digest(),
	})
}

func (s *Store) fetchNARInfo(ctx context.Context, u *url.URL) (info *NARInfo, rneg *requestNegotiation, err error) {
	res, err := fetch(ctx, s.client(), &fetchRequest{
		url:    u,
		accept: "text/x-nix-narinfo,text/*;q=0.9,*/*;q=0.8",
		origin: s.URL,
	})
	rneg = requestNegotiationFromFetchResponse(res, err)
	if err != nil {
		if !isClientError(err) {
			err = temporaryError{err}
		}
		return nil, rneg, err
	}
	result := new(NARInfo)
	if err := result.UnmarshalText(res.body); err != nil {
		return nil, rneg, &url.Error{
			Op:  "get",
			URL: u.Redacted(),
			Err: err,
		}
	}
	return result, rneg, nil
}

// FetchRealizations implements [zbstore.RealizationFetcher]
// by fetching the realization document(s) for the given [derivation hash].
//
// [derivation hash]: https://zb.256lights.llc/binary-cache/realizations#derivation-hashes
func (s *Store) FetchRealizations(ctx context.Context, drvHash nix.Hash) (zbstore.RealizationMap, error) {
	var hr *hal.Resource
	op := "fetch realizations for " + drvHash.String()
	err := retry(ctx, op, func() error {
		var err error
		hr, err = s.discover(ctx)
		return err
	})
	result := zbstore.RealizationMap{DerivationHash: drvHash}
	if err != nil {
		return result, err
	}

	var ec multierror.Collector
	for u := range s.realizationURLs(&ec, hr, drvHash) {
		err := retry(ctx, op, func() error {
			return s.mergeRealizations(ctx, &result, u)
		})
		if err != nil {
			ec.Add(err)
		}
	}
	return result, ec.Error()
}

func (s *Store) mergeRealizations(ctx context.Context, dst *zbstore.RealizationMap, u *url.URL) error {
	res, err := fetch(ctx, s.client(), &fetchRequest{
		url:    u,
		accept: "application/json,text/*;q=0.9,*/*;q=0.8",
		origin: s.URL,
	})
	if err != nil {
		if isNotFound(err) {
			log.Debugf(ctx, "Fetch realizations for %v: %v", dst.DerivationHash, err)
			return nil
		}
		if !isClientError(err) {
			err = temporaryError{err}
		}
		return err
	}
	doc := new(zbstore.RealizationMap)
	unmarshalers := jsonv2.UnmarshalFromFunc(zbstore.UnmarshalHashJSONFrom)
	if err := jsonv2.Unmarshal(res.body, doc, jsonv2.WithUnmarshalers(unmarshalers)); err != nil {
		return &url.Error{
			Op:  "parse",
			URL: u.Redacted(),
			Err: err,
		}
	}
	if err := dst.Merge(*doc); err != nil {
		return &url.Error{
			Op:  "parse",
			URL: u.Redacted(),
			Err: err,
		}
	}
	return nil
}

// WriteRealizations uploads the realizations to the store.
// WriteRealizations attempts to send a PUT request to each "https://zb-build.dev/api/rel/realization" link
// from the discovery document in sequence until one succeeds.
// Conditional requests are used to prevent lost concurrent updates,
// as best as the server supports.
func (s *Store) WriteRealizations(ctx context.Context, realizations zbstore.RealizationMap) error {
	if realizations.IsEmpty() {
		return nil
	}
	op := "update realizations for " + realizations.DerivationHash.String()
	return retry(ctx, op, func() error {
		hr, err := s.discover(ctx)
		if err != nil {
			return err
		}

		var ec multierror.Collector
		hasPutAllowed := false
		for u := range s.realizationURLs(&ec, hr, realizations.DerivationHash) {
			err := s.putRealizations(ctx, u, realizations)
			if err == nil {
				if err := ec.Error(); err != nil {
					log.Warnf(ctx, "While %s: %v", op, err)
				}
				return nil
			}
			if !isClientError(err) {
				err = temporaryError{err}
			}
			ec.Add(err)
			if !isMethodNotAllowed(err) {
				hasPutAllowed = true
			}
		}
		if ec.Error() == nil {
			ec.Add(&url.Error{
				Op:  "discover",
				URL: s.URL.Redacted(),
				Err: fmt.Errorf("no %s relation", realizationRelation),
			})
		} else if !hasPutAllowed {
			ec.Add(&url.Error{
				Op:  "discover",
				URL: s.URL.Redacted(),
				Err: fmt.Errorf("%s not supported for %s relation", http.MethodPut, realizationRelation),
			})
		}
		return ec.Error()
	})
}

func (s *Store) putRealizations(ctx context.Context, u *url.URL, realizations zbstore.RealizationMap) error {
	var existing zbstore.RealizationMap
	oldResource, fetchError := fetch(ctx, s.client(), &fetchRequest{
		url:    u,
		accept: "application/json,text/*;q=0.9,*/*;q=0.8",
		origin: s.URL,
	})
	rneg := requestNegotiationFromFetchResponse(oldResource, fetchError)
	if !rneg.isMethodAllowed(http.MethodPut) {
		log.Debugf(ctx, "Skipping %s because %s not in Allow header", u.Redacted(), http.MethodPut)
		return &url.Error{
			Op:  "put",
			URL: u.Redacted(),
			Err: methodNotAllowedError{http.MethodPut},
		}
	}
	noReplace := false
	var validators xhttp.ValidatorFields
	switch {
	case fetchError == nil:
		unmarshalers := jsonv2.UnmarshalFromFunc(zbstore.UnmarshalHashJSONFrom)
		if err := jsonv2.Unmarshal(oldResource.body, &existing, jsonv2.WithUnmarshalers(unmarshalers)); err != nil {
			return &url.Error{
				Op:  "get",
				URL: u.Redacted(),
				Err: err,
			}
		}
		existing.Compact()
		validators = oldResource.validators
	case isNotFound(fetchError):
		existing = zbstore.RealizationMap{DerivationHash: realizations.DerivationHash}
		noReplace = true
	case isClientError(fetchError):
		return fetchError
	default:
		// Make error opaque.
		return errors.New(fetchError.Error())
	}
	if err := existing.Merge(realizations); err != nil {
		return fmt.Errorf("%s: %v", u.Redacted(), err)
	}

	marshalers := jsonv2.MarshalToFunc(zbstore.MarshalHashJSONTo)
	newData, err := jsonv2.Marshal(existing, jsonv2.WithMarshalers(marshalers))
	if err != nil {
		return &url.Error{
			Op:  "rewrite",
			URL: u.Redacted(),
			Err: err,
		}
	}

	log.Infof(ctx, "Uploading realizations for %s to %s...", realizations.DerivationHash, s.URL.Redacted())
	err = put(ctx, s.client(), &putRequest{
		url:    u,
		origin: s.URL,
		getContent: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(newData)), nil
		},
		contentLength:  int64(len(newData)),
		contentType:    "application/json",
		acceptEncoding: rneg.acceptEncoding,
		noReplace:      noReplace,
		precondition:   validators,
		cacheControl:   s.RealizationsCacheControl,
	})
	if err != nil {
		if isMethodNotAllowed(err) {
			log.Debugf(ctx, "Skipping %s: %v", u.Redacted(), err)
			err = &url.Error{
				Op:  "put",
				URL: u.Redacted(),
				Err: methodNotAllowedError{http.MethodPut},
			}
		}
		return err
	}
	return nil
}

func (s *Store) realizationURLs(ec *multierror.Collector, discoveryDocument *hal.Resource, drvHash nix.Hash) iter.Seq[*url.URL] {
	return s.expandLinks(ec, discoveryDocument, realizationRelation, struct {
		HashAlgorithm    string
		HashDigest       string
		HashDigestHex    string
		HashDigestBase64 string
	}{
		HashAlgorithm:    drvHash.Type().String(),
		HashDigest:       drvHash.RawBase32(),
		HashDigestHex:    drvHash.RawBase16(),
		HashDigestBase64: drvHash.RawBase64(),
	})
}

func (s *Store) expandLinks(ec *multierror.Collector, discoveryDocument *hal.Resource, rel string, params any) iter.Seq[*url.URL] {
	realizationLinks := discoveryDocument.Links[rel]
	if realizationLinks.Single {
		return func(yield func(*url.URL) bool) {
			ec.Add(fmt.Errorf("%s: link relation %s is not an array", s.URL.Redacted(), rel))
		}
	}
	return func(yield func(*url.URL) bool) {
		addedNotTemplatedError := false
		for _, link := range realizationLinks.Objects {
			if !link.Templated {
				if !addedNotTemplatedError {
					ec.Add(fmt.Errorf("%s: link relation %s: not all links are templated", s.URL.Redacted(), rel))
					addedNotTemplatedError = true
				}
				continue
			}
			u, err := link.Expand(params)
			if err != nil {
				ec.Add(fmt.Errorf("%s: link relation %s: %v", s.URL.Redacted(), rel, err))
				continue
			}
			u, err = resolveReference(s.URL, u)
			if err != nil {
				ec.Add(fmt.Errorf("%s: link relation %s: %v", s.URL.Redacted(), rel, err))
				continue
			}
			if !yield(u) {
				return
			}
		}
	}
}

// httpObject is the implementation of [zbstore.Object] for [Store].
type httpObject struct {
	client Client
	base   *url.URL
	info   *NARInfo
}

func (obj *httpObject) Info() *zbstore.ObjectInfo {
	return &zbstore.ObjectInfo{
		StorePath:      obj.info.StorePath,
		NARHash:        obj.info.NARHash,
		NARSize:        obj.info.NARSize,
		References:     obj.info.References,
		ContentAddress: obj.info.CA,
	}
}

func (obj *httpObject) WriteNAR(ctx context.Context, dst io.Writer) error {
	ref, err := url.Parse(obj.info.URL)
	if err != nil {
		return fmt.Errorf("download %s: invalid nar url: %v", obj.info.StorePath, err)
	}
	narFileURL, err := resolveReference(obj.base, ref)
	if err != nil {
		return fmt.Errorf("download %s: %v", obj.info.StorePath, err)
	}
	return retry(ctx, "download "+string(obj.info.StorePath), func() error {
		req := &http.Request{
			Method: http.MethodGet,
			URL:    narFileURL,
			Header: http.Header{
				"Accept":          {"*/*"},
				"Accept-Encoding": {httpencoding.Accept},
			},
		}
		resp, err := obj.client.Do(req)
		if err != nil {
			// Errors will already be a [*url.Error].
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err := xhttp.ErrorFromResponse(resp)
			return &url.Error{
				Op:  "get",
				URL: narFileURL.Redacted(),
				Err: err,
			}
		}
		decodedBody, err := httpencoding.Decode(resp.Body, resp.Header.Get("Content-Encoding"))
		if err != nil {
			return &url.Error{
				Op:  "get",
				URL: narFileURL.Redacted(),
				Err: err,
			}
		}
		defer decodedBody.Close()
		if _, err := io.Copy(dst, decodedBody); err != nil {
			return &url.Error{
				Op:  "get",
				URL: narFileURL.Redacted(),
				Err: err,
			}
		}
		return nil
	})
}

func resolveReference(baseURL, ref *url.URL) (*url.URL, error) {
	targetURL := baseURL.ResolveReference(ref)
	if (targetURL.Scheme == "" || targetURL.Scheme == fileurl.Scheme) && baseURL.Scheme != fileurl.Scheme {
		return nil, fmt.Errorf("link to %s not permitted from %s", ref.Redacted(), baseURL.Redacted())
	}
	return targetURL, nil
}

func retry(ctx context.Context, op string, f func() error) error {
	for t := xtime.NewBackoffTimer(retryBackoffTable[:], retryBackoffJitter); ; {
		err := f()
		if !isTemporaryError(err) {
			var ec multierror.Collector
			for err := range multierror.All(err) {
				ec.Add(fmt.Errorf("%s: %w", op, err))
			}
			return ec.Error()
		}
		log.Warnf(ctx, "%s (will retry): %v", op, err)
		if err := t.Sleep(ctx); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
}

type temporaryError struct {
	err error
}

func (te temporaryError) Error() string { return te.err.Error() }
func (te temporaryError) Unwrap() error { return te.err }

// isTemporaryError reports whether err indicates a failure
// that might be resolved by retrying.
func isTemporaryError(err error) bool {
	_, ok := errors.AsType[temporaryError](err)
	return ok
}

const retryBackoffJitter = 0.25

var retryBackoffTable = [...]time.Duration{
	10 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	1 * time.Second,
	1 * time.Second,
	5 * time.Second,
	5 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}
