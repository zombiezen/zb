// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log/testlog"
)

func TestExpression(t *testing.T) {
	tests := []struct {
		expr string
		want map[string]string
	}{
		{
			expr: "nil",
			want: map[string]string{"": "nil"},
		},
		{
			expr: "true",
			want: map[string]string{"": "true"},
		},
		{
			expr: "false",
			want: map[string]string{"": "false"},
		},
		{
			expr: `"foo"`,
			want: map[string]string{"": "foo"},
		},
		{
			expr: `"foo".."bar"`,
			want: map[string]string{"": "foobar"},
		},
		{
			expr: "42",
			want: map[string]string{"": "42"},
		},
		{
			expr: "3.14",
			want: map[string]string{"": "3.14"},
		},
		{
			expr: `{n=0}`,
			want: map[string]string{"n": "0"},
		},
		{
			expr: "{123, 456}",
			want: map[string]string{
				"":  "123",
				"2": "456",
			},
		},
		{
			expr: "{123, nil, 456}",
			want: map[string]string{
				"":  "123",
				"3": "456",
			},
		},
		{
			expr: "{n=3, 123}",
			want: map[string]string{
				"":  "123",
				"n": "3",
			},
		},
		{
			expr: `{}`,
			want: map[string]string{},
		},
		{
			expr: `{foo="bar", baz=42}`,
			want: map[string]string{
				"foo": "bar",
				"baz": "42",
			},
		},
	}

	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			t.Error("eval.Close:", err)
		}
	}()

	for _, test := range tests {
		outputs, err := eval.Expression(ctx, test.expr, system.Current())
		if err != nil {
			t.Errorf("%s: %v", test.expr, err)
			continue
		}
		got := make(map[string]string)
		for k, out := range outputs.All() {
			got[k] = out.String()
		}
		if diff := cmp.Diff(test.want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s (-want +got):\n%s", test.expr, diff)
		}
	}
}

func TestGetenv(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		envOK    bool
		want     string
	}{
		{
			name:     "Success",
			envValue: "foo",
			envOK:    true,
			want:     "foo",
		},
		{
			name:     "Missing",
			envValue: "",
			envOK:    false,
			want:     "nil",
		},
		{
			name:     "Empty",
			envValue: "",
			envOK:    true,
			want:     "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := testcontext.New(t)
			storeDir := backendtest.NewStoreDirectory(t)

			const wantKey = "BAR"
			_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
				TempDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			callCount := 0
			eval, err := NewEval(&Options{
				Store:          &testRPCStore{Client: store},
				StoreDirectory: storeDir,
				LookupEnv: func(ctx context.Context, key string) (string, bool) {
					callCount++
					if key != wantKey {
						t.Errorf("LookupEnv called with %q; want %q", key, wantKey)
					}
					return test.envValue, test.envOK
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := eval.Close(); err != nil {
					t.Error("eval.Close:", err)
				}
			}()

			expr := "os.getenv('" + wantKey + "')"
			got, err := eval.Expression(ctx, expr, system.Current())
			if err != nil {
				t.Fatalf("%s: %v", expr, err)
			}
			if got.Default().String() != test.want {
				t.Errorf("%s = %q; want %q", expr, got, test.want)
			}
		})
	}
}

func TestStringMethod(t *testing.T) {
	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			t.Error("eval.Close:", err)
		}
	}()

	const expr = `("abcdef"):sub(2, 4)`
	got, err := eval.Expression(ctx, expr, system.Current())
	if err != nil {
		t.Fatal(err)
	}
	const want = "bcd"
	if got.Default().String() != want {
		t.Errorf("%s = %s; want %s", expr, got, want)
	}
}

