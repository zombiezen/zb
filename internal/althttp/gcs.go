// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package althttp

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2/apierror"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/xhttp"
)

// GCSScheme is the URL scheme for [GCSTransport].
const GCSScheme = "gs"

var _ http.RoundTripper = (*GCSTransport)(nil)

// GCSTransport is an [http.RoundTripper]
// that transforms GET, HEAD, and PUT requests to "gs://" URLs
// into Google Cloud Storage HTTP API requests.
type GCSTransport struct {
	Client *storage.Client
}

func (t *GCSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != GCSScheme {
		return nil, http.ErrSkipAltProtocol
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	if req.URL.Port() != "" {
		return nil, &url.Error{
			Op:  cmp.Or(req.Method, http.MethodGet),
			URL: req.URL.Redacted(),
			Err: fmt.Errorf("ports not allowed in %s:// URLs", GCSScheme),
		}
	}
	switch req.Method {
	case "", http.MethodGet:
		return t.get(req), nil
	case http.MethodHead:
		return t.head(req), nil
	case http.MethodPut:
		return t.put(req), nil
	default:
		resp := errorResponse(req, "", http.StatusNotImplemented)
		resp.Header["Allow"] = []string{"GET, HEAD, PUT"}
		return resp, nil
	}
}

func (t *GCSTransport) objectHandle(u *url.URL) *storage.ObjectHandle {
	return t.Client.Bucket(u.Hostname()).Object(strings.TrimPrefix(u.Path, "/"))
}

func (t *GCSTransport) get(req *http.Request) *http.Response {
	ctx := req.Context()
	objectHandle := t.objectHandle(req.URL)
	attrs, err := objectHandle.Attrs(ctx)
	if err != nil {
		code := http.StatusInternalServerError
		if err, ok := errors.AsType[*apierror.APIError](err); ok && err.HTTPCode() != -1 {
			code = err.HTTPCode()
		}
		return errorResponse(req, err.Error(), code)
	}
	statusCode := xhttp.EvaluatePreconditions(http.MethodGet, req.Header, gcsObjectValidatorFields(attrs), true)
	if statusCode == http.StatusNotModified {
		resp := &http.Response{
			StatusCode:    statusCode,
			Status:        xhttp.Status(statusCode),
			Request:       req,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			ContentLength: 0,
			Header: http.Header{
				"Date": {time.Now().UTC().Format(http.TimeFormat)},
			},
			Body: bytebuffer.Null{},
		}
		setGCSObjectHeaders(resp.Header, attrs)
		return resp
	}

	objectHandle = objectHandle.Generation(attrs.Generation).ReadCompressed(true)
	r, err := objectHandle.NewReader(ctx)
	if err != nil {
		return errorResponse(req, err.Error(), http.StatusInternalServerError)
	}
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: r.Attrs.Size,
		Header: http.Header{
			"Date": {time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: r,
	}
	resp.Status = xhttp.Status(resp.StatusCode)
	setGCSObjectHeaders(resp.Header, attrs)
	decodeResponse(req, resp)
	return resp
}

func (t *GCSTransport) head(req *http.Request) *http.Response {
	ctx := req.Context()
	objectHandle := t.objectHandle(req.URL)
	attrs, err := objectHandle.Attrs(ctx)
	if err != nil {
		code := http.StatusInternalServerError
		if err, ok := errors.AsType[*apierror.APIError](err); ok && err.HTTPCode() != -1 {
			code = err.HTTPCode()
		}
		return errorResponse(req, err.Error(), code)
	}
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: attrs.Size,
		Header: http.Header{
			"Date": {time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: bytebuffer.Null{},
	}
	resp.Status = xhttp.Status(resp.StatusCode)
	setGCSObjectHeaders(resp.Header, attrs)
	return resp
}

func (t *GCSTransport) put(req *http.Request) *http.Response {
	ctx := req.Context()

	contentType := req.Header.Get("Content-Type")
	if contentType == "" {
		resp := errorResponse(req, "Empty Content-Type", http.StatusUnsupportedMediaType)
		resp.Header.Set("Accept", "*/*")
		resp.Header.Set("Accept-Encoding", "*")
		return resp
	}

	objectHandle := t.objectHandle(req.URL)
	attrs, attrsError := objectHandle.Attrs(ctx)
	if attrsError != nil && hasPreconditions(req.Header) && !errors.Is(attrsError, storage.ErrObjectNotExist) {
		return errorResponse(req, attrsError.Error(), http.StatusInternalServerError)
	}
	var validators xhttp.ValidatorFields
	if attrsError == nil {
		validators = gcsObjectValidatorFields(attrs)
	}
	statusCode := xhttp.EvaluatePreconditions(http.MethodPut, req.Header, validators, attrsError == nil)
	switch statusCode {
	case http.StatusPreconditionFailed:
		return errorResponse(req, "Precondition failed", statusCode)
	case http.StatusCreated:
		objectHandle = objectHandle.If(storage.Conditions{
			DoesNotExist: true,
		})
	case http.StatusOK:
		statusCode = http.StatusNoContent
	}
	if attrsError == nil && hasPreconditions(req.Header) {
		objectHandle = objectHandle.If(storage.Conditions{
			GenerationMatch: attrs.Generation,
		})
	}

	w := objectHandle.NewWriter(ctx)
	w.ContentType = contentType
	w.ContentEncoding = req.Header.Get("Content-Encoding")
	w.CacheControl = req.Header.Get("Cache-Control")
	_, err := io.Copy(w, req.Body)
	err = cmp.Or(err, w.Close())
	if err != nil {
		return errorResponse(req, err.Error(), http.StatusInternalServerError)
	}

	resp := &http.Response{
		StatusCode:    statusCode,
		Status:        xhttp.Status(statusCode),
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: 0,
		Header: http.Header{
			"Date": {time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: bytebuffer.Null{},
	}
	setGCSObjectHeaders(resp.Header, w.Attrs())
	resp.Header.Del("Content-Type")
	resp.Header.Del("Cache-Control")
	if statusCode == http.StatusNoContent {
		resp.Header.Del("Content-Length")
	} else {
		resp.Header.Set("Content-Length", "0")
	}
	return resp
}

func gcsObjectValidatorFields(attrs *storage.ObjectAttrs) xhttp.ValidatorFields {
	vf := xhttp.ValidatorFields{
		LastModified: attrs.Updated,
	}
	if attrs.Etag != "" {
		if etag, err := xhttp.StrongEntityTag(attrs.Etag); err == nil {
			vf.ETag = etag
		}
	}
	return vf
}

func setGCSObjectHeaders(dst http.Header, attrs *storage.ObjectAttrs) {
	dst.Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
	dst.Set("Content-Type", attrs.ContentType)
	if attrs.ContentEncoding != "" {
		dst.Set("Content-Encoding", attrs.ContentEncoding)
	}
	vf := gcsObjectValidatorFields(attrs)
	if !vf.LastModified.IsZero() {
		dst.Set("Last-Modified", attrs.Updated.UTC().Format(http.TimeFormat))
	}
	if vf.ETag != "" {
		dst.Set("ETag", string(vf.ETag))
	}
	if attrs.CacheControl != "" {
		dst.Set("Cache-Control", attrs.CacheControl)
	}
}
