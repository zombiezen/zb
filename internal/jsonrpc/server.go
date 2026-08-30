// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package jsonrpc

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
)

// ServerCodec represents a single connection from a server to a client.
// ReadRequest and WriteResponse must be safe to call concurrently with each other,
// but [Server] guarantees that it will never make multiple concurrent ReadRequest calls
// nor multiple concurrent WriteResponse calls.
//
// WriteResponse must not retain response after it returns.
type ServerCodec interface {
	ReadRequest() (jsontext.Value, error)
	WriteResponse(response jsontext.Value) error
}

// A type that implements Handler responds to JSON-RPC requests.
// Implementations of JSONRPC must be safe to call from multiple goroutines concurrently.
//
// The jsonrpc package provides [Client], [ServeMux], and [HandlerFunc]
// as implementations of Handler.
type Handler interface {
	JSONRPC(ctx context.Context, req *Request) (*Response, error)
}

// HandlerFunc is a function that implements [Handler].
type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

// JSONRPC calls f.
func (f HandlerFunc) JSONRPC(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

// MethodNotFoundHandler implements [Handler]
// by returning a [MethodNotFound] error for all requests.
type MethodNotFoundHandler struct{}

// JSONRPC returns an error for which [ErrorCode] returns [MethodNotFound].
func (MethodNotFoundHandler) JSONRPC(ctx context.Context, req *Request) (*Response, error) {
	return nil, Error(MethodNotFound, fmt.Errorf("method %q not found", req.Method))
}

type server struct {
	writeLock sync.Mutex
	codec     ServerCodec

	mu        sync.Mutex
	cancelMap map[RequestID]context.CancelFunc
}

// Serve serves JSON-RPC requests for a connection.
// Serve will read requests from the codec until ReadRequest returns an error,
// which Serve will return once all requests have completed.
func Serve(ctx context.Context, codec ServerCodec, handler Handler) error {
	srv := &server{
		codec:     codec,
		cancelMap: make(map[RequestID]context.CancelFunc),
	}
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		content, err := codec.ReadRequest()
		if err != nil {
			return err
		}

		// TODO(someday): Support batches.
		parsed := new(serverRequest)
		dec := jsontext.NewDecoder(bytes.NewBuffer(content))
		if err := parsed.UnmarshalJSONFrom(dec); err != nil {
			srv.writeError(err)
			continue
		}

		requestCtx, cancel := context.WithCancel(ctx)
		if !parsed.Notification {
			srv.mu.Lock()
			srv.cancelMap[parsed.id] = cancel
			srv.mu.Unlock()
		}

		wg.Go(func() {
			srv.single(requestCtx, handler, parsed, cancel)
		})
	}
}

func (srv *server) single(ctx context.Context, handler Handler, req *serverRequest, cancel context.CancelFunc) {
	defer cancel()
	// Make defensive copy of request information.
	notification := req.Notification

	var resp *Response
	var handlerError error
	switch req.Method {
	case cancelMethod:
		resp, handlerError = srv.cancel(&req.Request)
	default:
		resp, handlerError = handler.JSONRPC(ctx, &req.Request)
	}
	cancel()

	if notification {
		// Notifications do not receive a response.
		return
	}

	srv.mu.Lock()
	delete(srv.cancelMap, req.id)
	srv.mu.Unlock()

	buf := new(bytes.Buffer)
	enc := jsontext.NewEncoder(buf)
	if handlerError != nil {
		if err := marshalErrorResponseJSONTo(enc, req.id, handlerError); err != nil {
			panic(err)
		}
	} else {
		if err := marshalResponseJSONTo(enc, req.id, resp); err != nil {
			panic(err)
		}
	}

	srv.writeLock.Lock()
	defer srv.writeLock.Unlock()
	srv.codec.WriteResponse(jsonValueFromBuffer(buf))
}

// cancel handles a [cancelMethod] request.
func (srv *server) cancel(req *Request) (*Response, error) {
	var args cancelParams
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, Error(InvalidParams, err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if cancel := srv.cancelMap[args.ID]; cancel != nil {
		cancel()
	}
	delete(srv.cancelMap, args.ID)
	return nil, nil
}

func (srv *server) writeError(err error) {
	buf := new(bytes.Buffer)
	enc := jsontext.NewEncoder(buf)
	if err := marshalErrorResponseJSONTo(enc, RequestID{}, err); err != nil {
		panic(err)
	}

	srv.writeLock.Lock()
	defer srv.writeLock.Unlock()
	srv.codec.WriteResponse(jsonValueFromBuffer(buf))
}

// ServeMux is a mapping of method names to JSON-RPC handlers.
type ServeMux map[string]Handler

// JSONRPC calls the handler that corresponds to the request's method
// or returns a [MethodNotFound] error if no such handler is present.
func (mux ServeMux) JSONRPC(ctx context.Context, req *Request) (*Response, error) {
	h := mux[req.Method]
	if h == nil {
		return nil, Error(MethodNotFound, fmt.Errorf("method %s not found", req.Method))
	}
	return h.JSONRPC(ctx, req)
}

type serverRequest struct {
	id RequestID
	Request
}

func (req *serverRequest) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return Error(ParseError, err)
	}
	if got, want := tok.Kind(), jsontext.Kind('{'); got != want {
		return Error(InvalidRequest, fmt.Errorf("jsonrpc request must be an object (got %v)", got))
	}

	hadVersion := false
	hadMethod := false
	req.Notification = true
