// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package jsonrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/xtime"
	"zombiezen.com/go/log"
	"zombiezen.com/go/xcontext"
)

// ClientCodec represents a single connection from a client to a server.
// WriteRequest and ReadResponse must be safe to call concurrently with each other,
// but [Client] guarantees that it will never make multiple concurrent WriteRequest calls
// nor multiple concurrent ReadResponse calls.
//
// Close is called when the Client no longer intends to use the connection.
// Close may be called concurrently with WriteRequest or ReadResponse:
// doing so should interrupt either call and cause the call to return an error.
type ClientCodec interface {
	RequestWriter
	ReadResponse() (jsontext.Value, error)
	Close() error
}

// RequestWriter holds the WriteRequest method of [ClientCodec].
// WriteRequest must not retain request after it returns.
type RequestWriter interface {
	WriteRequest(request jsontext.Value) error
}

// OpenFunc opens a connection for a [Client].
type OpenFunc func(ctx context.Context) (ClientCodec, error)

// A Client represents a JSON-RPC client
// that automatically reconnects after I/O errors.
// Methods on Client are safe to call from multiple goroutines concurrently.
type Client struct {
	// comms is a channel of RPCs to send to the server
	// with optional responses.
	comms chan clientRequest
	// cancelComms stops the communicate method.
	cancelComms context.CancelFunc
	// commsDone is closed once the communicate method returns.
	commsDone chan struct{}
	// codecRequests is a channel for requests of the codec.
	codecRequests chan clientCodecRequest
}

// NewClient returns a new [Client] that opens connections using the given function.
// The caller is responsible for calling [Client.Close]
// when the Client is no longer in use.
//
// NewClient will start opening a connection in the background,
// but will return before the connection is established.
// The first call to [Client.JSONRPC] will block on the connection.
func NewClient(ctx context.Context, open OpenFunc) *Client {
	c := &Client{
		comms:         make(chan clientRequest),
		commsDone:     make(chan struct{}),
		codecRequests: make(chan clientCodecRequest),
	}
	var commsCtx context.Context
	commsCtx, c.cancelComms = context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(c.commsDone)
		c.communicate(commsCtx, open)
	}()
	return c
}

// Close closes the client connection.
func (c *Client) Close() error {
	c.cancelComms()
	<-c.commsDone
	return nil
}

// JSONRPC sends a request to the server.
func (c *Client) JSONRPC(ctx context.Context, req *Request) (*Response, error) {
	if !isValidParamStruct(req.Params) {
		return nil, Error(InvalidRequest, fmt.Errorf("call json rpc %s: params must be an object or an array", req.Method))
	}

	write := make(chan error, 1)
	creq := clientRequest{
		context: ctx,
		Request: req,
		write:   write,
	}
	if req.Notification {
		select {
		case c.comms <- creq:
			select {
			case err := <-write:
				if err != nil {
					return nil, fmt.Errorf("call json rpc %s: %w", req.Method, err)
				}
				return nil, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("call json rpc %s: %w", req.Method, ctx.Err())
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("call json rpc %s: %w", req.Method, ctx.Err())
		}
	}

	responseChan := make(chan rawResponse, 1)
	creq.response = responseChan
	select {
	case c.comms <- creq:
	case <-ctx.Done():
		return nil, fmt.Errorf("call json rpc %s: %w", req.Method, ctx.Err())
	}
	resp, err := (<-responseChan).toResponse()
	if err != nil {
		return resp, fmt.Errorf("call json rpc %s: %w", req.Method, err)
	}
	return resp, nil
}

func (c *Client) communicate(ctx context.Context, open OpenFunc) {
	for {
		if ctx.Err() != nil {
			return
		}

		log.Debugf(ctx, "Opening new JSON-RPC connection...")
		conn, err := open(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Warnf(ctx, "Failed to open JSON-RPC connection (will retry): %v", err)
				if err := xtime.Sleep(ctx, 1*time.Second); err != nil {
					return
				}
			}

			continue
		}

		c.handleConn(ctx, conn, xcontext.CloseWhenDone(ctx, conn))
	}
}

type inflightRequestState struct {
	context      context.Context
	responseChan chan<- rawResponse
	ignoreCancel func()
}

