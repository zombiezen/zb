// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

// Client implements [zbstore.Store], [zbstore.Importer], and [zbstore.Exporter] via JSON-RPC.
type Client struct {
	client *jsonrpc.Client
}

// NewClient returns a [*Client] that opens connections using the given function.
// The caller is responsible for calling [*Client.Close] when the client is no longer in use.
func NewClient(ctx context.Context, openConn func(context.Context) (io.ReadWriteCloser, error)) *Client {
	s := new(Client)
	s.client = jsonrpc.NewClient(ctx, func(ctx context.Context) (jsonrpc.ClientCodec, error) {
		conn, err := openConn(ctx)
		if err != nil {
			return nil, err
		}
		return newClientCodec(ctx, conn), nil
	})
	return s
}

// Close closes the client connection.
func (c *Client) Close() error {
	return c.client.Close()
}

// JSONRPC implements [jsonrpc.Handler] by sending a request to the server.
func (c *Client) JSONRPC(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	if req.Method == ExportMethod {
		// Prevent users from initiating exports except through [*Client.StoreExport].
		return nil, jsonrpc.Error(jsonrpc.MethodNotFound, fmt.Errorf("method %q not found", req.Method))
	}
	return c.client.JSONRPC(ctx, req)
}

// Object implements [zbstore.Store] by making an [InfoRequest] to the server.
func (c *Client) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	var resp struct {
		Info jsontext.Value `json:"info"`
	}
	err := jsonrpc.Do(ctx, c.client, InfoMethod, &resp, &InfoRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("stat %s: %v", path, err)
	}
	if len(resp.Info) == 0 || resp.Info.Kind() == 'n' {
		return nil, fmt.Errorf("stat %s: %w", path, zbstore.ErrNotFound)
	}
	info := new(ObjectInfo)
	if err := jsonv2.Unmarshal(resp.Info, info); err != nil {
		return nil, fmt.Errorf("stat %s: %v", path, err)
	}
	return &object{
		store:   c,
		rawInfo: resp.Info,
		info:    *info.WithPath(path),
	}, nil
}

