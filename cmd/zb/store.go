// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-json-experiment/json/jsontext"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/xio"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

type storeDatabaseFlags struct {
	DBPath string `kong:"name=db,default=${default_store_db},help=Path to store database file."`
}

type storeCommand struct {
	Object storeObjectCommand `kong:"cmd"`
}

func (storeCommand) Signature() string {
	return `kong:"cmd,help=Inspect the store."`
}

type storeObjectCommand struct {
	Info     storeObjectInfoCommand     `kong:"cmd"`
	Import   storeObjectImportCommand   `kong:"cmd"`
	Export   storeObjectExportCommand   `kong:"cmd"`
	Copy     storeObjectCopyCommand     `kong:"cmd,aliases=cp"`
	Delete   storeObjectDeleteCommand   `kong:"cmd,aliases=rm"`
	Register storeObjectRegisterCommand `kong:"cmd,hidden"`
}

func (storeObjectCommand) Signature() string {
	return `kong:"help=Inspect and transfer store objects."`
}

type storeObjectInfoCommand struct {
	Paths      []string `kong:"name=path,arg,optional"`
	JSONFormat bool     `kong:"name=json,Print object info as JSON"`
}

func (c *storeObjectInfoCommand) Signature() string {
	return `kong:"help=Show metadata of one or more store objects."`
}

