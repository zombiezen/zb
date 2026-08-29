// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"sync"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

// Store implements [zbstore.Store], [zbstore.Importer], and [zbstore.Exporter] via JSON-RPC.
type Store struct {
	Handler jsonrpc.Handler

	mu             sync.Mutex
	idPrefix       string
	idCounter      uint64
	pendingExports map[string]pendingExport
}

type pendingExport struct {
	w    <-chan io.Writer
	done chan<- error
}

// Object implements [zbstore.Store] by making a [InfoRequest] to s.Handler.
//
// Calling [zbstore.Object.WriteNAR] on the returned object
// depends on s.Handler being wired up to [*Store.Import].
// Otherwise, WriteNAR will block until ctx.Done() is closed.
func (s *Store) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	resp := new(InfoResponse)
	err := jsonrpc.Do(ctx, s.Handler, InfoMethod, resp, &InfoRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("stat %s: %v", path, err)
	}
	if resp.Info == nil {
		return nil, fmt.Errorf("stat %s: %w", path, zbstore.ErrNotFound)
	}
	return &object{
		store: s,
		info:  *resp.Info.WithPath(path),
	}, nil
}

// StoreImport implements [zbstore.Importer]
// by sending the `nix-store --export` data over the underlying connection.
// StoreImport will return an error if s.Handler is not a [*jsonrpc.Client] using a [*Codec].
func (s *Store) StoreImport(ctx context.Context, r io.Reader) error {
	client, ok := s.Handler.(*jsonrpc.Client)
	if !ok {
		return fmt.Errorf("import store objects: store handler is %T (want %T)", s.Handler, (*jsonrpc.Client)(nil))
	}
	generic, releaseConn, err := client.Codec(ctx)
	if err != nil {
		return err
	}
	codec, ok := generic.(*Codec)
	if !ok {
		releaseConn()
		return fmt.Errorf("import store objects: store connection is %T (want %T)", generic, (*Codec)(nil))
	}
	exportError := codec.Export(nil, r)
	releaseConn()
	if exportError != nil {
		return exportError
	}

	// Add sync point via doing a no-op RPC.
	// This ensures that the export has been processed before returning.
	if err := jsonrpc.Do(ctx, client, NopMethod, nil, nil); err != nil {
		return fmt.Errorf("import store objects: wait for export to complete: %v", err)
	}
	return nil
}

// StoreExport implements [zbstore.Exporter]
// by sending an [ExportRequest] to s.Handler.
//
// StoreExport depends on s.Handler being wired up to [*Store.Import].
// Otherwise, StoreExport will block until ctx.Done() is closed.
func (s *Store) StoreExport(ctx context.Context, dst io.Writer, paths sets.Set[zbstore.Path], opts *zbstore.ExportOptions) error {
	if err := s.export(ctx, dst, NewExportRequest(paths, opts)); err != nil {
		return fmt.Errorf("export store objects: %w", err)
	}
	return nil
}

func (s *Store) export(ctx context.Context, dst io.Writer, req *ExportRequest) error {
	s.mu.Lock()
	if s.idPrefix == "" {
		var bits [9]byte
		rand.Read(bits[:])
		s.idPrefix = base64.URLEncoding.EncodeToString(bits[:])
	}
	id := s.idPrefix + strconv.FormatUint(s.idCounter, 16)
	s.idCounter++
	if s.pendingExports == nil {
		s.pendingExports = make(map[string]pendingExport)
	}
	writerChan := make(chan io.Writer)
	done := make(chan error)
	s.pendingExports[id] = pendingExport{
		w:    writerChan,
		done: done,
	}
	s.mu.Unlock()

	grp, ctx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		// This RPC blocks until the export is finished,
		// so we run concurrently handing off the writer to [*Store.Import].
		return sendExportRequest(ctx, s.Handler, id, req)
	})

	grp.Go(func() error {
		select {
		case writerChan <- dst:
			// If [*Store.Import] started writing to dst,
			// then we need to wait until it is done before returning
			// to avoid writing to dst after export returns.
			return <-done
		case <-ctx.Done():
			close(writerChan)
			return ctx.Err()
		}
	})

	return grp.Wait()
}

func sendExportRequest(ctx context.Context, h jsonrpc.Handler, id string, params *ExportRequest) error {
	paramsJSON, err := jsonv2.Marshal(params)
	if err != nil {
		return fmt.Errorf("call json rpc %s: %v", ExportMethod, err)
	}
	idJSON, err := jsontext.AppendQuote(nil, id)
	if err != nil {
		return fmt.Errorf("call json rpc %s: %v", ExportMethod, err)
	}
	log.Debugf(ctx, "Sending export request for %v with id=%+q...", params.Paths, id)
	_, err = h.JSONRPC(ctx, &jsonrpc.Request{
		Method: ExportMethod,
		Params: paramsJSON,
		Extra: map[string]jsontext.Value{
			ExportIDExtraFieldName: idJSON,
		},
	})
	log.Debugf(ctx, "Export RPC finished: id=%+q err=%v", id, err)
	return err
}

// Import implements [Importer]
// to dispatch `nix-store --export` JSON-RPC messages
// to other methods that request exports, like [*Store.StoreExport].
func (s *Store) Import(header jsonrpc.Header, body io.Reader) error {
	ctx := context.Background()
	id := header.Get(ExportIDHeaderName)

	var done chan<- error
	var ecw *errorCaptureWriter
	if id == "" {
		log.Warnf(ctx, "Received unsolicited export over RPC without ID")
	} else {
		s.mu.Lock()
		e, idKnown := s.pendingExports[id]
		delete(s.pendingExports, id)
		s.mu.Unlock()

		if !idKnown {
			log.Warnf(ctx, "Received unsolicited export over RPC with id=%+q", id)
		} else if w, ok := <-e.w; !ok {
			log.Debugf(ctx, "Received export over RPC with id=%+q. No longer interested.", id)
		} else {
			log.Debugf(ctx, "Receiving export over RPC with id=%+q...", id)
			// The Importer.Import method determines the boundary of the body.
			// When we tee, we don't want copy failures downstream
			// to mess up our JSON-RPC connection.
			// We swallow the errors and try to read the `nix-store --export` data to the end.
			ecw = &errorCaptureWriter{w: w}
			body = io.TeeReader(body, ecw)
			done = e.done
		}
	}

	readError := zbstore.ReceiveExport(nopReceiver{}, body)
	log.Debugf(ctx, "Finished receiving RPC export id=%+q err=%v", id, readError)
	if done != nil {
		done <- cmp.Or(readError, ecw.err)
	}
	return readError
}

type object struct {
	store *Store
	info  zbstore.ObjectInfo
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
		err := zbstore.ReceiveExport(&singleNARReceiver{w: dst}, pr)
		pr.CloseWithError(err)
		return err
	})
	if err := grp.Wait(); err != nil {
		return fmt.Errorf("write nar for %s: %w", obj.info.StorePath, err)
	}
	return nil
}

type singleNARReceiver struct {
	w        io.Writer
	received bool
}

func (snr *singleNARReceiver) Write(p []byte) (n int, err error) {
	if snr.received {
		return 0, fmt.Errorf("received multiple store objects from export")
	}
	return snr.w.Write(p)
}

func (snr *singleNARReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	snr.received = true
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