// WriteObject implements [zbstore.ObjectWriter]
// by sending `nix-store --export` data over the underlying connection.
// WriteObject will return an error if s.Handler is not a [*jsonrpc.Client] using a [*Codec].
func (c *Client) WriteObject(ctx context.Context, object zbstore.Object) error {
	path := object.Info().StorePath
	if _, err := c.Object(ctx, path); err == nil {
		// Already exists: no need to re-import.
		log.Debugf(ctx, "Using existing store path %s", path)
		return nil
	} else if !errors.Is(err, zbstore.ErrNotFound) {
		return fmt.Errorf("write %s: %v", path, err)
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		exporter := zbstore.NewExportWriter(pw)
		if err := exporter.WriteObject(ctx, object); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := exporter.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	err := c.StoreImport(ctx, pr)
	pr.Close()
	<-done
	return err
}

// StoreImport implements [zbstore.Importer]
// by sending the `nix-store --export` data over the underlying connection.
func (c *Client) StoreImport(ctx context.Context, r io.Reader) error {
	generic, releaseConn, err := c.client.Codec(ctx)
	if err != nil {
		return fmt.Errorf("import store objects: %w", err)
	}
	zc, ok := generic.(*clientCodec)
	if !ok {
		releaseConn()
		return fmt.Errorf("import store objects: store connection is %T (want %T)", generic, (*clientCodec)(nil))
	}
	err = writeExport(ctx, zc.w, "", r)
	releaseConn()
	if err != nil {
		return fmt.Errorf("import store objects: %v", err)
	}

	// Add sync point via doing a no-op RPC.
	// This ensures that the export has been processed before returning.
	if err := jsonrpc.Do(ctx, c.client, NopMethod, nil, nil); err != nil {
		return fmt.Errorf("import store objects: wait for export to complete: %v", err)
	}
	return nil
}

// StoreExport implements [zbstore.Exporter] by sending an [ExportRequest] to the server.
func (c *Client) StoreExport(ctx context.Context, dst io.Writer, paths sets.Set[zbstore.Path], opts *zbstore.ExportOptions) error {
	if err := c.export(ctx, dst, NewExportRequest(paths, opts)); err != nil {
		return fmt.Errorf("export store objects: %w", err)
	}
	return nil
}

func (c *Client) export(ctx context.Context, dst io.Writer, req *ExportRequest) error {
	var id string
	writerChan := make(chan io.Writer)
	writeDone := make(chan error)
	rpcDone := make(chan error, 1)
	err := func() error {
		generic, releaseConn, err := c.client.Codec(ctx)
		if err != nil {
			return err
		}
		defer releaseConn()
		cc, ok := generic.(*clientCodec)
		if !ok {
			return fmt.Errorf("store connection is %T (want %T)", generic, (*clientCodec)(nil))
		}

		cc.mu.Lock()
		if cc.idPrefix == "" {
			var bits [9]byte
			rand.Read(bits[:])
			cc.idPrefix = base64.URLEncoding.EncodeToString(bits[:])
		}
		id = cc.idPrefix + strconv.FormatUint(cc.idCounter, 16)
		cc.idCounter++
		requestJSON, err := jsonv2.Marshal(&exportRPCRequest{
			id:     id,
			params: req,
		})
		if err != nil {
			cc.mu.Unlock()
			return err
		}
		cc.pendingExports[id] = pendingExport{
			w:         writerChan,
			writeDone: writeDone,
			rpcDone:   rpcDone,
		}
		cc.mu.Unlock()

		log.Debugf(ctx, "Sending export request for %v with id=%+q...", req.Paths, id)
		err = cc.WriteRequest(requestJSON)
		if err != nil {
			cc.mu.Lock()
			close(writerChan)
			delete(cc.pendingExports, id)
			cc.mu.Unlock()
			return err
		}

		return nil
	}()
	if err != nil {
		return err
	}

	select {
	case writerChan <- dst:
		// If [*clientCodec.receiveExport] started writing to dst,
		// then we need to wait until it is done before returning
		// to avoid writing to dst after export returns.
		importError := <-writeDone
		select {
		case rpcError := <-rpcDone:
			log.Debugf(ctx, "Export RPC finished: id=%+q err=%v", id, err)
			if errors.Is(rpcError, errInterrupt) {
				// Connection closed is not an interesting error,
				// especially if the import succeeded overall.
				rpcError = nil
			}
			return errors.Join(rpcError, importError)
		case <-ctx.Done():
			if importError != nil {
				return importError
			}
			return ctx.Err()
		}
	case err := <-rpcDone:
		log.Debugf(ctx, "Export RPC finished: id=%+q err=%v", id, err)
		if err == nil {
			err = errors.New("server did not send export")
		}
		return err
	case <-ctx.Done():
		close(writerChan)
		// TODO(someday): Send cancel message.
		// Would need to synchronize with [*jsonrpc.Client].
		return ctx.Err()
	}
}

// RawObjectInfo returns the JSON serialization of the [*ObjectInfo] for the [zbstore.Object].
// If the object came from [*Client.Object], then this will be the original bytes received.
func RawObjectInfo(obj zbstore.Object) jsontext.Value {
	if obj, ok := obj.(*object); ok {
		return bytes.Clone(obj.rawInfo)
	}
	data, err := jsonv2.Marshal(NewObjectInfo(obj.Info()))
	if err != nil {
		panic(err)
	}
	return data
}

type object struct {
	store   *Client
	info    zbstore.ObjectInfo
	rawInfo jsontext.Value
}

func (obj *object) Info() *zbstore.ObjectInfo {
	return &obj.info
}

func (obj *object) WriteNAR(ctx context.Context, dst io.Writer) error {
	pr, pw := io.Pipe()
	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		err := obj.store.export(ctx, pw, &ExportRequest{
			Paths:             []zbstore.Path{obj.info.StorePath},
			ExcludeReferences: true,
		})
		pw.CloseWithError(err)
		return err
	})
	grp.Go(func() error {
		snw := &singleNARWriter{w: dst}
		err := (&zbstore.BufferedImporter{
			ObjectWriter:  snw,
			BufferCreator: snw,
		}).StoreImport(ctx, pr)
		pr.CloseWithError(err)
		return err
	})
	if err := grp.Wait(); err != nil {
		return fmt.Errorf("write nar for %s: %w", obj.info.StorePath, err)
	}
	return nil
}

