// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"

	. "zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
)

func TestImport(t *testing.T) {
	t.Parallel()

	t.Run("ActualDir", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		dir := backendtest.NewStoreDirectory(t)

		server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}

		data, err := readTestData(dir, "TestImport.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := data.writeTo(ctx, server, nil); err != nil {
			t.Fatal(err)
		}
		runScriptTest(ctx, t, dir, server, data, nil)
	})

	t.Run("MappedDir", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		realStoreDir := t.TempDir()
		dir := zbstore.DefaultDirectory()

		server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				RealStoreDirectory: realStoreDir,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		data, err := readTestData(dir, "TestImport.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := data.writeTo(ctx, server, nil); err != nil {
			t.Fatal(err)
		}
		runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
			realDirectory: realStoreDir,
		})
	})

	t.Run("RPC", func(t *testing.T) {
		t.Parallel()

		ctx := testcontext.New(t)
		dir := backendtest.NewStoreDirectory(t)

		server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		defer wg.Wait()
		client := zbstorerpc.NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
			serverConn, clientConn := net.Pipe()
			wg.Go(func() {
				var serverImporter struct {
					*Server
					zbstorerpc.Importer
				}
				serverImporter.Server = server
				serverImporter.Importer = &zbstore.BufferedImporter{
					ObjectWriter: server,
				}
				zbstorerpc.Serve(ctx, serverConn, serverImporter)
			})
			return clientConn, nil
		})
		defer func() {
			if err := client.Close(); err != nil {
				t.Error("client.Close:", err)
			}
		}()

		data, err := readTestData(dir, "TestImport.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := data.writeTo(ctx, client, nil); err != nil {
			t.Fatal(err)
		}
		runScriptTest(ctx, t, dir, server, data, nil)
	})
}