func (c *Client) handleConn(ctx context.Context, conn ClientCodec, closer io.Closer) {
	// Read messages in a separate goroutine,
	// so we can select on them down below.
	messages := make(chan jsontext.Value)
	go func() {
		defer close(messages)
		for {
			msg, err := conn.ReadResponse()
			if err != nil {
				log.Debugf(ctx, "Failed to read JSON-RPC response: %v", err)
				return
			}
			messages <- msg
		}
	}()

	inflight := make(map[int64]inflightRequestState)
	cancels := make(chan int64)
	var cancelGroup sync.WaitGroup

	defer func() {
		log.Debugf(ctx, "Shutting down JSON-RPC connection")
		closer.Close()
		// Inform any pending application calls that the response will never come.
		for id, r := range inflight {
			log.Debugf(ctx, "Response for %v dropped", id)
			r.responseChan <- rawResponse{error: errInterrupt}
			r.ignoreCancel()
		}
		inflight = nil

		// Drain the reader goroutine.
		for range messages {
		}

		// Drain the cancels channel.
		go func() {
			cancelGroup.Wait()
			close(cancels)
		}()
		for range cancels {
		}
	}()

	nextID := int64(1)
	buf := new(bytes.Buffer)
	var enc jsontext.Encoder
	for {
		select {
		case msg := <-messages:
			if msg == nil {
				// Connection fault.
				return
			}
			dispatchResponse(ctx, msg, inflight)
		case req := <-c.comms:
			// Handle incoming application requests.

			id := int64(-1)
			if req.Notification {
				log.Debugf(ctx, "Writing %s JSON-RPC notification", req.Method)
			} else {
				id = nextID
				nextID++

				cancelGroup.Add(1)
				stopAfterFunc := context.AfterFunc(req.context, func() {
					cancels <- id
					cancelGroup.Done()
				})
				inflight[id] = inflightRequestState{
					context:      req.context,
					responseChan: req.response,
					ignoreCancel: func() {
						if stopAfterFunc() {
							cancelGroup.Done()
						}
					},
				}

				if log.IsEnabled(log.Debug) {
					clientID := marshalClientID(nil, id)
					log.Debugf(ctx, "Writing %s JSON-RPC with id=%s", req.Method, clientID)
				}
			}

			buf.Reset()
			enc.Reset(buf)
			if err := marshalClientRequestJSONTo(&enc, id, req); err != nil {
				if req.write != nil {
					req.write <- err
				} else {
					log.Warnf(ctx, "Failed to marshal message: %v", err)
				}
				continue
			}

			err := conn.WriteRequest(jsonValueFromBuffer(buf))
			if req.write != nil {
				req.write <- err
			}
			if err != nil {
				log.Debugf(ctx, "Failed to send message: %v", err)
				return
			}
		case id := <-cancels:
			// A request's context has been canceled.

			state, waiting := inflight[id]
			if !waiting {
				continue
			}

			if log.IsEnabled(log.Debug) {
				clientID := marshalClientID(nil, id)
				log.Debugf(ctx, "Canceling JSON-RPC with id=%s", clientID)
			}
			buf.Reset()
			enc.Reset(buf)
			if err := marshalCancelRequestJSONTo(&enc, id); err != nil {
				log.Errorf(ctx, "Failed to marshal cancel message: %v", err)
				continue
			}

			// Remove from in-flight.
			// We don't reuse IDs unless we wrap 2^64,
			// so we'll drop the server's response and that's okay.
			if state.responseChan != nil {
				state.responseChan <- rawResponse{
					error: state.context.Err(),
				}
			}
			if state.ignoreCancel != nil {
				state.ignoreCancel()
			}
			delete(inflight, id)

			if err := conn.WriteRequest(jsonValueFromBuffer(buf)); err != nil {
				log.Debugf(ctx, "Failed to send message: %v", err)
				return
			}
		case r := <-c.codecRequests:
			r.codec <- conn
		appUse:
			for {
				// We intentionally do not listen for ctx.Done() in this loop
				// because we want holding the codec to delay client shutdown.
				// Responses can continue to be handled.
				select {
				case <-r.release:
					break appUse
				case msg := <-messages:
					if msg == nil {
						// Connection fault.
						return
					}
					dispatchResponse(ctx, msg, inflight)
				}
			}
		case <-ctx.Done():
			// If the application is closing the client,
			// end the loop and close the connection (via the defer).
			return
		}
	}
}