type singleNARWriter struct {
	w        io.Writer
	received bool
}

func (snw *singleNARWriter) WriteObject(ctx context.Context, object zbstore.Object) error {
	if snw.received {
		return errors.New("received multiple store objects from export")
	}
	snw.received = true
	return nil
}

func (snw *singleNARWriter) CreateBuffer(size int64) (bytebuffer.ReadWriteSeekCloser, error) {
	if snw.received {
		return nil, errors.New("started buffer for second object")
	}
	return onlyWriter{snw.w}, nil
}

type onlyWriter struct {
	io.Writer
}

func (onlyWriter) Read(p []byte) (int, error) {
	return 0, errors.New("cannot read when writing single NAR")
}

func (onlyWriter) Seek(offset int64, whence int) (int64, error) {
	return 0, errors.New("cannot seek when writing single NAR")
}

func (onlyWriter) Close() error {
	return nil
}

// clientCodec implements [jsonrpc.ClientCodec] on an [io.ReadWriteCloser]
// using the Language Server Protocol "base protocol" for framing.
type clientCodec struct {
	ctx context.Context
	r   *jsonrpc.Reader
	w   *jsonrpc.Writer
	c   io.Closer

	mu             sync.Mutex
	idPrefix       string
	idCounter      uint64
	pendingExports map[string]pendingExport
}

type pendingExport struct {
	w         <-chan io.Writer
	writeDone chan<- error
	rpcDone   chan<- error
}

func newClientCodec(ctx context.Context, rwc io.ReadWriteCloser) *clientCodec {
	return &clientCodec{
		ctx:            ctx,
		r:              jsonrpc.NewReader(rwc),
		w:              jsonrpc.NewWriter(rwc),
		c:              rwc,
		pendingExports: make(map[string]pendingExport),
	}
}

func (cc *clientCodec) WriteRequest(request jsontext.Value) error {
	return writeRPCMessage(cc.w, request)
}

func (cc *clientCodec) ReadResponse() (jsontext.Value, error) {
	for {
		header, bodySize, err := cc.r.NextMessage()
		if err != nil {
			return nil, err
		}
		switch ct := header.Get("Content-Type"); ct {
		case rpcContentType:
			if bodySize < 0 {
				return nil, fmt.Errorf("remote sent api message without valid Content-Length")
			}
			if bodySize > maxAPIMessageSize {
				return nil, fmt.Errorf("remote sent large api message (%d bytes)", maxAPIMessageSize)
			}
			body, err := io.ReadAll(cc.r)
			if err != nil {
				return nil, err
			}
			if !cc.interceptExportResponse(body) {
				return body, nil
			}
		case exportContentType:
			if err := cc.receiveExport(header.Get(exportIDHeaderName)); err != nil {
				if bodySize < 0 {
					return nil, err
				}
				log.Warnf(cc.ctx, "%v", err)
			}
		default:
			// Ignore, if possible.
			if bodySize < 0 {
				return nil, fmt.Errorf("remote sent unknown Content-Type %q without valid Content-Length", ct)
			}
		}
	}
}