func (c *storeObjectInfoCommand) Run(ctx context.Context, g *globalConfig) error {
	store := g.openLocalStore(ctx)
	defer store.Close()

	const errNotExist = "does not exist"

	// TODO(someday): Batch.
	var buf []byte
	for i, p := range c.Paths {
		path, err := zbstore.ParsePath(p)
		if err != nil {
			return err
		}

		req := &zbstorerpc.InfoRequest{
			Path: path,
		}
		if c.JSONFormat {
			// Dump info response directly to preserve unknown fields.
			var partialParsed struct {
				Info jsontext.Value `json:"info"`
			}
			err = jsonrpc.Do(ctx, store, zbstorerpc.InfoMethod, &partialParsed, req)
			if err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			if string(partialParsed.Info) == "null" {
				return fmt.Errorf("%s: %v", path, errNotExist)
			}
			if err := partialParsed.Info.Compact(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			jsonBytes := append(slices.Clip([]byte(partialParsed.Info)), '\n')
			if _, err := os.Stdout.Write(jsonBytes); err != nil {
				return err
			}
			continue
		}

		resp := new(zbstorerpc.InfoResponse)
		err = jsonrpc.Do(ctx, store, zbstorerpc.InfoMethod, resp, req)
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		if resp.Info == nil {
			return fmt.Errorf("%s: %v", path, errNotExist)
		}

		buf = buf[:0]
		if i > 0 {
			// Blank line between entries.
			buf = append(buf, '\n')
		}
		buf, err = resp.Info.WithPath(path).AppendText(buf)
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(buf); err != nil {
			return err
		}
	}

	return nil
}

type storeObjectExportCommand struct {
	Paths             []zbstore.Path `kong:"arg,name=path"`
	IncludeReferences bool           `kong:"name=references,negatable,help=Include referenced store objects (default ${default}),default=true"`
	OutputPath        string         `kong:"name=output,short=o,placeholder=file,help=Output file"`
}

func (c *storeObjectExportCommand) Signature() string {
	return `kong:"help=Export one or more store objects."`
}

func (c *storeObjectExportCommand) Run(ctx context.Context, g *globalConfig) error {
	if c.OutputPath == "" && term.IsTerminal(int(os.Stdout.Fd())) {
		//lint:ignore ST1005 Output is known to be a terminal: punctuation is okay.
		return errors.New("refusing to send binary export to stdout (a tty). Pass --output=- to override.")
	}
	output, err := openOutputFile(c.OutputPath)
	if err != nil {
		return err
	}
	closer := xio.CloseOnce(output)
	defer closer.Close()

	store := g.openLocalStore(ctx)
	defer store.Close()

	err = store.StoreExport(ctx, output, sets.Collect(slices.Values(c.Paths)), &zbstore.ExportOptions{
		ExcludeReferences: !c.IncludeReferences,
	})
	if err != nil {
		return err
	}
	if err := closer.Close(); err != nil {
		return err
	}
	return nil
}

type nopReceiver struct{}

func (nopReceiver) Write(p []byte) (n int, err error)         { return len(p), nil }
func (nopReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {}

type storeObjectImportCommand struct {
	Paths []string `kong:"arg,name=path,optional"`
}

func (c *storeObjectImportCommand) Signature() string {
	return `kong:"help=Import store objects from a previous \\'zb store object export\\' command."`
}

func (c *storeObjectImportCommand) Run(ctx context.Context, g *globalConfig) error {
	store := g.openLocalStore(ctx)
	defer store.Close()

	inputPaths := c.Paths
	if len(inputPaths) == 0 {
		inputPaths = []string{"-"}
	}
	if len(inputPaths) == 1 && inputPaths[0] == "-" && term.IsTerminal(int(os.Stdin.Fd())) {
		log.Infof(ctx, "Waiting for data on stdin...")
	}

	storePaths, err := catExports(ctx, store, inputPaths)
	if err != nil {
		return err
	}
	ok := true
	for _, path := range storePaths {
		var exists bool
		err := jsonrpc.Do(ctx, store, zbstorerpc.ExistsMethod, &exists, &zbstorerpc.ExistsRequest{
			Path: string(path),
		})
		if err != nil {
			log.Errorf(ctx, "Checking for existence of %s: %v", path, err)
		} else if !exists {
			log.Errorf(ctx, "Importing %s failed", path)
		} else {
			log.Infof(ctx, "Imported %s", path)
		}
	}
	if !ok {
		return errors.New("one or more paths not successfully imported")
	}
	return nil
}

// catExports concatenates the exports from the given files into a single export
// and sends it to the store connected via the [zbstore.Importer].
func catExports(ctx context.Context, store zbstore.Importer, exportFiles []string) ([]zbstore.Path, error) {
	// If there are no files, then no-op.
	if len(exportFiles) == 0 {
		return nil, nil
	}

	// If there is a single file, then copy the file verbatim.
	if len(exportFiles) == 1 {
		f, err := openInputFile(exportFiles[0])
		if err != nil {
			return nil, err
		}
		defer f.Close()

		// We still need to parse the export to determine store paths to confirm.
		// If this fails, don't fail the overall operation.
		pr, pw := io.Pipe()
		ch := make(chan []zbstore.Path)
		go func() {
			rec := &exportPathRecorder{ctx: ctx}
			if err := zbstore.ReceiveExport(rec, pr); err != nil {
				log.Warnf(ctx, "Invalid store export format in %s: %v", inputFileName(exportFiles[0]), err)
			}
			// If we encountered a parse error, still consume the rest of the stream.
			io.Copy(io.Discard, pr)
			pr.Close()
			ch <- rec.paths
		}()

		err = store.StoreImport(ctx, io.TeeReader(f, pw))
		pw.Close()
		paths := <-ch
		return paths, err
	}

	// Start sending to the store.
	pr, pw := io.Pipe()
	ch := make(chan error)
	go func() {
		err := store.StoreImport(ctx, pr)
		pr.CloseWithError(err)
		ch <- err
		close(ch)
	}()
	defer func() { <-ch }()

	// Copy each NAR inside each export file.
	var storePaths []zbstore.Path
	exporter := zbstore.NewExportWriter(pw)
	for _, path := range exportFiles {
		var err error
		storePaths, err = copyToExporter(ctx, storePaths, exporter, path)
		if err != nil {
			return storePaths, err
		}
	}
	if err := exporter.Close(); err != nil {
		return storePaths, err
	}
	if err := pw.Close(); err != nil {
		return storePaths, err
	}
	err := <-ch
	return storePaths, err
}

// copyToExporter reads the file at path in the `nix-store --export` format
// and copies each NAR file to the exporter.
// It appends each of the store paths encountered to storePaths.
func copyToExporter(ctx context.Context, storePaths []zbstore.Path, exporter *zbstore.ExportWriter, path string) ([]zbstore.Path, error) {
	f, err := openInputFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	recv := &passthroughReceiver{exporter: exporter}
	rec := &exportPathRecorder{
		ctx:     ctx,
		paths:   storePaths,
		wrapped: recv,
	}
	if err := zbstore.ReceiveExport(rec, f); err != nil {
		return rec.paths, fmt.Errorf("copying %s: %v", inputFileName(path), err)
	}
	if recv.err != nil {
		return rec.paths, fmt.Errorf("copying %s: %v", inputFileName(path), recv.err)
	}
	return rec.paths, nil
}

// passthroughReceiver copies NAR files to an exporter.
// It is a helper for [copyToExporter].
type passthroughReceiver struct {
	exporter *zbstore.ExportWriter
	err      error
}

func (pr *passthroughReceiver) Write(p []byte) (int, error) {
	if pr.err != nil {
		return 0, pr.err
	}
	var n int
	n, pr.err = pr.exporter.Write(p)
	return n, pr.err
}

func (pr *passthroughReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	if pr.err == nil {
		pr.err = pr.exporter.Trailer(trailer)
	}
}

// exportPathRecorder is a [zbstore.NARReceiver] that records the store paths encountered.
// It optionally passes through its methods to wrapped.
type exportPathRecorder struct {
	ctx     context.Context
	paths   []zbstore.Path
	wrapped zbstore.NARReceiver
}

func (rec *exportPathRecorder) Write(p []byte) (n int, err error) {
	if rec.wrapped == nil {
		return len(p), nil
	}
	return rec.wrapped.Write(p)
}

func (rec *exportPathRecorder) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	log.Debugf(rec.ctx, "Found trailer for %s", trailer.StorePath)
	rec.paths = append(rec.paths, trailer.StorePath)

	if rec.wrapped != nil {
		rec.wrapped.ReceiveNAR(trailer)
	}
}

type storeObjectCopyCommand struct {
	Paths       []zbstore.Path `kong:"arg,name=path,required,help=Store object paths."`
	Source      *storeConfig   `kong:"name=from,short=f,help=Store to copy from (default to local)"`
	Destination *storeConfig   `kong:"name=to,short=t,help=Store to copy to (default to local)"`
}

func (c *storeObjectCopyCommand) Signature() string {
	return `kong:"help=Copy one or more store objects to/from a remote store."`
}

func (c *storeObjectCopyCommand) Validate() error {
	if c.Source.isNull() && c.Destination.isNull() {
		return fmt.Errorf("--from or --to must be specified")
	}
	return nil
}

func (c *storeObjectCopyCommand) Run(ctx context.Context, g *globalConfig) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	base := fileurl.FromPath(cwd)
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	sourceConfig := c.Source.resolve(base)
	destinationConfig := c.Destination.resolve(base)
	paths := sets.Collect(slices.Values(c.Paths))
	provideHTTPClient, cleanup := singletonProvider(g.newHTTPClient)
	defer cleanup()

	switch {
	case sourceConfig.isNull() && !destinationConfig.isNull():
		localStore := g.openLocalStore(ctx)
		defer localStore.Close()
		destinationStore, err := destinationConfig.toStore(func() (zbstorehttp.Client, error) {
			return provideHTTPClient()
		})
		if err != nil {
			return err
		}
		return storeCopy(ctx, destinationStore, localStore, paths)
	case !sourceConfig.isNull() && destinationConfig.isNull():
		localStore := g.openLocalStore(ctx)
		defer localStore.Close()
		sourceStore, err := sourceConfig.toStore(func() (zbstorehttp.Client, error) {
			return provideHTTPClient()
		})
		if err != nil {
			return err
		}
		return storeCopy(ctx, localStore, sourceStore, paths)
	case !sourceConfig.isNull() && !destinationConfig.isNull():
		sourceStore, err := sourceConfig.toStore(func() (zbstorehttp.Client, error) {
			return provideHTTPClient()
		})
		if err != nil {
			return err
		}
		destinationStore, err := destinationConfig.toStore(func() (zbstorehttp.Client, error) {
			return provideHTTPClient()
		})
		if err != nil {
			return err
		}
		return storeCopy(ctx, destinationStore, sourceStore, paths)
	default:
		return c.Validate()
	}
}