func marshalClientRequestJSONTo(enc *jsontext.Encoder, id int64, req clientRequest) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("marshal json-rpc request: %v", err)
		}
	}()

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := marshalJSONRPCVersionTo(enc); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("method")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(req.Method)); err != nil {
		return err
	}

	if !req.Notification {
		if err := enc.WriteToken(jsontext.String("id")); err != nil {
			return err
		}
		clientID := marshalClientID(enc.AvailableBuffer(), id)
		if err := enc.WriteValue(clientID); err != nil {
			return err
		}
	}

	if len(req.Params) > 0 {
		if err := enc.WriteToken(jsontext.String("params")); err != nil {
			return err
		}
		if err := enc.WriteValue(req.Params); err != nil {
			return err
		}
	}

	if len(req.Extra) > 0 {
		for k := range req.Extra {
			if isReservedRequestField(k) {
				return fmt.Errorf("extra field %q not permitted", k)
			}
		}
		keys := maps.Keys(req.Extra)
		if deterministic, ok := jsonv2.GetOption(enc.Options(), jsonv2.Deterministic); ok && deterministic {
			keys = slices.Values(slices.Sorted(keys))
		}
		for k := range keys {
			if err := enc.WriteToken(jsontext.String(k)); err != nil {
				return err
			}
			if err := enc.WriteValue(req.Extra[k]); err != nil {
				return err
			}
		}
	}

	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	return nil
}

