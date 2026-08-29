// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
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

	generateImport := func(dir zbstore.Directory) ([]*zbstore.Blob, []byte, error) {
		ar, err := txtar.ParseFile(filepath.Join("testdata", "TestExport.txt"))
		if err != nil {
			return nil, nil, err
		}
		objects, _, err := storetest.TxtarObjects(dir, ar.Files)
		if err != nil {
			return nil, nil, err
		}
		exportBuffer := new(bytes.Buffer)
		exporter := zbstore.NewExportWriter(exportBuffer)
		for _, obj := range objects {
			if err := exporter.WriteObject(context.Background(), obj); err != nil {
				return nil, nil, err
			}
		}
		if err := exporter.Close(); err != nil {
			return nil, nil, err
		}
		return objects, exportBuffer.Bytes(), nil
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("RPC", func(t *testing.T) {
				ctx := testcontext.New(t)

				dir := backendtest.NewStoreDirectory(t)
				records, importData, err := generateImport(dir)
				if err != nil {
					t.Fatal(err)
				}

				receiver := new(storetest.BlobReceiver)
				_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
					TempDir: t.TempDir(),
					ClientOptions: zbstorerpc.CodecOptions{
						Importer: zbstorerpc.NewReceiverImporter(receiver),
					},
				})
				if err != nil {
					t.Fatal(err)
				}

				// Import test data.
				codec, releaseCodec, err := storeCodec(ctx, client)
				if err != nil {
					t.Fatal(err)
				}
				err = codec.Export(nil, bytes.NewReader(importData))
				releaseCodec()
				if err != nil {
					t.Fatal(err)
				}

				// Call exists method.
				// Exports don't send a response, so this introduces a sync point.
				var exists bool
				lastPath := records[len(records)-1].StorePath
				err = jsonrpc.Do(ctx, client, zbstorerpc.ExistsMethod, &exists, &zbstorerpc.ExistsRequest{
					Path: string(lastPath),
				})
				if err != nil {
					t.Error(err)
				}
				if !exists {
					t.Errorf("store reports exists=false for %s", lastPath)
				}

				// Perform export.
				req := &zbstorerpc.ExportRequest{
					Paths:             make([]zbstore.Path, len(test.paths)),
					ExcludeReferences: test.excludeReferences,
				}
				for i, pathIndex := range test.paths {
					req.Paths[i] = records[pathIndex].StorePath
				}
				if err := jsonrpc.Do(ctx, client, zbstorerpc.ExportMethod, nil, req); err != nil {
					t.Error("Export:", err)
				}

				// Check contents of export.
				want := make([]*zbstore.Blob, len(test.want))
				for i, pathIndex := range test.want {
					want[i] = records[pathIndex]
				}
				diff := cmp.Diff(
					want, receiver.Blobs,
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
					records, importData, err := generateImport(dir)
					if err != nil {
						t.Fatal(err)
					}

					srv, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
						TempDir: t.TempDir(),
						Options: backend.Options{
							RealStoreDirectory: realDir,
						},
					})
					if err != nil {
						t.Fatal(err)
					}

					// Import test data.
					codec, releaseCodec, err := storeCodec(ctx, client)
					if err != nil {
						t.Fatal(err)
					}
					err = codec.Export(nil, bytes.NewReader(importData))
					releaseCodec()
					if err != nil {
						t.Fatal(err)
					}

					// Call exists method.
					// Exports don't send a response, so this introduces a sync point.
					var exists bool
					lastPath := records[len(records)-1].StorePath
					err = jsonrpc.Do(ctx, client, zbstorerpc.ExistsMethod, &exists, &zbstorerpc.ExistsRequest{
						Path: string(lastPath),
					})
					if err != nil {
						t.Error(err)
					}
					if !exists {
						t.Errorf("store reports exists=false for %s", lastPath)
					}

					// Perform export.
					got := new(bytes.Buffer)
					req := &zbstorerpc.ExportRequest{
						Paths:             make([]zbstore.Path, len(test.paths)),
						ExcludeReferences: test.excludeReferences,
					}
					for i, pathIndex := range test.paths {
						req.Paths[i] = records[pathIndex].StorePath
					}
					if err := srv.Export(ctx, got, req); err != nil {
						t.Error("Export:", err)
					}

					// Check contents of export.
					receiver := new(storetest.BlobReceiver)
					if err := zbstore.ReceiveExport(receiver, got); err != nil {
						t.Error("Read export:", err)
					}
					want := make([]*zbstore.Blob, len(test.want))
					for i, pathIndex := range test.want {
						want[i] = records[pathIndex]
					}
					diff := cmp.Diff(
						want, receiver.Blobs,
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
