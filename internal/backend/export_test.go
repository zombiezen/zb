// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

func TestExport(t *testing.T) {
	const (
		noDepsPath             = 0
		directDependencyPath   = 1
		indirectDependencyPath = 2
		selfDependencyPath     = 3
	)
	tests := []struct {
		name              string
		paths             []int
		excludeReferences bool
		want              []int
	}{
		{
			name:  "EmptyList",
			paths: []int{},
			want:  []int{},
		},
		{
			name:  "IndependentPath",
			paths: []int{noDepsPath},
			want:  []int{noDepsPath},
		},
		{
			name:  "SelfDependencyPath",
			paths: []int{selfDependencyPath},
			want:  []int{selfDependencyPath},
		},
		{
			name:  "DirectDependencyPath",
			paths: []int{directDependencyPath},
			want:  []int{noDepsPath, directDependencyPath},
		},
		{
			name:  "IndirectDependencyPath",
			paths: []int{indirectDependencyPath},
			want:  []int{noDepsPath, directDependencyPath, indirectDependencyPath},
		},
		{
			name:              "IndirectDependencyPathExcludeReferences",
			paths:             []int{indirectDependencyPath},
			excludeReferences: true,
			want:              []int{indirectDependencyPath},
		},
		{
			name:  "Deduplicate",
			paths: []int{noDepsPath, directDependencyPath},
			want:  []int{noDepsPath, directDependencyPath},
		},
		{
			name:  "DeduplicateAndReorder",
			paths: []int{directDependencyPath, noDepsPath},
			want:  []int{noDepsPath, directDependencyPath},
		},
	}

	readExportTestData := func(dir zbstore.Directory) (storetest.BlobSlice, error) {
		ar, err := txtar.ParseFile(filepath.Join("testdata", "TestExport.txt"))
		if err != nil {
			return nil, err
		}
		txtarStore, err := storetest.TxtarObjects(dir, ar.Files)
		if err != nil {
			return nil, err
		}
		return txtarStore.BlobSlice, nil
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("RPC", func(t *testing.T) {
				ctx := testcontext.New(t)

				dir := backendtest.NewStoreDirectory(t)
				records, err := readExportTestData(dir)
				if err != nil {
					t.Fatal(err)
				}

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
							*backend.Server
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

				for _, record := range records {
					if err := server.WriteObject(ctx, record); err != nil {
						t.Fatal(err)
					}
				}

				// Perform export.
				paths := make(sets.Set[zbstore.Path])
				for _, pathIndex := range test.paths {
					paths.Add(records[pathIndex].StorePath)
				}
				buf := new(bytes.Buffer)
				err = client.StoreExport(ctx, buf, paths, &zbstore.ExportOptions{
					ExcludeReferences: test.excludeReferences,
				})
				if err != nil {
					t.Error(err)
				}
				var got storetest.BlobSlice
				if err := got.StoreImport(ctx, buf); err != nil {
					t.Error(err)
				}

				// Check contents of export.
				want := make(storetest.BlobSlice, len(test.want))
				for i, pathIndex := range test.want {
					want[i] = records[pathIndex]
				}
				diff := cmp.Diff(
					want, got,
					cmpopts.EquateEmpty(),
					storetest.TransformSortedSet[zbstore.Path](),
				)
				if diff != "" {
					t.Errorf("export (-want +got):\n%s", diff)
				}
			})

			for _, mapped := range [...]bool{false, true} {
				var mapTestName string
				if mapped {
					mapTestName = "Mapped"
				} else {
					mapTestName = "Real"
				}

				t.Run(mapTestName, func(t *testing.T) {
					ctx := testcontext.New(t)

					var dir zbstore.Directory
					var realDir string
					if mapped {
						dir = zbstore.DefaultDirectory()
						realDir = t.TempDir()
					} else {
						dir = backendtest.NewStoreDirectory(t)
						realDir = string(dir)
					}
					records, err := readExportTestData(dir)
					if err != nil {
						t.Fatal(err)
					}

					server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
						TempDir: t.TempDir(),
						Options: backend.Options{
							RealStoreDirectory: realDir,
						},
					})
					if err != nil {
						t.Fatal(err)
					}

					for _, record := range records {
						if err := server.WriteObject(ctx, record); err != nil {
							t.Fatal(err)
						}
					}

					// Perform export.
					exportBuffer := new(bytes.Buffer)
					paths := make(sets.Set[zbstore.Path], len(test.paths))
					for _, pathIndex := range test.paths {
						paths.Add(records[pathIndex].StorePath)
					}
					err = server.StoreExport(ctx, exportBuffer, paths, &zbstore.ExportOptions{
						ExcludeReferences: test.excludeReferences,
					})
					if err != nil {
						t.Error("Export:", err)
					}

					// Check contents of export.
					var got storetest.BlobSlice
					if err := got.StoreImport(ctx, exportBuffer); err != nil {
						t.Error("Read export:", err)
					}
					want := make(storetest.BlobSlice, len(test.want))
					for i, pathIndex := range test.want {
						want[i] = records[pathIndex]
					}
					diff := cmp.Diff(
						want, got,
						cmpopts.EquateEmpty(),
						storetest.TransformSortedSet[zbstore.Path](),
					)
					if diff != "" {
						t.Errorf("export (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}
