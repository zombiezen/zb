// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/storetest"
	. "zb.256lights.llc/pkg/zbstore"
)

func TestBufferedImporter(t *testing.T) {
	dir := filepath.Join("testdata", "TestBufferedImporter")
	listing, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range listing {
		inputFileName := entry.Name()
		if strings.HasPrefix(inputFileName, ".") {
			continue
		}
		testName, isInput := strings.CutSuffix(inputFileName, ".in")
		if !isInput {
			continue
		}
		inputFileName = filepath.Join(dir, inputFileName)
		txtarFileName := filepath.Join(dir, testName+".txt")

		t.Run(testName, func(t *testing.T) {
			ctx := t.Context()
			input, err := os.ReadFile(inputFileName)
			if err != nil {
				t.Fatal(err)
			}
			archive, err := txtar.ParseFile(txtarFileName)
			if err != nil {
				t.Fatal(err)
			}
			want, _, err := storetest.TxtarObjects(DefaultUnixDirectory, archive.Files)
			if err != nil {
				t.Fatal(err)
			}
			var got storetest.BlobSlice
			if err := got.StoreImport(ctx, bytes.NewReader(input)); err != nil {
				t.Error(err)
			}
			if diff := cmp.Diff(want, got, storetest.TransformSortedSet[Path](), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("objects (-want +got):\n%s", diff)
			}
		})
	}
}