keys:
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return Error(ParseError, err)
		}
		var key string
		switch kind := tok.Kind(); kind {
		case '"':
			key = tok.String()
		case '}':
			break keys
		default:
			return Error(ParseError, fmt.Errorf("unexpected %v token", kind))
		}

		switch key {
		case "jsonrpc":
			hadVersion = true
			var versionString string
			if err := jsonv2.UnmarshalDecode(dec, &versionString); err != nil {
				return Error(InvalidRequest, fmt.Errorf("jsonrpc version: %v", err))
			}
			if versionString != "2.0" {
				return Error(InvalidRequest, fmt.Errorf("jsonrpc version %q not supported", versionString))
			}
		case "method":
			hadMethod = true
			if err := jsonv2.UnmarshalDecode(dec, &req.Method); err != nil {
				return Error(InvalidRequest, fmt.Errorf("jsonrpc method: %v", err))
			}
		case "params":
			var err error
			req.Params, err = dec.ReadValue()
			if err != nil {
				return Error(InvalidRequest, err)
			}
		case "id":
			req.Notification = false
			if err := req.id.UnmarshalJSONFrom(dec); err != nil {
				return Error(InvalidRequest, fmt.Errorf("jsonrpc id: %v", err))
			}
		default:
			if isReservedRequestField(key) {
				if err := dec.SkipValue(); err != nil {
					return Error(InvalidRequest, err)
				}
				continue
			}
			if req.Extra == nil {
				req.Extra = make(map[string]jsontext.Value)
			}
			v, err := dec.ReadValue()
			if err != nil {
				return Error(InvalidRequest, err)
			}
			req.Extra[key] = v.Clone()
		}
	}

	if !hadVersion {
		return Error(InvalidRequest, fmt.Errorf("jsonrpc version missing in request"))
	}
	if !hadMethod {
		return Error(InvalidRequest, fmt.Errorf("jsonrpc method missing in request"))
	}
	if req.Notification {
		req.id = RequestID{}
	}

	return nil
}

func marshalResponseJSONTo(enc *jsontext.Encoder, id RequestID, resp *Response) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("marshal json-rpc response: %v", err)
		}
	}()

	if resp != nil {
		for k := range resp.Extra {
			if isReservedResponseField(k) {
				return fmt.Errorf("extra field %q not permitted", k)
			}
		}
	}

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := marshalJSONRPCVersionTo(enc); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("id")); err != nil {
		return err
	}
	if err := id.MarshalJSONTo(enc); err != nil {
		return err
	}

	if err := enc.WriteToken(jsontext.String("result")); err != nil {
		return err
	}
	if resp == nil || len(resp.Result) == 0 {
		if err := enc.WriteToken(jsontext.Null); err != nil {
			return err
		}
	} else {
		if err := enc.WriteValue(resp.Result); err != nil {
			return err
		}
	}

	if resp != nil {
		extraKeys := maps.Keys(resp.Extra)
		if deterministic, _ := jsonv2.GetOption(enc.Options(), jsonv2.Deterministic); deterministic {
			extraKeys = slices.Values(slices.Sorted(extraKeys))
		}
		for k := range extraKeys {
			if err := enc.WriteToken(jsontext.String(k)); err != nil {
				return err
			}
			if err := enc.WriteValue(resp.Extra[k]); err != nil {
				return err
			}
		}
	}

	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	return nil
}

func marshalErrorResponseJSONTo(enc *jsontext.Encoder, id RequestID, responseError error) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("marshal json-rpc response: %v", err)
		}
	}()

	code, ok := CodeFromError(responseError)
	if !ok {
		code = UnknownErrorCode
	}

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := marshalJSONRPCVersionTo(enc); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("id")); err != nil {
		return err
	}
	if err := id.MarshalJSONTo(enc); err != nil {
		return err
	}

	if err := enc.WriteToken(jsontext.String("error")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("code")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.Int(int64(code))); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("message")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(responseError.Error())); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}

	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	return nil
}
