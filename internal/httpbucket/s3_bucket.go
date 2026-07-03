// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package httpbucket

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var _ http.RoundTripper = (*S3Transport)(nil)

// S3Transport is an [http.RoundTripper] that transform requests to "s3://" URLs
// to S3 HTTP API requests.
type S3Transport struct {
	Client *s3.Client
}

// RoundTrip implements [http.RoundTripper] for "s3://" URLs.
func (transport *S3Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "s3" {
		return nil, http.ErrSkipAltProtocol
	}
	ctx := req.Context()
	switch req.Method {
	case "", http.MethodGet:
		input := &s3.GetObjectInput{
			Bucket: new(req.Host),
			Key:    new(strings.TrimPrefix(req.URL.Path, "/")),
		}
		if values := req.Header.Values("Range"); len(values) > 0 {
			input.Range = new(strings.Join(values, headerFieldCombiner))
		}
		if values := req.Header.Values("If-Match"); len(values) > 0 {
			input.IfMatch = new(strings.Join(values, headerFieldCombiner))
		}
		if values := req.Header.Values("If-None-Match"); len(values) > 0 {
			input.IfNoneMatch = new(strings.Join(values, headerFieldCombiner))
		}
		if values := req.Header.Values("If-Modified-Since"); len(values) == 1 {
			if t, err := http.ParseTime(values[0]); err == nil {
				input.IfModifiedSince = new(t)
			}
		}
		if values := req.Header.Values("If-Unmodified-Since"); len(values) == 1 {
			if t, err := http.ParseTime(values[0]); err == nil {
				input.IfUnmodifiedSince = new(t)
			}
		}
		output, err := transport.Client.GetObject(ctx, input)
		if err != nil {
			if err, isNoSuchKey := errors.AsType[*types.NoSuchKey](err); isNoSuchKey {
				return errorResponse(req, err.ErrorMessage(), http.StatusNotFound), nil
			}
			return errorResponse(req, err.Error(), http.StatusInternalServerError), nil
		}

		resp := &http.Response{
			Request:       req,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			ContentLength: *output.ContentLength,
			Header: http.Header{
				"Content-Length": {strconv.FormatInt(*output.ContentLength, 10)},
				"Date":           {time.Now().UTC().Format(http.TimeFormat)},
			},
			Body: output.Body,
		}
		// TODO(soon): Handle 304s.
		switch {
		case output.ContentRange != nil:
			resp.StatusCode = http.StatusPartialContent
		default:
			resp.StatusCode = http.StatusOK
		}
		resp.Status = http.StatusText(resp.StatusCode)
		if output.ContentType != nil {
			resp.Header.Set("Content-Type", *output.ContentType)
		}
		if output.ContentEncoding != nil {
			resp.Header.Set("Content-Encoding", *output.ContentEncoding)
		}
		if output.ContentLanguage != nil {
			resp.Header.Set("Content-Language", *output.ContentLanguage)
		}
		if output.AcceptRanges != nil {
			resp.Header.Set("Accept-Ranges", *output.AcceptRanges)
		}
		if output.ContentRange != nil {
			resp.Header.Set("Content-Range", *output.ContentRange)
		}
		if output.LastModified != nil {
			resp.Header.Set("Last-Modified", output.LastModified.UTC().Format(http.TimeFormat))
		}
		if output.ETag != nil {
			resp.Header.Set("ETag", *output.ETag)
		}
		if output.CacheControl != nil {
			resp.Header.Set("Cache-Control", *output.CacheControl)
		}
		return resp, nil
	case http.MethodHead:
		input := &s3.HeadObjectInput{
			Bucket: new(req.Host),
			Key:    new(strings.TrimPrefix(req.URL.Path, "/")),
		}
		output, err := transport.Client.HeadObject(ctx, input)
		if err != nil {
			if err, isNoSuchKey := errors.AsType[*types.NoSuchKey](err); isNoSuchKey {
				return errorResponse(req, err.ErrorMessage(), http.StatusNotFound), nil
			}
			return errorResponse(req, err.Error(), http.StatusInternalServerError), nil
		}

		resp := &http.Response{
			Request:       req,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			StatusCode:    http.StatusOK,
			Status:        http.StatusText(http.StatusOK),
			ContentLength: *output.ContentLength,
			Header: http.Header{
				"Content-Length": {strconv.FormatInt(*output.ContentLength, 10)},
				"Date":           {time.Now().UTC().Format(http.TimeFormat)},
			},
			Body: http.NoBody,
		}
		if output.ContentType != nil {
			resp.Header.Set("Content-Type", *output.ContentType)
		}
		if output.ContentEncoding != nil {
			resp.Header.Set("Content-Encoding", *output.ContentEncoding)
		}
		if output.ContentLanguage != nil {
			resp.Header.Set("Content-Language", *output.ContentLanguage)
		}
		if output.AcceptRanges != nil {
			resp.Header.Set("Accept-Ranges", *output.AcceptRanges)
		}
		if output.ContentRange != nil {
			resp.Header.Set("Content-Range", *output.ContentRange)
		}
		if output.LastModified != nil {
			resp.Header.Set("Last-Modified", output.LastModified.UTC().Format(http.TimeFormat))
		}
		if output.ETag != nil {
			resp.Header.Set("ETag", *output.ETag)
		}
		if output.CacheControl != nil {
			resp.Header.Set("Cache-Control", *output.CacheControl)
		}
		return resp, nil
	default:
		resp := errorResponse(req, "", http.StatusMethodNotAllowed)
		resp.Header["Allow"] = []string{"GET, HEAD, PUT"}
		return resp, nil
	}
}
