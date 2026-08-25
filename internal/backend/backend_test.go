// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	. "zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log/testlog"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

func TestImport(t *testing.T) {
	runTest := func(t *testing.T, dir zbstore.Directory, realStoreDir string) {
		ctx := testcontext.New(t)

		ar, err := txtar.ParseFile(filepath.Join("testdata", "TestImport.txt"))
		if err != nil {
			t.Fatal(err)
		}
		objects, _, err := storetest.TxtarObjects(dir, ar.Files)
		if err != nil {
			t.Fatal(err)
		}

		exportBuffer := new(bytes.Buffer)
		exporter := zbstore.NewExportWriter(exportBuffer)
		for _, obj := range objects {
			if err := exporter.WriteObject(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
		if err := exporter.Close(); err != nil {
			t.Fatal(err)
		}

		_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				RealStoreDirectory: realStoreDir,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		codec, releaseCodec, err := storeCodec(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		err = codec.Export(nil, exportBuffer)
		releaseCodec()
		if err != nil {
			t.Fatal(err)
		}

		for _, obj := range objects {
			// Call exists method.
			// Exports don't send a response, so this introduces a sync point.
			var exists bool
			err = jsonrpc.Do(ctx, client, zbstorerpc.ExistsMethod, &exists, &zbstorerpc.ExistsRequest{
				Path: string(obj.StorePath),
			})
			if err != nil {
				t.Error(err)
			}
			if !exists {
				t.Errorf("store reports exists=false for %s", obj.StorePath)
			}

			// Call info method.
			var info zbstorerpc.InfoResponse
			err = jsonrpc.Do(ctx, client, zbstorerpc.InfoMethod, &info, &zbstorerpc.InfoRequest{
				Path: obj.StorePath,
			})
			if err != nil {
				t.Error(err)
			} else {
				want := wantObjectInfo(info.Info, obj)
				if diff := cmp.Diff(want, info.Info); diff != "" {
					t.Errorf("%s info (-want +got):\n%s", obj.StorePath, diff)
				}
			}

			// Verify that store object exists on disk.
			realFilePath := filepath.Join(realStoreDir, obj.StorePath.Base())
			got := new(bytes.Buffer)
			if err := nar.DumpPath(got, realFilePath); err != nil {
				t.Error(err)
			} else if diff := cmp.Diff(obj.NAR, got.Bytes()); diff != "" {
				t.Errorf("%s NAR content (-want +got):\n%s", obj.StorePath, diff)
			}
		}
	}

	t.Run("ActualDir", func(t *testing.T) {
		realStoreDir := t.TempDir()
		storeDir, err := zbstore.CleanDirectory(realStoreDir)
		if err != nil {
			t.Fatal(err)
		}
		runTest(t, storeDir, realStoreDir)
	})

	t.Run("MappedDir", func(t *testing.T) {
		runTest(t, zbstore.DefaultDirectory(), t.TempDir())
	})
}

func TestFetch(t *testing.T) {
	dir := zbstore.DefaultDirectory()
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	narBuffer := new(bytes.Buffer)
	if err := storetest.SingleFileNAR(narBuffer, []byte("Hello, World!\n")); err != nil {
		t.Fatal(err)
	}
	narHash := nix.NewHash(nix.SHA256, new(sha256.Sum256(narBuffer.Bytes()))[:])
	path, ca, err := storetest.ExportSourceNAR(exporter, narBuffer.Bytes(), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	t.Run("NotFound", func(t *testing.T) {
		ctx := testcontext.New(t)
		realStoreDir := t.TempDir()
		_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				RealStoreDirectory: realStoreDir,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		got := new(zbstorerpc.FetchResponse)
		err = jsonrpc.Do(ctx, client, zbstorerpc.FetchMethod, got, &zbstorerpc.FetchRequest{
			Paths: []zbstore.Path{path},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := &zbstorerpc.FetchResponse{
			Found: map[zbstore.Path]*zbstorerpc.ObjectInfo{},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("response (-want +got):\n%s", diff)
		}
	})

	t.Run("ExistsLocally", func(t *testing.T) {
		ctx := testcontext.New(t)
		realStoreDir := t.TempDir()
		_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				RealStoreDirectory: realStoreDir,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		codec, releaseCodec, err := storeCodec(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		err = codec.Export(nil, bytes.NewReader(exportBuffer.Bytes()))
		releaseCodec()
		if err != nil {
			t.Fatal(err)
		}

		got := new(zbstorerpc.FetchResponse)
		err = jsonrpc.Do(ctx, client, zbstorerpc.FetchMethod, got, &zbstorerpc.FetchRequest{
			Paths: []zbstore.Path{path},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := &zbstorerpc.FetchResponse{
			Found: map[zbstore.Path]*zbstorerpc.ObjectInfo{
				path: {
					ContentAddress: ca,
					NARSize:        int64(narBuffer.Len()),
					NARHash:        narHash,
				},
			},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("response (-want +got):\n%s", diff)
		}
	})

	t.Run("FromFallback", func(t *testing.T) {
		ctx := testcontext.New(t)

		fallback := new(storetest.Store)
		if err := fallback.StoreImport(ctx, bytes.NewReader(exportBuffer.Bytes())); err != nil {
			t.Fatal(err)
		}

		realStoreDir := t.TempDir()
		_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				RealStoreDirectory: realStoreDir,
				Fallback:           fallback,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		got := new(zbstorerpc.FetchResponse)
		err = jsonrpc.Do(ctx, client, zbstorerpc.FetchMethod, got, &zbstorerpc.FetchRequest{
			Paths: []zbstore.Path{path},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := &zbstorerpc.FetchResponse{
			Found: map[zbstore.Path]*zbstorerpc.ObjectInfo{
				path: {
					ContentAddress: ca,
					NARSize:        int64(narBuffer.Len()),
					NARHash:        narHash,
				},
			},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("response (-want +got):\n%s", diff)
		}
	})
}

func TestDelete(t *testing.T) {
	dir := zbstore.DefaultDirectory()
	const fileContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	storePath1, _, err := storetest.ExportFlatFile(exporter, dir, "hello.txt", []byte(fileContent), nix.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	storePath2, _, err := storetest.ExportSourceFile(exporter, []byte(storePath1), storetest.SourceExportOptions{
		Name:      "ref.txt",
		Directory: dir,
		References: zbstore.References{
			Others: *sets.NewSorted(storePath1),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	storePath3, _, err := storetest.ExportSourceFile(exporter, []byte(storePath2), storetest.SourceExportOptions{
		Name:      "chain.txt",
		Directory: dir,
		References: zbstore.References{
			Others: *sets.NewSorted(storePath2),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const fakeDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	storeObject4Content := string(storePath2) + "\n" + string(dir) + fakeDigest + "-self.txt\n"
	storePath4, _, err := storetest.ExportSourceFile(exporter, []byte(storeObject4Content), storetest.SourceExportOptions{
		Name:       "self.txt",
		Directory:  dir,
		TempDigest: fakeDigest,
		References: zbstore.References{
			Self:   true,
			Others: *sets.NewSorted(storePath2),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	allObjects := sets.New(storePath1, storePath2, storePath3, storePath4)

	tests := []struct {
		name      string
		paths     sets.Set[zbstore.Path]
		recursive bool
		want      sets.Set[zbstore.Path]
		error     bool
	}{
		{
			name:  "DeleteNothing",
			paths: sets.New[zbstore.Path](),
			want:  allObjects,
		},
		{
			name:  "DeleteRecursiveNothing",
			paths: sets.New[zbstore.Path](),
			want:  allObjects,
		},
		{
			name:  "DeleteNoReverseDeps",
			paths: sets.New(storePath3),
			want:  sets.New(storePath1, storePath2, storePath4),
		},
		{
			name:      "DeleteRecursiveNoReverseDeps",
			recursive: true,
			paths:     sets.New(storePath3),
			want:      sets.New(storePath1, storePath2, storePath4),
		},
		{
			name:  "DeleteSelfDep",
			paths: sets.New(storePath4),
			want:  sets.New(storePath1, storePath2, storePath3),
		},
		{
			name:      "DeleteRecursiveSelfDep",
			recursive: true,
			paths:     sets.New(storePath4),
			want:      sets.New(storePath1, storePath2, storePath3),
		},
		{
			name:  "DeleteWithReverseDeps",
			paths: sets.New(storePath2),
			want:  allObjects,
			error: true,
		},
		{
			name:      "DeleteRecursiveWithReverseDeps",
			paths:     sets.New(storePath2),
			recursive: true,
			want:      sets.New(storePath1),
		},
		{
			name:  "DeleteWithChainOfReverseDeps",
			paths: sets.New(storePath1),
			want:  allObjects,
			error: true,
		},
		{
			name:      "DeleteRecursiveWithChainOfReverseDeps",
			paths:     sets.New(storePath1),
			recursive: true,
			want:      sets.New[zbstore.Path](),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := testcontext.New(t)

			realStoreDir := t.TempDir()
			server, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
				TempDir: t.TempDir(),
				Options: Options{
					RealStoreDirectory: realStoreDir,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			codec, releaseCodec, err := storeCodec(ctx, client)
			if err != nil {
				t.Fatal(err)
			}
			err = codec.Export(nil, bytes.NewReader(exportBuffer.Bytes()))
			releaseCodec()
			if err != nil {
				t.Fatal(err)
			}

			// Call exists method.
			// Exports don't send a response, so this introduces a sync point.
			var exists bool
			err = jsonrpc.Do(ctx, client, zbstorerpc.ExistsMethod, &exists, &zbstorerpc.ExistsRequest{
				Path: string(storePath2),
			})
			if err != nil {
				t.Error(err)
			}
			if !exists {
				t.Errorf("store reports exists=false for %s", storePath2)
			}

			// Perform delete.
			f := server.Delete
			if test.recursive {
				f = server.DeleteIncludingReferences
			}
			if err := f(ctx, test.paths); err != nil {
				t.Log("delete error:", err)
				if !test.error {
					t.Fail()
				}
			} else if test.error {
				t.Error("delete did not return an error")
			}

			// Compare files in store with what we expect.
			storeListing, err := os.ReadDir(realStoreDir)
			if err != nil {
				t.Fatal(err)
			}
			got := make(sets.Set[zbstore.Path])
			for _, ent := range storeListing {
				name := ent.Name()
				path, err := dir.Object(name)
				if err != nil {
					t.Errorf("Unexpected file %s in store (%v)", name, err)
				} else {
					got.Add(path)
				}
			}
			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("files after delete (-want +got):\n%s", diff)
			}

			// Ensure that store has the expected object info.
			for path := range allObjects.All() {
				var resp zbstorerpc.InfoResponse
				err := jsonrpc.Do(ctx, client, zbstorerpc.InfoMethod, &resp, &zbstorerpc.InfoRequest{
					Path: path,
				})
				if err != nil {
					t.Errorf("%s(%q): %v", zbstorerpc.InfoMethod, path, err)
					continue
				}
				if resp.Info != nil && !test.want.Has(path) {
					t.Errorf("store database still has information for %s", path)
				} else if resp.Info == nil && test.want.Has(path) {
					t.Errorf("store database no longer has information for %s", path)
				}
			}
		})
	}
}

// wantObjectInfo builds the expected [*zbstorerpcrpc.ObjectInfo] for the given blob.
// It uses got.NARHash to determine the hashing algorithm to check against.
func wantObjectInfo(got *zbstorerpc.ObjectInfo, blob *zbstore.Blob) *zbstorerpc.ObjectInfo {
	ht := got.NARHash.Type()
	if ht == 0 {
		ht = nix.SHA256
	}
	h := nix.NewHasher(ht)
	h.Write(blob.NAR)
	want := new(*blob.Info())
	want.NARHash = h.SumHash()
	return zbstorerpc.NewObjectInfo(want)
}

func storeCodec(ctx context.Context, client *jsonrpc.Client) (codec *zbstorerpc.Codec, release func(), err error) {
	generic, release, err := client.Codec(ctx)
	if err != nil {
		return nil, nil, err
	}
	codec, ok := generic.(*zbstorerpc.Codec)
	if !ok {
		release()
		return nil, nil, fmt.Errorf("store connection is %T (want %T)", generic, (*zbstorerpc.Codec)(nil))
	}
	return codec, release, nil
}

func TestMain(m *testing.M) {
	testlog.Main(nil)
	os.Exit(m.Run())
}
