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
	Paths      []zbstore.Path `kong:"name=path,arg,optional"`
	Recursive  bool           `kong:"short=r,help=Show information for referenced objects."`
	JSONFormat bool           `kong:"name=json,help=Print object info as JSON"`
}

func (c *storeObjectInfoCommand) Signature() string {
	return `kong:"help=Show metadata of one or more store objects."`
}

func (c *storeObjectInfoCommand) Run(ctx context.Context, g *globalConfig) error {
	store := g.openLocalStore(ctx)
	defer store.Close()

	p := &objectInfoPrinter{
		w:    os.Stdout,
		json: c.JSONFormat,
	}
	return zbstore.Copy(ctx, p, store, sets.Collect(slices.Values(c.Paths)), &zbstore.ExportOptions{
		ExcludeReferences: !c.Recursive,
	})
}

type objectInfoPrinter struct {
	w          io.Writer
	buf        []byte
	json       bool
	wroteFirst bool
}

func (p *objectInfoPrinter) WriteObject(ctx context.Context, object zbstore.Object) error {
	defer func() { p.wroteFirst = true }()
	if p.json {
		jsonBytes := zbstorerpc.RawObjectInfo(object)
		var err error
		jsonBytes, err = p.addPathToRPCObjectInfo(object.Info().StorePath, jsonBytes)
		if err != nil {
			return fmt.Errorf("%s: %v", object.Info().StorePath, err)
		}
		if _, err := p.w.Write(jsonBytes); err != nil {
			return err
		}
		return nil
	}

	p.buf = p.buf[:0]
	if p.wroteFirst {
		// Blank line between entries.
		p.buf = append(p.buf, '\n')
	}
	var err error
	p.buf, err = object.Info().AppendText(p.buf)
	if err != nil {
		return err
	}
	if _, err := p.w.Write(p.buf); err != nil {
		return err
	}
	return nil
}

func (p *objectInfoPrinter) addPathToRPCObjectInfo(path zbstore.Path, value jsontext.Value) (jsontext.Value, error) {
	out := bytes.NewBuffer(p.buf[:0])
	defer func() { p.buf = out.Bytes()[:0] }()

	dec := jsontext.NewDecoder(bytes.NewBuffer(value))
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	if tok.Kind() != '{' {
		return nil, fmt.Errorf("object info must be a JSON object (got %v)", tok.Kind())
	}

	enc := jsontext.NewEncoder(
		out,
		jsontext.SpaceAfterColon(false),
		jsontext.SpaceAfterComma(false),
	)
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return nil, err
	}
	if err := enc.WriteToken(jsontext.String("path")); err != nil {
		return nil, err
	}
	if err := enc.WriteToken(jsontext.String(string(path))); err != nil {
		return nil, err
	}
keyValues:
	for {
		keyToken, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		switch keyToken.Kind() {
		case '"':
			if err := enc.WriteToken(keyToken); err != nil {
				return nil, err
			}
		case '}':
			if err := enc.WriteToken(keyToken); err != nil {
				return nil, err
			}
			break keyValues
		default:
			return nil, fmt.Errorf("unexpected token %v", keyToken)
		}
		value, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		if err := enc.WriteValue(value); err != nil {
			return nil, err
		}
	}

	return out.Bytes(), nil
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

	recordReader, recordWriter := io.Pipe()
	ch := make(chan []zbstore.Path)
	go func() {
		rec := new(zbstore.InfoRecorder)
		(&zbstore.BufferedImporter{
			ObjectWriter: rec,
			BufferCreator: bytebuffer.CreateFunc(func(size int64) (bytebuffer.ReadWriteSeekCloser, error) {
				return bytebuffer.Null{}, nil
			}),
		}).StoreImport(ctx, recordReader)
		// If we encountered an error, still consume the rest of the stream.
		io.Copy(io.Discard, recordReader)
		recordReader.Close()

		paths := make([]zbstore.Path, 0, len(rec.Written))
		for _, info := range rec.Written {
			paths = append(paths, info.StorePath)
		}
		ch <- paths
	}()

	exportReader, exportWriter := io.Pipe()
	go func() {
		err := catExports(ctx, io.MultiWriter(recordWriter, exportWriter), inputPaths)
		recordWriter.CloseWithError(err)
		exportWriter.CloseWithError(err)
	}()

	err := store.StoreImport(ctx, exportReader)
	exportReader.Close()
	storePaths := <-ch
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

// catExports concatenates the exports from the given files into a single export.
func catExports(ctx context.Context, dst io.Writer, exportFiles []string) error {
	if len(exportFiles) == 1 {
		// If there is a single file, then stream the file directly.
		f, err := openInputFile(exportFiles[0])
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, f)
		f.Close()
		return fmt.Errorf("copying %s: %v", inputFileName(exportFiles[0]), err)
	}

	exporter := zbstore.NewExportWriter(dst)
	for _, path := range exportFiles {
		f, err := openInputFile(path)
		if err != nil {
			return err
		}
		err = exporter.StoreImport(ctx, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("copying %s: %v", inputFileName(path), err)
		}
	}
	return exporter.Close()
}

type storeObjectCopyCommand struct {
	Paths          []zbstore.Path `kong:"arg,name=path,required,help=Store object paths."`
	Source         *storeConfig   `kong:"name=from,short=f,help=Store to copy from (default to local)"`
	Destination    *storeConfig   `kong:"name=to,short=t,help=Store to copy to (default to local)"`
	MaxConcurrency int            `kong:"short=j,help=Limit maximum concurrent requests,default=2"`
}

func (c *storeObjectCopyCommand) Signature() string {
	return `kong:"help=Copy one or more store objects to/from a remote store."`
}

func (c *storeObjectCopyCommand) Validate() error {
	if c.Source.isNull() && c.Destination.isNull() {
		return fmt.Errorf("--from or --to must be specified")
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("--max-concurrency must be >1")
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
		return zbstore.Copy(ctx, destinationStore, localStore, paths, &zbstore.ExportOptions{
			MaxConcurrency: c.MaxConcurrency,
		})
	case !sourceConfig.isNull() && destinationConfig.isNull():
		localStore := g.openLocalStore(ctx)
		defer localStore.Close()
		sourceStore, err := sourceConfig.toStore(func() (zbstorehttp.Client, error) {
			return provideHTTPClient()
		})
		if err != nil {
			return err
		}
		return zbstore.Copy(ctx, localStore, sourceStore, paths, &zbstore.ExportOptions{
			MaxConcurrency: c.MaxConcurrency,
		})
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
		return zbstore.Copy(ctx, destinationStore, sourceStore, paths, &zbstore.ExportOptions{
			MaxConcurrency: c.MaxConcurrency,
		})
	default:
		return c.Validate()
	}
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
