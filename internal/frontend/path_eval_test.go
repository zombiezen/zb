// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/zbstore"
)

func TestPath(t *testing.T) {
	t.Run("SingleFile", func(t *testing.T) {
		wantContent, err := os.ReadFile(filepath.Join("testdata", "TestPath", "hello.txt"))
		if err != nil {
			t.Fatal(err)
		}

		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		testStore := &testRPCStore{client: store}
		eval, err := NewEval(&Options{
			Store:          testStore,
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

		for range 3 {
			got, err := eval.Expression(ctx, `path("testdata/TestPath/hello.txt")`, system.Current())
			if err != nil {
				t.Fatal(err)
			}
			gotPath, gotSubpath, err := storeDir.ParsePath(got.Default().String())
			if err != nil {
				t.Fatal(err)
			}
			if gotSubpath != "" {
				t.Errorf("expression result contains subpath %q", gotSubpath)
			}

			gotContent, err := os.ReadFile(filepath.Join(string(storeDir), gotPath.Base()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotContent, wantContent) {
				t.Errorf("content of %s = %q; want %q", gotPath, gotContent, wantContent)
			}

			// Verify that we only attempted a single import during the whole test.
			if diff := cmp.Diff([]zbstore.Path{gotPath}, testStore.readImports()); diff != "" {
				t.Errorf("imported paths (-want +got):\n%s", diff)
			}
		}
	})

	t.Run("FileChange", func(t *testing.T) {
		const wantContent1 = "AAA\n"
		const wantContent2 = "BBB\n"

		myPath := filepath.Join(t.TempDir(), "file.txt")
		if err := os.WriteFile(myPath, []byte(wantContent1), 0o666); err != nil {
			t.Fatal(err)
		}

		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		testStore := &testRPCStore{client: store}
		eval, err := NewEval(&Options{
			Store:          testStore,
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

		got1, err := eval.Expression(ctx, "path("+lualex.Quote(myPath)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath1, gotSubpath1, err := storeDir.ParsePath(got1.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath1 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath1)
		}

		gotContent1, err := os.ReadFile(filepath.Join(string(storeDir), gotPath1.Base()))
		if err != nil {
			t.Fatal(err)
		}
		if string(gotContent1) != wantContent1 {
			t.Errorf("content of %s = %q; want %q", gotPath1, gotContent1, wantContent1)
		}

		if err := os.WriteFile(myPath, []byte(wantContent2), 0o666); err != nil {
			t.Fatal(err)
		}
		got2, err := eval.Expression(ctx, "path("+lualex.Quote(myPath)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		if got1.Default().String() == got2.Default().String() {
			t.Errorf("first path (%q) == second path", got1)
		}
		gotPath2, gotSubpath2, err := storeDir.ParsePath(got2.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath2 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath2)
		}

		gotContent2, err := os.ReadFile(filepath.Join(string(storeDir), gotPath2.Base()))
		if err != nil {
			t.Fatal(err)
		}
		if string(gotContent2) != wantContent2 {
			t.Errorf("content of %s = %q; want %q", gotPath2, gotContent2, wantContent2)
		}
	})

	t.Run("Directory", func(t *testing.T) {
		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		testStore := &testRPCStore{client: store}
		eval, err := NewEval(&Options{
			Store:          testStore,
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

		for range 3 {
			got, err := eval.Expression(ctx, `path("testdata/TestPath/dir")`, system.Current())
			if err != nil {
				t.Fatal(err)
			}
			gotPath, gotSubpath, err := storeDir.ParsePath(got.Default().String())
			if err != nil {
				t.Fatal(err)
			}
			if gotSubpath != "" {
				t.Errorf("expression result contains subpath %q", gotSubpath)
			}
			compareDirectoryToTestdata(t, string(gotPath), "a.txt", "b.txt", "c", "c/d.txt")

			// Verify that we only attempted a single import during the whole test.
			if diff := cmp.Diff([]zbstore.Path{gotPath}, testStore.readImports()); diff != "" {
				t.Errorf("imported paths (-want +got):\n%s", diff)
			}
		}
	})

	t.Run("DirectoryAdd", func(t *testing.T) {
		myDir := t.TempDir()
		if err := copyFile(filepath.Join("testdata", "TestPath", "dir", "a.txt"), filepath.Join(myDir, "a.txt")); err != nil {
			t.Fatal(err)
		}

		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		eval, err := NewEval(&Options{
			Store:          &testRPCStore{client: store},
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

		got1, err := eval.Expression(ctx, "path("+lualex.Quote(myDir)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath1, gotSubpath1, err := storeDir.ParsePath(got1.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath1 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath1)
		}
		compareDirectoryToTestdata(t, string(gotPath1), "a.txt")

		if err := copyFile(filepath.Join("testdata", "TestPath", "dir", "b.txt"), filepath.Join(myDir, "b.txt")); err != nil {
			t.Fatal(err)
		}

		got2, err := eval.Expression(ctx, "path("+lualex.Quote(myDir)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath2, gotSubpath2, err := storeDir.ParsePath(got2.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath2 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath2)
		}
		compareDirectoryToTestdata(t, string(gotPath2), "a.txt", "b.txt")
	})

	t.Run("DirectoryRemove", func(t *testing.T) {
		myDir := t.TempDir()
		if err := copyFile(filepath.Join("testdata", "TestPath", "dir", "a.txt"), filepath.Join(myDir, "a.txt")); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(filepath.Join("testdata", "TestPath", "dir", "b.txt"), filepath.Join(myDir, "b.txt")); err != nil {
			t.Fatal(err)
		}

		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		eval, err := NewEval(&Options{
			Store:          &testRPCStore{client: store},
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

		got1, err := eval.Expression(ctx, "path("+lualex.Quote(myDir)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath1, gotSubpath1, err := storeDir.ParsePath(got1.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath1 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath1)
		}
		compareDirectoryToTestdata(t, string(gotPath1), "a.txt", "b.txt")

		if err := os.Remove(filepath.Join(myDir, "b.txt")); err != nil {
			t.Fatal(err)
		}

		got2, err := eval.Expression(ctx, "path("+lualex.Quote(myDir)+")", system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath2, gotSubpath2, err := storeDir.ParsePath(got2.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath2 != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath2)
		}
		compareDirectoryToTestdata(t, string(gotPath2), "a.txt")
	})

	t.Run("FilteredDirectory", func(t *testing.T) {
		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		eval, err := NewEval(&Options{
			Store:          &testRPCStore{client: store},
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

		got, err := eval.Expression(ctx, `path{path = "testdata/TestPath/dir"; filter = function(name) return name ~= "c/d.txt" end }`, system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath, gotSubpath, err := storeDir.ParsePath(got.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath)
		}

		compareDirectoryToTestdata(t, string(gotPath), "a.txt", "b.txt", "c")
	})

	t.Run("FilteredFile", func(t *testing.T) {
		wantContent, err := os.ReadFile(filepath.Join("testdata", "TestPath", "hello.txt"))
		if err != nil {
			t.Fatal(err)
		}

		ctx := testcontext.New(t)
		storeDir := backendtest.NewStoreDirectory(t)

		store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
			TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		eval, err := NewEval(&Options{
			Store:          &testRPCStore{client: store},
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

		got, err := eval.Expression(ctx, `path{path = "testdata/TestPath/hello.txt"; filter = function() return false end }`, system.Current())
		if err != nil {
			t.Fatal(err)
		}
		gotPath, gotSubpath, err := storeDir.ParsePath(got.Default().String())
		if err != nil {
			t.Fatal(err)
		}
		if gotSubpath != "" {
			t.Errorf("expression result contains subpath %q", gotSubpath)
		}

		gotContent, err := os.ReadFile(filepath.Join(string(storeDir), gotPath.Base()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotContent, wantContent) {
			t.Errorf("content of %s = %q; want %q", gotPath, gotContent, wantContent)
		}
	})
}

func TestReadFile(t *testing.T) {
	wantContent, err := os.ReadFile(filepath.Join("testdata", "TestPath", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}

	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	testStore := &testRPCStore{client: store}
	eval, err := NewEval(&Options{
		Store:          testStore,
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

	got, err := eval.Expression(ctx, `readFile("testdata/TestPath/hello.txt")`, system.Current())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got.Default().String()), wantContent) {
		t.Errorf("got = %q; want %q", got, wantContent)
	}
}

// compareDirectoryToTestdata compares dir to the directory at testdata/dir.
// If dir does not contain exactly the files named in wantFiles,
// then compareDirectoryToTestdata logs a failure to tb.
func compareDirectoryToTestdata(tb testing.TB, dir string, wantFiles ...string) {
	tb.Helper()

	for _, name := range wantFiles {
		goldenPath := filepath.Join("testdata", "TestPath", "dir", filepath.FromSlash(name))
		wantInfo, err := os.Lstat(goldenPath)
		if err != nil {
			tb.Error(err)
			continue
		}

		testPath := filepath.Join(dir, filepath.FromSlash(name))
		gotInfo, err := os.Lstat(testPath)
		if err != nil {
			tb.Errorf("file %s: %v", name, err)
			continue
		}
		wantMode := wantInfo.Mode()
		gotMode := gotInfo.Mode()
		if wantMode.Type() != gotMode.Type() {
			tb.Errorf("file %s: mode is %v; want %v", name, gotMode, wantMode)
			continue
		}

		switch wantMode.Type() {
		case 0:
			if (wantMode&0o111 == 0) != (gotMode&0o111 == 0) {
				tb.Errorf("file %s: mode is %v; want %v", name, gotMode, wantMode)
			}
			wantContent, err := os.ReadFile(goldenPath)
			if err != nil {
				tb.Error(err)
				continue
			}
			gotContent, err := os.ReadFile(testPath)
			if err != nil {
				tb.Fatal(err)
			}
			if !bytes.Equal(gotContent, wantContent) {
				tb.Errorf("file %s: content = %q; want %q", name, gotContent, wantContent)
			}
		case os.ModeDir:
			// Nothing additional to check.
		default:
			tb.Errorf("file %s: unsupported mode %v", name, wantMode)
		}
	}

	err := filepath.WalkDir(dir, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			tb.Error(err)
			return nil
		}
		if path == dir {
			return nil
		}
		got := filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))
		if !slices.Contains(wantFiles, got) {
			tb.Errorf("unexpected file %q", got)
		}
		return nil
	})
	if err != nil {
		tb.Error(err)
	}
}

func TestCollatePath(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "a", -1},
		{"a", "", 1},
		{"a", "a", 0},
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a/b", -1},
		{"a/b", "a", 1},
		{"a!b", "a/b", 1},
		{"a/b", "a!b", -1},
	}
	for _, test := range tests {
		if got := collatePath(test.a, test.b); got != test.want {
			t.Errorf("collatePath(%q, %q) = %d; want %d", test.a, test.b, got, test.want)
		}
	}
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return &os.LinkError{
			Op:  "copy",
			Old: src,
			New: dst,
			Err: err,
		}
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return &os.LinkError{
			Op:  "copy",
			Old: src,
			New: dst,
			Err: err,
		}
	}
	_, writeError := io.Copy(dstFile, srcFile)
	closeError := dstFile.Close()
	if writeError != nil {
		return &os.LinkError{
			Op:  "copy",
			Old: src,
			New: dst,
			Err: writeError,
		}
	}
	if closeError != nil {
		return &os.LinkError{
			Op:  "copy",
			Old: src,
			New: dst,
			Err: closeError,
		}
	}
	return nil
}
