// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zombiezen.com/go/log"
)

// Server is the interface used by [Serve] to handle zb JSON-RPC messages.
type Server interface {
	jsonrpc.Handler
	Importer
}

// Importer wraps the StoreImport method from [zbstore.Importer].
type Importer interface {
	StoreImport(ctx context.Context, r io.Reader) error
}

// Serve serves zb JSON-RPC requests for a connection.
// Serve will read requests from the [io.ReadWriteCloser] until Read returns an error,
// which Serve will return once all requests have completed.
// When the [context.Context]'s Done() channel is closed,
// Serve will attempt to shut down the reading side of the connection to trigger an error.
//
// Serve will always close the [io.ReadWriteCloser] before returning.
func Serve(ctx context.Context, rwc io.ReadWriteCloser, srv Server) error {
	if f, ok := closeReadFunc(rwc); ok {
		readClosed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			defer close(readClosed)
			f()
		})
		defer func() {
			if !stop() {
				<-readClosed
			}
		}()
	}

	sc := newServerCodec(ctx, rwc, srv)
	serveError := jsonrpc.Serve(ctx, sc, jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
		dst := exportDestination{w: sc.w}
		if idJSON := req.Extra[exportIDExtraFieldName]; len(idJSON) > 0 {
			if err := jsonv2.Unmarshal(idJSON, &dst.id); err != nil {
				return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("%s: %v", exportIDExtraFieldName, err))
			}
		}
		ctx = context.WithValue(ctx, exportDestinationContextKey{}, dst)
		return srv.JSONRPC(ctx, req)
	}))
	sc.close()
	closeError := rwc.Close()
	return errors.Join(serveError, closeError)
}

// exportIDExtraFieldName is the name of the extra field in [jsonrpc.Request]
// used to pass a value that will be passed through with [exportIDHeaderName].
const exportIDExtraFieldName = "zbExportID"

// serverCodec implements [jsonrpc.ServerCodec] on an [io.ReadWriter]
// using the Language Server Protocol "base protocol" for framing.
type serverCodec struct {
	ctx      context.Context
	r        *jsonrpc.Reader
	w        chan *jsonrpc.Writer
	importer Importer
}

func newServerCodec(ctx context.Context, rw io.ReadWriter, importer Importer) *serverCodec {
	sc := &serverCodec{
		ctx:      ctx,
		r:        jsonrpc.NewReader(rw),
		w:        make(chan *jsonrpc.Writer, 1),
		importer: importer,
	}
	sc.w <- jsonrpc.NewWriter(rw)
	return sc
}

func (sc *serverCodec) ReadRequest() (jsontext.Value, error) {
	for {
		header, bodySize, err := sc.r.NextMessage()
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
			body, err := io.ReadAll(sc.r)
			if err != nil {
				return nil, err
			}
			return body, nil
		case exportContentType:
			if err := sc.importer.StoreImport(sc.ctx, sc.r); err != nil {
				err = fmt.Errorf("while receiving export: %v", err)
				if bodySize < 0 {
					return nil, err
				}
				log.Warnf(sc.ctx, "%v", err)
			}
		default:
			// Ignore, if possible.
			if bodySize < 0 {
				return nil, fmt.Errorf("remote sent unknown Content-Type %q without valid Content-Length", ct)
			}
		}
	}
}

func (sc *serverCodec) WriteResponse(response jsontext.Value) error {
	w := <-sc.w
	defer func() { sc.w <- w }()
	return writeRPCMessage(w, response)
}

func (sc *serverCodec) close() {
	if _, hasWriter := <-sc.w; hasWriter {
		close(sc.w)
	}
}

type exportDestinationContextKey struct{}

type exportDestination struct {
	id string
	w  chan *jsonrpc.Writer
}

// ServeExport copies a `nix-store --export` stream from an [io.Reader]
// to the connection being handled by [Serve].
// For contexts that are not derived from a JSON-RPC initiated by [Serve],
// ServeExport reads a `nix-store --export` stream from the [io.Reader] and discards the data.
func ServeExport(ctx context.Context, r io.Reader) error {
	v := ctx.Value(exportDestinationContextKey{})
	if v == nil {
		return nopImporter{}.StoreImport(ctx, r)
	}
	dst := v.(exportDestination)
	select {
	case w, ok := <-dst.w:
		if !ok {
			return errors.New("export on a closed connection")
		}
		defer func() { dst.w <- w }()
		return writeExport(ctx, w, dst.id, r)
	case <-ctx.Done():
		return fmt.Errorf("write export: %w", ctx.Err())
	}
}

func closeReadFunc(r io.Reader) (f func() error, ok bool) {
	if cr, ok := r.(interface{ CloseRead() error }); ok {
		return cr.CloseRead, true
	} else if rd, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
		return func() error {
			return rd.SetReadDeadline(time.Now())
		}, true
	}
	return func() error { return nil }, false
}