func TestImportExitStore(t *testing.T) {
	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			t.Error("eval.Close:", err)
		}
	}()

	secretPath := filepath.Join(t.TempDir(), "secret.lua")
	if err := os.WriteFile(secretPath, []byte("return \"secret\"\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	fContent := `return import(` + lualex.Quote(secretPath) + `)`
	expr := `local f = toFile("f.lua", ` + lualex.Quote(fContent) + `); local m = await(import(f)); assert(m == nil, string.format("%s is not nil", type(m)))`
	if _, err := eval.Expression(ctx, expr, system.Current()); err != nil {
		t.Fatal(err)
	}
}

func TestStorePath(t *testing.T) {
	archive := txtar.Parse([]byte("" +
		"-- jj24kz6i96lk2arql8642d84zsp210mv-hello.txt --\n" +
		"Hello, World!\n"))

	t.Run("ExistsLocally", func(t *testing.T) {
		ctx := testcontext.New(t)

		storeDir := backendtest.NewStoreDirectory(t)
		objects, _, err := storetest.TxtarObjects(storeDir, archive.Files)
		if err != nil {
			t.Fatal(err)
		}
		exportBuffer := new(bytes.Buffer)
		exporter := zbstore.NewExportWriter(exportBuffer)
		for _, object := range objects {
			if err := exporter.WriteObject(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		if err := exporter.Close(); err != nil {
			t.Fatal(err)
		}

		_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rpcStore := &testRPCStore{Client: store}
		if err := rpcStore.StoreImport(ctx, exportBuffer); err != nil {
			t.Fatal(err)
		}

		eval, err := NewEval(&Options{
			Store:          rpcStore,
			StoreDirectory: storeDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := eval.Close(); err != nil {
				t.Error("eval.Close:", err)
			}
		}()

		wantPath := objects[0].StorePath
		got, err := eval.Expression(ctx, "storePath("+lualex.Quote(string(wantPath))+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		if got.Default().String() != string(wantPath) {
			t.Errorf("storePath(%q) = %s; want %s", wantPath, got, wantPath)
		}
	})

	t.Run("FromFallback", func(t *testing.T) {
		ctx := testcontext.New(t)

		storeDir := backendtest.NewStoreDirectory(t)
		objects, _, err := storetest.TxtarObjects(storeDir, archive.Files)
		if err != nil {
			t.Fatal(err)
		}
		exportBuffer := new(bytes.Buffer)
		exporter := zbstore.NewExportWriter(exportBuffer)
		for _, object := range objects {
			if err := exporter.WriteObject(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		if err := exporter.Close(); err != nil {
			t.Fatal(err)
		}
		fallback := new(storetest.Store)
		if err := fallback.StoreImport(ctx, bytes.NewReader(exportBuffer.Bytes())); err != nil {
			t.Fatal(err)
		}

		_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: backend.Options{
				Fallback: fallback,
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		eval, err := NewEval(&Options{
			Store:          &testRPCStore{Client: store},
			StoreDirectory: storeDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := eval.Close(); err != nil {
				t.Error("eval.Close:", err)
			}
		}()

		wantPath := objects[0].StorePath
		got, err := eval.Expression(ctx, "storePath("+lualex.Quote(string(wantPath))+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		if got.Default().String() != string(wantPath) {
			t.Errorf("storePath(%q) = %s; want %s", wantPath, got, wantPath)
		}
	})
}

func TestExtract(t *testing.T) {
	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			t.Error("eval.Close:", err)
		}
	}()

	path := filepath.Join("testdata", "TestExtract", "extract.lua")
	results, err := eval.URLs(ctx, []string{
		path + "#full",
		path + "#stripped",
	}, system.Current())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Full", func(t *testing.T) {
		ctx := testcontext.New(t)

		refs := results.Group(1).OutputReferences()
		if len(refs) == 0 {
			t.Fatal("No output references for result")
		}
		var response zbstorerpc.RealizeResponse
		err := jsonrpc.Do(ctx, store, zbstorerpc.RealizeMethod, &response, &zbstorerpc.RealizeRequest{
			DrvPaths: slices.Collect(func(yield func(zbstore.Path) bool) {
				for ref := range refs {
					if !yield(ref.DrvPath) {
						return
					}
				}
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		build, err := backendtest.WaitForSuccessfulBuild(ctx, store, response.BuildID)
		if err != nil {
			t.Fatal(err)
		}
		for ref := range refs {
			outputPath, err := build.FindRealizeOutput(ref)
			if err != nil {
				t.Fatal(err)
			}
			if !outputPath.Valid {
				t.Fatalf("missing path for %v", ref)
			}
			got, err := os.ReadFile(filepath.Join(string(outputPath.X), "foo", "bar.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if want := "Hello, World!\n"; string(got) != want {
				t.Errorf("content of %s = %q; want %q", outputPath.X, got, want)
			}
		}
	})

	t.Run("Stripped", func(t *testing.T) {
		ctx := testcontext.New(t)

		refs := results.Group(2).OutputReferences()
		if len(refs) == 0 {
			t.Fatal("No output references for result-2")
		}
		var response zbstorerpc.RealizeResponse
		err := jsonrpc.Do(ctx, store, zbstorerpc.RealizeMethod, &response, &zbstorerpc.RealizeRequest{
			DrvPaths: slices.Collect(func(yield func(zbstore.Path) bool) {
				for ref := range refs {
					if !yield(ref.DrvPath) {
						return
					}
				}
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		build, err := backendtest.WaitForSuccessfulBuild(ctx, store, response.BuildID)
		if err != nil {
			t.Fatal(err)
		}
		for ref := range refs {
			outputPath, err := build.FindRealizeOutput(ref)
			if err != nil {
				t.Fatal(err)
			}
			if !outputPath.Valid {
				t.Fatalf("missing path for %v", ref)
			}
			got, err := os.ReadFile(filepath.Join(string(outputPath.X), "bar.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if want := "Hello, World!\n"; string(got) != want {
				t.Errorf("content of %s = %q; want %q", outputPath.X, got, want)
			}
		}
	})
}

func TestNewState(t *testing.T) {
	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			t.Error("eval.Close:", err)
		}
	}()

	l, err := eval.newState()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error("Close:", err)
		}
	}()

	if got, want := l.Top(), 0; got != want {
		t.Errorf("l.Top() = %d; want %d", got, want)
	}
	if tp, err := l.Global(ctx, "derivation"); err != nil || tp != lua.TypeFunction {
		t.Errorf("l.Global(ctx, \"derivation\") = %v, %v; want function, <nil>", tp, err)
	}
}

// BenchmarkNewState measures the performance of spinning up a new interpreter.
func BenchmarkNewState(b *testing.B) {
	ctx := testcontext.New(b)
	storeDir := backendtest.NewStoreDirectory(b)

	_, store, err := backendtest.NewServer(ctx, b, storeDir, &backendtest.Options{
		TempDir: b.TempDir(),
	})
	if err != nil {
		b.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{Client: store},
		StoreDirectory: storeDir,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := eval.Close(); err != nil {
			b.Error("eval.Close:", err)
		}
	}()

	for b.Loop() {
		l, err := eval.newState()
		if err != nil {
			b.Fatal(err)
		}
		if err := l.Close(); err != nil {
			b.Error(err)
		}
	}
}

// testRPCStore is an implementation of [Store]
// that communicates to a real backend using JSON-RPC.
// Imported paths are tracked.
// Realization logs are ignored.
type testRPCStore struct {
	*zbstorerpc.Client

	mu      sync.Mutex
	imports []zbstore.Path
}

func (store *testRPCStore) readImports() []zbstore.Path {
	store.mu.Lock()
	defer store.mu.Unlock()
	return slices.Clone(store.imports)
}

func (store *testRPCStore) StoreImport(ctx context.Context, r io.Reader) error {
	done := make(chan struct{})
	pr, pw := io.Pipe()
	go func() {
		defer close(done)
		defer pr.Close()
		zbstore.ReceiveExport(exportSpy{store}, pr)
	}()
	err := store.Client.StoreImport(ctx, io.TeeReader(r, pw))
	<-done
	return err
}

func (store *testRPCStore) FetchObjects(ctx context.Context, paths []zbstore.Path) (map[zbstore.Path]*zbstorerpc.ObjectInfo, error) {
	var resp zbstorerpc.FetchResponse
	err := jsonrpc.Do(ctx, store, zbstorerpc.FetchMethod, &resp, &zbstorerpc.FetchRequest{
		Paths: paths,
	})
	if err != nil {
		return nil, err
	}
	return resp.Found, nil
}

func (store *testRPCStore) Realize(ctx context.Context, want sets.Set[zbstore.OutputReference]) ([]*zbstorerpc.BuildResult, error) {
	var realizeResponse zbstorerpc.RealizeResponse
	err := jsonrpc.Do(ctx, store, zbstorerpc.RealizeMethod, &realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: slices.Collect(func(yield func(zbstore.Path) bool) {
			for ref := range want.All() {
				if !yield(ref.DrvPath) {
					return
				}
			}
		}),
		Reuse: &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		return nil, err
	}
	build, err := backendtest.WaitForSuccessfulBuild(ctx, store, realizeResponse.BuildID)
	if err != nil {
		return nil, err
	}
	return build.Results, nil
}

type exportSpy struct {
	store *testRPCStore
}

func (e exportSpy) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (e exportSpy) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	e.store.imports = append(e.store.imports, trailer.StorePath)
}

func TestMain(m *testing.M) {
	testlog.Main(nil)
	os.Exit(m.Run())
}