func marshalCancelRequestJSONTo(enc *jsontext.Encoder, id int64) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("marshal json-rpc cancel request: %v", err)
		}
	}()

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := marshalJSONRPCVersionTo(enc); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("method")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(cancelMethod)); err != nil {
		return err
	}

	if err := enc.WriteToken(jsontext.String("params")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("id")); err != nil {
		return err
	}
	clientID := marshalClientID(enc.AvailableBuffer(), id)
	if err := enc.WriteValue(clientID); err != nil {
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

var errInterrupt = errors.New("connection interrupted")

// dispatchResponse sends a server response (possibly a batch)
// to the corresponding listener(s).
func dispatchResponse(ctx context.Context, msg jsontext.Value, inflight map[int64]inflightRequestState) {
	batch, err := unmarshalResponseBatch(msg)
	if err != nil {
		log.Warnf(ctx, "JSON-RPC server returned invalid JSON: %v", err)
		return
	}
	for _, resp := range batch {
		// We specifically don't check anything beyond ID here
		// because we want most errors to be returned to the application.
		id, err := resp.id()
		if err != nil {
			continue
		}
		if !id.IsString() {
			// We only make string IDs.
			continue
		}
		idNum, ok := unmarshalClientID(id.String())
		if !ok {
			// We only make *numeric* string IDs.
			continue
		}

		state := inflight[idNum]
		if state.responseChan != nil {
			state.responseChan <- resp
		}
		if state.ignoreCancel != nil {
			state.ignoreCancel()
		}
		delete(inflight, idNum)
	}
}

// Codec obtains the client's currently active codec,
// waiting for a connection to be established if necessary.
// The caller is responsible for calling the release function
// when it is no longer using the codec:
// all new calls to [Client.JSONRPC] will block
// to avoid concurrent message writes.
// After the first call to the release function,
// subsequent calls are no-ops.
// Client will continue to read responses from the codec
// while the caller is holding onto the codec.
//
// Codec guarantees that if it succeeds,
// it returns a value that was returned by the [OpenFunc] provided to [NewClient].
// However, applications should not call either ReadResponse or Close on the returned codec.
func (c *Client) Codec(ctx context.Context) (codec RequestWriter, release func(), err error) {
	// Buffer not strictly necessary,
	// but allows communication goroutine to go back to reading messages quickly.
	codecChan := make(chan RequestWriter, 1)
	releaseChan := make(chan struct{})

	select {
	case c.codecRequests <- clientCodecRequest{codecChan, releaseChan}:
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("obtain jsonrpc client connection: %w", ctx.Err())
	}
	codec = <-codecChan
	var once sync.Once
	return codec, func() {
		once.Do(func() { close(releaseChan) })
	}, nil
}

// A clientRequest is sent from client methods to the connection handler
// to be written on the wire.
type clientRequest struct {
	context context.Context
	*Request

	// write is a channel that will receive a notification of the write's result if non-nil.
	// It must have a buffer of at least 1.
	write chan<- error

	// response is the channel that will receive the response message if non-nil.
	// It must have a buffer of at least 1.
	// If the connection is interrupted before a response is received,
	// it will receive a nil message.
	response chan<- rawResponse
}

type clientCodecRequest struct {
	codec   chan<- RequestWriter
	release <-chan struct{}
}

type rawResponse struct {
	msg   map[string]jsontext.Value
	error error
}

func (resp rawResponse) toResponse() (*Response, error) {
	if resp.error != nil {
		return nil, resp.error
	}
	if err := resp.checkVersion(); err != nil {
		return nil, err
	}
	extra := inverseFilterMap(resp.msg, isReservedResponseField)

	switch resultField, errorField := resp.msg["result"], resp.msg["error"]; {
	case len(resultField) > 0 && len(errorField) > 0:
		err := fmt.Errorf("jsonrpc response contains both result and error")
		if len(extra) > 0 {
			return &Response{Extra: extra}, err
		}
		return nil, err
	case len(resultField) > 0:
		return &Response{
			Result: resultField,
			Extra:  extra,
		}, nil
	case len(errorField) > 0:
		var errorObject struct {
			Code    ErrorCode `json:"code"`
			Message string    `json:"message"`
		}
		err := jsonv2.Unmarshal(errorField, &errorObject)
		if err != nil {
			// If we can't unmarshal, use the JSON directly.
			// Better than nothing for debugging.
			err = errors.New(errorField.String())
		} else if errorObject.Message != "" {
			err = Error(errorObject.Code, errors.New(errorObject.Message))
		} else {
			err = Error(errorObject.Code, fmt.Errorf("jsonrpc error %d", errorObject.Code))
		}
		if len(extra) > 0 {
			return &Response{Extra: extra}, err
		}
		return nil, err
	default:
		err := fmt.Errorf("jsonrpc response does not contain result nor error")
		if len(extra) > 0 {
			return &Response{Extra: extra}, err
		}
		return nil, err
	}
}

func (resp rawResponse) checkVersion() error {
	version := resp.msg["jsonrpc"]
	if len(version) == 0 {
		return fmt.Errorf("jsonrpc version missing in response")
	}
	var s string
	if err := jsonv2.Unmarshal(version, &s); err != nil {
		return fmt.Errorf("jsonrpc version: %v", err)
	}
	if s != "2.0" {
		return fmt.Errorf("jsonrpc version %q not supported", s)
	}
	return nil
}

func (resp rawResponse) id() (RequestID, error) {
	raw := resp.msg["id"]
	if len(raw) == 0 {
		return RequestID{}, fmt.Errorf("jsonrpc response missing id")
	}
	var id RequestID
	err := id.UnmarshalJSON(raw)
	if err != nil {
		return RequestID{}, fmt.Errorf("jsonrpc response id: %v", err)
	}
	return id, nil
}

// marshalClientID appends a hex-encoded JSON string of an integer ID
// to the given byte slice.
func marshalClientID(dst []byte, id int64) []byte {
	// To avoid any potential problems with JSON number precision,
	// encode the ID as a JSON string.
	dst = slices.Grow(dst, 8*2+len(`"-"`))
	dst = append(dst, '"')
	dst = strconv.AppendInt(dst, id, 16)
	dst = append(dst, '"')
	return dst
}

// unmarshalClientID parses a string back to an integer ID,
// ensuring that it is of the format from [marshalClientID].
func unmarshalClientID(s string) (int64, bool) {
	s2, neg := strings.CutPrefix(s, "-")
	s2, zero := strings.CutPrefix(s2, "0")
	if zero {
		// Zero is only an acceptable prefix if it is the entire string.
		return 0, !neg && s2 == ""
	}
	for _, c := range []byte(s2) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			// Not a lowercase hex digit.
			return 0, false
		}
	}

	id, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// unmarshalResponseBatch unmarshals either a JSON-RPC response object
// or an array of such objects.
func unmarshalResponseBatch(msg jsontext.Value) ([]rawResponse, error) {
	if msg.Kind() != '[' {
		var response rawResponse
		if err := jsonv2.Unmarshal(msg, &response.msg); err != nil {
			return nil, err
		}
		return []rawResponse{response}, nil
	}

	// Split apart the array ourselves.
	// If one element isn't an object, don't fail the entire batch.
	dec := jsontext.NewDecoder(bytes.NewBuffer(msg))
	if tok, err := dec.ReadToken(); err != nil {
		return nil, err
	} else if got := tok.Kind(); got != '[' {
		return nil, fmt.Errorf("unexpected %v token", got)
	}
	var responses []rawResponse
	for dec.PeekKind() != ']' {
		data, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		var r rawResponse
		r.error = jsonv2.Unmarshal(data, &r.msg)
		responses = append(responses, r)
	}
	return responses, nil
}

func isValidParamStruct(msg jsontext.Value) bool {
	if len(msg) == 0 {
		// Omitted is fine.
		return true
	}
	kind := msg.Kind()
	return kind == '{' || kind == '['
}