func (cc *clientCodec) interceptExportResponse(response jsontext.Value) bool {
	cc.mu.Lock()
	hasExports := len(cc.pendingExports) > 0
	cc.mu.Unlock()
	if !hasExports {
		// We don't need to parse the payload if we don't have any pending exports.
		return false
	}

	var parsed struct {
		Version string         `json:"jsonrpc"`
		ID      string         `json:"id"`
		Error   jsontext.Value `json:"error"`
	}
	if err := jsonv2.Unmarshal(response, &parsed); err != nil {
		return false
	}
	if parsed.Version != "2.0" || parsed.ID == "" {
		return false
	}

	cc.mu.Lock()
	e, idKnown := cc.pendingExports[parsed.ID]
	delete(cc.pendingExports, parsed.ID)
	cc.mu.Unlock()

	if !idKnown {
		return false
	}
	var rpcError error
	if len(parsed.Error) > 0 {
		var errorObject struct {
			Code    jsonrpc.ErrorCode `json:"code"`
			Message string            `json:"message"`
		}
		if err := jsonv2.Unmarshal(parsed.Error, &errorObject); err != nil {
			// If we can't unmarshal, use the JSON directly.
			// Better than nothing for debugging.
			rpcError = errors.New(parsed.Error.String())
		} else if errorObject.Message != "" {
			rpcError = jsonrpc.Error(errorObject.Code, errors.New(errorObject.Message))
		} else {
			rpcError = jsonrpc.Error(errorObject.Code, fmt.Errorf("jsonrpc error %d", errorObject.Code))
		}
	}
	e.rpcDone <- rpcError
	return true
}

func (cc *clientCodec) receiveExport(id string) error {
	var w io.Writer
	var done chan<- error
	idKnown := false
	if id != "" {
		cc.mu.Lock()
		var e pendingExport
		e, idKnown = cc.pendingExports[id]
		if idKnown && e.w != nil {
			cc.pendingExports[id] = pendingExport{
				rpcDone: e.rpcDone,
			}
		}
		cc.mu.Unlock()

		// Mostly synchronous: sender is either blocking sending the writer or closed.
		if e.w == nil {
			log.Warnf(cc.ctx, "Receiving duplicate export over RPC with id=%+q", id)
		} else {
			w = <-e.w
			if w == nil {
				log.Debugf(cc.ctx, "Receiving export over RPC with id=%+q. No longer interested.", id)
			} else {
				log.Debugf(cc.ctx, "Receiving export over RPC with id=%+q...", id)
				done = e.writeDone
			}
		}
	}
	if !idKnown {
		log.Warnf(cc.ctx, "Receiving unsolicited export over RPC with id=%+q", id)
	}
	var importError error
	if w == nil {
		importError = zbstore.Null{}.StoreImport(cc.ctx, cc.r)
	} else {
		// The Importer.Import method determines the boundary of the body.
		// When we tee, we don't want copy failures downstream
		// to mess up our JSON-RPC connection.
		// We swallow the errors and try to read the `nix-store --export` data to the end.
		ecw := &errorCaptureWriter{w: w}
		importError = zbstore.Null{}.StoreImport(cc.ctx, io.TeeReader(cc.r, ecw))
		done <- cmp.Or(importError, ecw.err)
	}
	log.Debugf(cc.ctx, "Finished receiving RPC export id=%+q err=%v", id, importError)
	if importError != nil {
		return fmt.Errorf("while receiving export: %w", importError)
	}
	return nil
}

func (cc *clientCodec) Close() error {
	err := cc.c.Close()

	cc.mu.Lock()
	for _, e := range cc.pendingExports {
		e.rpcDone <- errInterrupt
	}
	clear(cc.pendingExports)
	cc.mu.Unlock()

	return err
}

// errorCaptureWriter passes through writes to another [io.Writer]
// until an error occurs,
// but will never surface the error.
type errorCaptureWriter struct {
	w   io.Writer
	err error
}

// Write writes p to ecw.w unless an error has occurred.
// Write always returns len(p), nil.
func (ecw *errorCaptureWriter) Write(p []byte) (int, error) {
	if ecw.err == nil {
		_, ecw.err = ecw.w.Write(p)
	}
	return len(p), nil
}

var errInterrupt = errors.New("connection interrupted")
