// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

// Package backendtest provides utilities for running a [backend.Server] for tests.
package backendtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
)

// NewStoreDirectory returns the path to a newly created, empty [zbstore.Directory]
// that is cleaned up when the test finishes.
// If directory creation fails, NewStoreDirectory terminates the test by calling [testing.TB.Fatal].
// As such, NewStoreDirectory must be called from the goroutine running the test or benchmark function.
func NewStoreDirectory(tb testing.TB) zbstore.Directory {
	tb.Helper()
	realStoreDir, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	storeDir, err := zbstore.CleanDirectory(realStoreDir)
	if err != nil {
		tb.Fatal(err)
	}
	return storeDir
}

// TB is a subset of the [testing.TB] interface that can be safely called from any goroutine.
type TB interface {
	Logf(format string, args ...any)
	Fail()
	Cleanup(func())
}

// Options is the set of optional parameters to [NewServer].
type Options struct {
	backend.Options

	// TempDir is the directory to use to store intermediate build results
	// and the store database.
	// If empty, then a new directory is created and registered for cleanup.
	TempDir string
}

// NewServer creates a new [backend.Server] suitable for testing
// and returns a client connected to it.
// The server and the client will be closed as part of test cleanup.
// If opts is nil, it is treated the same as if it was passed new(Options).
func NewServer(ctx context.Context, tb TB, storeDir zbstore.Directory, opts *Options) (*backend.Server, *zbstorerpc.Client, error) {
	if opts == nil {
		opts = new(Options)
	}
	tempDir := opts.TempDir
	if tempDir == "" {
		var err error
		tempDir, err = os.MkdirTemp("", "zb-backendtest-*")
		if err != nil {
			return nil, nil, err
		}
		tb.Cleanup(func() {
			if err := os.RemoveAll(tempDir); err != nil {
				tb.Logf("%v", err)
			}
		})
	}

	buildDir := filepath.Join(tempDir, "build")
	if err := os.Mkdir(buildDir, 0o777); err != nil {
		return nil, nil, err
	}

	var wg sync.WaitGroup
	opts2 := new(backend.Options)
	if opts != nil {
		*opts2 = opts.Options
	}
	opts2.BuildDirectory = buildDir
	opts2.DisableSandbox = true
	if opts2.CoresPerBuild < 1 {
		opts2.CoresPerBuild = 1
	}
	if opts2.BuildContext == nil {
		opts2.BuildContext = func(_ context.Context, _ string) context.Context {
			return ctx
		}
	}
	realStoreDir := opts2.RealStoreDirectory
	if realStoreDir == "" {
		realStoreDir = string(storeDir)
	}
	srv := backend.NewServer(storeDir, filepath.Join(tempDir, "db.sqlite"), opts2)

	clientCtx, stopClient := context.WithCancel(context.WithoutCancel(ctx))
	client := zbstorerpc.NewClient(clientCtx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			serverReceiver := srv.NewNARReceiver(ctx, bytebuffer.BufferCreator{})
			defer serverReceiver.Cleanup(context.WithoutCancel(ctx))

			var serverImporter struct {
				*backend.Server
				zbstorerpc.Importer
			}
			serverImporter.Server = srv
			serverImporter.Importer = zbstorerpc.NewReceiverImporter(serverReceiver)
			zbstorerpc.Serve(ctx, serverConn, serverImporter)
		})
		return clientConn, nil
	})

	tb.Cleanup(func() {
		stopClient()
		if err := client.Close(); err != nil {
			tb.Logf("client.Close: %v", err)
			tb.Fail()
		}
		wg.Wait()

		if err := srv.Close(); err != nil {
			tb.Logf("srv.Close: %v", err)
			tb.Fail()
		}

		// Make entire store writable for deletion.
		filepath.WalkDir(string(realStoreDir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			perm := os.FileMode(0o666)
			if entry.IsDir() {
				perm = 0o777
			}
			if err := os.Chmod(path, perm); err != nil {
				tb.Logf("%v", err)
			}
			return nil
		})
	})

	return srv, client, nil
}

// WaitForBuild waits until the store finishes a build or the context is canceled,
// whichever comes first.
func WaitForBuild(ctx context.Context, client jsonrpc.Handler, buildID string) (*zbstorerpc.Build, error) {
	if buildID == "" {
		return nil, fmt.Errorf("cannot wait for empty build ID")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp := new(zbstorerpc.Build)
		err := jsonrpc.Do(ctx, client, zbstorerpc.GetBuildMethod, resp, &zbstorerpc.GetBuildRequest{
			BuildID: buildID,
		})
		if err != nil {
			return nil, fmt.Errorf("waiting for build %s: %w", buildID, err)
		}
		if resp.Status != zbstorerpc.BuildActive {
			return resp, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for build %s: %w", buildID, ctx.Err())
		}
	}
}

// WaitForSuccessfulBuild waits until the store finishes a build or the context is canceled,
// whichever comes first.
// If the build status is not [zbstore.BuildSuccess],
// then WaitForSuccessfulBuild returns an error.
func WaitForSuccessfulBuild(ctx context.Context, client jsonrpc.Handler, buildID string) (*zbstorerpc.Build, error) {
	resp, err := WaitForBuild(ctx, client, buildID)
	if err == nil && resp.Status != zbstorerpc.BuildSuccess {
		err = fmt.Errorf("build %s failed with status %q", buildID, resp.Status)
	}
	return resp, err
}

// ReadLog reads the entire log for the given build and derivation path into memory.
func ReadLog(ctx context.Context, client jsonrpc.Handler, buildID string, drvPath zbstore.Path) ([]byte, error) {
	buf := new(bytes.Buffer)
	for {
		resp := new(zbstorerpc.ReadLogResponse)
		err := jsonrpc.Do(ctx, client, zbstorerpc.ReadLogMethod, resp, &zbstorerpc.ReadLogRequest{
			BuildID:    buildID,
			DrvPath:    drvPath,
			RangeStart: int64(buf.Len()),
		})
		if err != nil {
			return buf.Bytes(), fmt.Errorf("read log for %s: %w", drvPath, err)
		}
		payload, err := resp.Payload()
		if err != nil {
			return buf.Bytes(), fmt.Errorf("read log for %s: %w", drvPath, err)
		}
		buf.Write(payload)
		if resp.EOF {
			return bytes.ReplaceAll(buf.Bytes(), []byte("\r\n"), []byte("\n")), nil
		}
	}
}
