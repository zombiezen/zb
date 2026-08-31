// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package storetest

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

func TestEmptyStore(t *testing.T) {
	ctx := testcontext.New(t)

	path, err := zbstore.DefaultUnixDirectory.Object("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	store := new(Store)

	if _, err := store.Object(ctx, path); !errors.Is(err, zbstore.ErrNotFound) {
		t.Errorf("new(Store).Object(ctx, %s) = _, %v; want <nil>, %v", path, err, zbstore.ErrNotFound)
	}
}

func TestWriteObject(t *testing.T) {
	ctx := testcontext.New(t)

	archive := txtar.Parse([]byte("" +
		"-- hz8i7yfcbsgz7h3zj7mnk9r1jln7ywjh-hello.txt --\n" +
		"Hello, World!\n"))
	objects, _, err := TxtarObjects(zbstore.DefaultUnixDirectory, archive.Files)
	if err != nil {
		t.Fatal(err)
	}

	store := new(Store)
	if err := store.WriteObject(ctx, objects[0]); err != nil {
		t.Error(err)
	}

	t.Run("Object", func(t *testing.T) {
		obj, err := store.Object(ctx, objects[0].StorePath)
		if err != nil {
			t.Fatal(err)
		}
		info := obj.Info()
		if info.StorePath != objects[0].StorePath {
			t.Errorf("obj.Info().StorePath = %q; want %q", info.StorePath, objects[0].StorePath)
		}
		if info.NARSize != int64(len(objects[0].NAR)) {
			t.Errorf("obj.Info().NARSize = %d; want %d", info.NARSize, len(objects[0].NAR))
		}
		if !info.ContentAddress.Equal(objects[0].ContentAddress) {
			t.Errorf("obj.Info().ContentAddress = %v; want %v", info.ContentAddress, objects[0].ContentAddress)
		}

		got := new(bytes.Buffer)
		if err := obj.WriteNAR(ctx, got); err != nil {
			t.Error("obj.WriteNAR:", err)
		}
		if !bytes.Equal(got.Bytes(), objects[0].NAR) {
			t.Error("NAR content does not match")
		}
	})

	t.Run("ObjectBatch", func(t *testing.T) {
		batch, err := store.ObjectBatch(ctx, sets.New(objects[0].StorePath))
		if err != nil {
			t.Error("ObjectBatch:", err)
		}
		if len(batch) != 1 {
			t.Errorf("len(batch) == %d; want 1", len(batch))
		}

		obj := batch[0]
		info := obj.Info()
		if info.StorePath != objects[0].StorePath {
			t.Errorf("batch[0].Info().StorePath = %q; want %q", info.StorePath, objects[0].StorePath)
		}
		if info.NARSize != int64(len(objects[0].NAR)) {
			t.Errorf("batch[0].Info().NARSize = %d; want %d", info.NARSize, len(objects[0].NAR))
		}
		if !info.ContentAddress.Equal(objects[0].ContentAddress) {
			t.Errorf("batch[0].Info().ContentAddress = %v; want %v", info.ContentAddress, objects[0].ContentAddress)
		}

		got := new(bytes.Buffer)
		if err := obj.WriteNAR(ctx, got); err != nil {
			t.Error("batch[0].WriteNAR:", err)
		}
		if !bytes.Equal(got.Bytes(), objects[0].NAR) {
			t.Error("NAR content does not match")
		}
	})
}

func TestStoreImport(t *testing.T) {
	ctx := testcontext.New(t)

	archive := txtar.Parse([]byte("" +
		"-- hz8i7yfcbsgz7h3zj7mnk9r1jln7ywjh-hello.txt --\n" +
		"Hello, World!\n"))
	objects, _, err := TxtarObjects(zbstore.DefaultUnixDirectory, archive.Files)
	if err != nil {
		t.Fatal(err)
	}
	exportBuffer := new(bytes.Buffer)
	exp := zbstore.NewExportWriter(exportBuffer)
	if err := exp.WriteObject(ctx, objects[0]); err != nil {
		t.Fatal(err)
	}
	if err := exp.Close(); err != nil {
		t.Fatal(err)
	}

	store := new(Store)
	if err := store.StoreImport(ctx, exportBuffer); err != nil {
		t.Error(err)
	}

	t.Run("Object", func(t *testing.T) {
		obj, err := store.Object(ctx, objects[0].StorePath)
		if err != nil {
			t.Fatal(err)
		}
		info := obj.Info()
		if info.StorePath != objects[0].StorePath {
			t.Errorf("obj.Info().StorePath = %q; want %q", info.StorePath, objects[0].StorePath)
		}
		if info.NARSize != int64(len(objects[0].NAR)) {
			t.Errorf("obj.Info().NARSize = %d; want %d", info.NARSize, len(objects[0].NAR))
		}
		if !info.ContentAddress.Equal(objects[0].ContentAddress) {
			t.Errorf("obj.Info().ContentAddress = %v; want %v", info.ContentAddress, objects[0].ContentAddress)
		}

		got := new(bytes.Buffer)
		if err := obj.WriteNAR(ctx, got); err != nil {
			t.Error("obj.WriteNAR:", err)
		}
		if !bytes.Equal(got.Bytes(), objects[0].NAR) {
			t.Error("NAR content does not match")
		}
	})

	t.Run("ObjectBatch", func(t *testing.T) {
		batch, err := store.ObjectBatch(ctx, sets.New(objects[0].StorePath))
		if err != nil {
			t.Error("ObjectBatch:", err)
		}
		if len(batch) != 1 {
			t.Errorf("len(batch) == %d; want 1", len(batch))
		}

		obj := batch[0]
		info := obj.Info()
		if info.StorePath != objects[0].StorePath {
			t.Errorf("batch[0].Info().StorePath = %q; want %q", info.StorePath, objects[0].StorePath)
		}
		if info.NARSize != int64(len(objects[0].NAR)) {
			t.Errorf("batch[0].Info().NARSize = %d; want %d", info.NARSize, len(objects[0].NAR))
		}
		if !info.ContentAddress.Equal(objects[0].ContentAddress) {
			t.Errorf("batch[0].Info().ContentAddress = %v; want %v", info.ContentAddress, objects[0].ContentAddress)
		}

		got := new(bytes.Buffer)
		if err := obj.WriteNAR(ctx, got); err != nil {
			t.Error("batch[0].WriteNAR:", err)
		}
		if !bytes.Equal(got.Bytes(), objects[0].NAR) {
			t.Error("NAR content does not match")
		}
	})
}