func storeCopy(ctx context.Context, dst, src zbstore.Store, paths sets.Set[zbstore.Path]) error {
	switch dst := dst.(type) {
	case *zbstorehttp.Store:
		// TODO(someday): Unify with backend logic.
		objects, err := zbstore.ObjectClosure(ctx, src, paths, 2)
		if err != nil {
			return err
		}
		// TODO(someday): Make concurrent.
		for _, obj := range objects {
			log.Infof(ctx, "Copying %s...", obj.Info().StorePath)
			err := dst.PutObject(ctx, &zbstorehttp.PutObjectRequest{
				StorePath:      obj.Info().StorePath,
				References:     obj.Info().References,
				ContentAddress: obj.Info().ContentAddress,
				NARSize:        obj.Info().NARSize,
				GetNAR: func() (io.ReadCloser, error) {
					pr, pw := io.Pipe()
					done := make(chan struct{})
					go func() {
						defer close(done)
						err := obj.WriteNAR(ctx, pw)
						pw.CloseWithError(err)
					}()
					return xio.ReadFuncCloser{
						Reader: pr,
						CloseFunc: func() error {
							err := pr.Close()
							<-done
							return err
						},
					}, nil
				},
			})
			if err != nil {
				return err
			}
		}
	case zbstore.Importer:
		grp, ctx := errgroup.WithContext(ctx)
		pr, pw := io.Pipe()
		spyReader, spyWriter := io.Pipe()
		grp.Go(func() error {
			err := zbstore.Export(ctx, src, pw, paths, nil)
			pw.CloseWithError(err)
			return err
		})
		grp.Go(func() error {
			err := dst.StoreImport(ctx, io.TeeReader(pr, spyWriter))
			pr.CloseWithError(err)
			spyWriter.CloseWithError(err)
			return err
		})
		grp.Go(func() error {
			defer spyReader.Close()
			zbstore.ReceiveExport(loggingNARReceiver{ctx}, spyReader)
			io.Copy(io.Discard, spyReader) // Drain any remaining data from the pipe.
			return nil
		})
		if err := grp.Wait(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("internal error: unable to copy to %T", dst)
	}

	return nil
}

type loggingNARReceiver struct {
	ctx context.Context
}

func (lnr loggingNARReceiver) ReceiveNAR(t *zbstore.ExportTrailer) {
	log.Infof(lnr.ctx, "Copying %s...", t.StorePath)
}

func (lnr loggingNARReceiver) Write(p []byte) (int, error) {
	return len(p), nil
}

type storeObjectDeleteCommand struct {
	storeDatabaseFlags `kong:"embed"`

	Paths     []zbstore.Path `kong:"arg,name=path,type=nativeStorePath,required,help=Store object paths."`
	Recursive bool           `kong:"short=r,help=Delete objects that depend on the paths."`
}

func (c *storeObjectDeleteCommand) Signature() string {
	return `kong:"help=Delete one or more store objects."`
}

func (c *storeObjectDeleteCommand) Run(ctx context.Context, g *globalConfig) error {
	backendServer := backend.NewServer(g.Directory, c.DBPath, &backend.Options{
		DatabasePoolSize:  1,
		DisableSandbox:    true,
		BuildLogRetention: -1,
	})
	defer backendServer.Close()

	f := backendServer.Delete
	if c.Recursive {
		f = backendServer.DeleteIncludingReferences
	}
	if err := f(ctx, sets.New(c.Paths...)); err != nil {
		return err
	}

	return nil
}

type storeObjectRegisterCommand struct {
	storeDatabaseFlags `kong:"embed"`

	Input io.Reader `kong:"-"`
}

func (c *storeObjectRegisterCommand) Signature() string {
	return `kong:"help=Add info for objects already present in the store directory."`
}

func (c *storeObjectRegisterCommand) BeforeResolve() error {
	c.Input = os.Stdin
	return nil
}

//go:embed docs/store_object_register.txt
var storeObjectRegisterDoc string

func (c *storeObjectRegisterCommand) Help() string {
	return storeObjectRegisterDoc
}

func (c *storeObjectRegisterCommand) Run(ctx context.Context, g *globalConfig) error {
	if err := os.MkdirAll(filepath.Dir(c.DBPath), 0o755); err != nil {
		return err
	}

	backendServer := backend.NewServer(g.Directory, c.DBPath, &backend.Options{
		DatabasePoolSize:            1,
		DisableSandbox:              true,
		BuildLogRetention:           -1,
		ContentAddressBufferCreator: bytebuffer.TempFileCreator{Pattern: contentAddressTempFilePattern},
	})
	defer backendServer.Close()

	s := bufio.NewScanner(c.Input)
	s.Split(splitObjectInfos)
	ok := true
	for info := new(zbstore.ObjectInfo); s.Scan(); {
		err := info.UnmarshalText(s.Bytes())
		if err != nil {
			log.Errorf(ctx, "Invalid object (skipping): %v", err)
			ok = false
			continue
		}
		if err := backendServer.Register(ctx, info); err != nil {
			log.Errorf(ctx, "Failed: %v", err)
			ok = false
		}
	}
	if !ok {
		return fmt.Errorf("one or more objects were not registered")
	}
	return nil
}

func splitObjectInfos(data []byte, atEOF bool) (advance int, token []byte, err error) {
	switch i := bytes.Index(data, []byte("\nStorePath:")); {
	case i >= 0:
		return i + 1, data[:i+1], nil
	case atEOF && len(data) == 0:
		return 0, nil, bufio.ErrFinalToken
	case atEOF && len(data) > 0:
		return len(data), data, bufio.ErrFinalToken
	default:
		return 0, nil, nil
	}
}
