// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix/nar"
)

func TestExportWriter(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		got := new(bytes.Buffer)
		ew := NewExportWriter(got)
		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}
		verifyExport(t, got.Bytes())
	})

	t.Run("SingleObject", func(t *testing.T) {
		const fileContent = "Hello, World!\n"

		got := new(bytes.Buffer)
		ew := NewExportWriter(got)
		nw := nar.NewWriter(ew)
		err := nw.WriteHeader(&nar.Header{
			Size: int64(len(fileContent)),
		})
		if err != nil {
			t.Error("write NAR header:", err)
		}
		if _, err := nw.Write([]byte(fileContent)); err != nil {
			t.Error("write NAR file content:", err)
		}
		if err := nw.Close(); err != nil {
			t.Error("close NAR:", err)
		}
		err = ew.Trailer(&ExportTrailer{
			StorePath:      "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
			ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:1qicirpsz48j7a2r5h9lj04kipdyvxanwglv9ymfq0qsv7isywdf"),
		})
		if err != nil {
			t.Error("write trailer:", err)
		}
		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}

		verifyExport(t, got.Bytes())
	})

	t.Run("WriteObject", func(t *testing.T) {
		const fileContent = "Hello, World!\n"

		got := new(bytes.Buffer)
		ew := NewExportWriter(got)
		err := ew.WriteObject(t.Context(), &fakeObject{
			nar: singleFileNAR(t, []byte(fileContent)),
			info: ObjectInfo{
				StorePath:      "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
				ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:1qicirpsz48j7a2r5h9lj04kipdyvxanwglv9ymfq0qsv7isywdf"),
			},
		})
		if err != nil {
			t.Error("WriteObject:", err)
		}
		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}

		verifyExport(t, got.Bytes())
	})

	t.Run("TrailerWithReference", func(t *testing.T) {
		const file1Content = "Hello, World!\n"
		const file1Path Path = "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"
		const file2Content = "I have a ref!\n" + string(file1Path) + "\n"

		got := new(bytes.Buffer)
		ew := NewExportWriter(got)

		nw := nar.NewWriter(ew)
		err := nw.WriteHeader(&nar.Header{
			Size: int64(len(file1Content)),
		})
		if err != nil {
			t.Error("write NAR header 1:", err)
		}
		if _, err := nw.Write([]byte(file1Content)); err != nil {
			t.Error("write NAR 1 file content:", err)
		}
		if err := nw.Close(); err != nil {
			t.Error("close NAR 1:", err)
		}
		err = ew.Trailer(&ExportTrailer{
			StorePath:      file1Path,
			ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:1qicirpsz48j7a2r5h9lj04kipdyvxanwglv9ymfq0qsv7isywdf"),
		})
		if err != nil {
			t.Error("write trailer:", err)
		}

		nw = nar.NewWriter(ew)
		err = nw.WriteHeader(&nar.Header{
			Size: int64(len(file2Content)),
		})
		if err != nil {
			t.Error("write NAR 2 header:", err)
		}
		if _, err := nw.Write([]byte(file2Content)); err != nil {
			t.Error("write NAR 2 file content:", err)
		}
		if err := nw.Close(); err != nil {
			t.Error("close NAR 2:", err)
		}

		err = ew.Trailer(&ExportTrailer{
			StorePath:      "/opt/zb/store/cpkdqrc1w4kaklx7881c8c4lw46dzrf7-ref.txt",
			References:     *sets.NewSorted(file1Path),
			ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:0zxvgbzvvxrxx23h50afmv5rzl5xgkbh20rss0xd5319wrd8511y"),
		})
		if err != nil {
			t.Error("write trailer:", err)
		}

		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}

		verifyExport(t, got.Bytes())
	})

	t.Run("StoreImport/SingleObject", func(t *testing.T) {
		const file1Path Path = "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"
		const file2Content = "I have a ref!\n" + string(file1Path) + "\n"
		helloExport, err := os.ReadFile(filepath.Join("testdata", "TestExportWriter", "SingleObject.out"))
		if err != nil {
			t.Fatal(err)
		}

		got := new(bytes.Buffer)
		ew := NewExportWriter(got)

		if err := ew.StoreImport(t.Context(), bytes.NewReader(helloExport)); err != nil {
			t.Error("StoreImport:", err)
		}

		nw := nar.NewWriter(ew)
		err = nw.WriteHeader(&nar.Header{
			Size: int64(len(file2Content)),
		})
		if err != nil {
			t.Error("write NAR 2 header:", err)
		}
		if _, err := nw.Write([]byte(file2Content)); err != nil {
			t.Error("write NAR 2 file content:", err)
		}
		if err := nw.Close(); err != nil {
			t.Error("close NAR 2:", err)
		}

		err = ew.Trailer(&ExportTrailer{
			StorePath:      "/opt/zb/store/cpkdqrc1w4kaklx7881c8c4lw46dzrf7-ref.txt",
			References:     *sets.NewSorted(file1Path),
			ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:0zxvgbzvvxrxx23h50afmv5rzl5xgkbh20rss0xd5319wrd8511y"),
		})
		if err != nil {
			t.Error("write trailer:", err)
		}

		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}

		verifyExport(t, got.Bytes())
	})

	t.Run("StoreImport/ImmediateEOF", func(t *testing.T) {
		const fileContent = "Hello, World!\n"

		got := new(bytes.Buffer)
		ew := NewExportWriter(got)
		if err := ew.StoreImport(t.Context(), strings.NewReader(exportEOFMarker)); err != nil {
			t.Error("StoreImport:", err)
		}
		nw := nar.NewWriter(ew)
		err := nw.WriteHeader(&nar.Header{
			Size: int64(len(fileContent)),
		})
		if err != nil {
			t.Error("write NAR header:", err)
		}
		if _, err := nw.Write([]byte(fileContent)); err != nil {
			t.Error("write NAR file content:", err)
		}
		if err := nw.Close(); err != nil {
			t.Error("close NAR:", err)
		}
		err = ew.Trailer(&ExportTrailer{
			StorePath:      "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
			ContentAddress: mustParseContentAddress(t, "fixed:r:sha256:1qicirpsz48j7a2r5h9lj04kipdyvxanwglv9ymfq0qsv7isywdf"),
		})
		if err != nil {
			t.Error("write trailer:", err)
		}
		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}

		verifyExport(t, got.Bytes())
	})

	t.Run("StoreImport/Empty", func(t *testing.T) {
		ew := NewExportWriter(io.Discard)
		if err := ew.StoreImport(t.Context(), bytebuffer.Null{}); err == nil {
			t.Error("StoreImport did not return error")
		} else {
			t.Log("StoreImport:", err)
		}
	})

	t.Run("StoreImport/ImmediateEOFTrailingData", func(t *testing.T) {
		got := new(bytes.Buffer)
		ew := NewExportWriter(got)
		r := strings.NewReader(exportEOFMarker + "\x00")
		if err := ew.StoreImport(t.Context(), r); err != nil {
			t.Error("StoreImport:", err)
		}
		if r.Len() != 1 {
			t.Errorf("remaining bytes = %d; want 1", r.Len())
		}
		if err := ew.Close(); err != nil {
			t.Error("Close:", err)
		}
		verifyExport(t, got.Bytes())
	})

	t.Run("StoreImport/InvalidMagic", func(t *testing.T) {
		badExport := strings.Repeat("\xba", len(exportObjectMarker))
		ew := NewExportWriter(io.Discard)
		if err := ew.StoreImport(t.Context(), strings.NewReader(badExport)); err == nil {
			t.Error("StoreImport did not return error")
		} else {
			t.Log("StoreImport:", err)
		}
	})

	t.Run("StoreImport/InvalidEOFMagic", func(t *testing.T) {
		badExport, err := os.ReadFile(filepath.Join("testdata", "TestExportWriter", "SingleObject.out"))
		if err != nil {
			t.Fatal(err)
		}
		eofMarker := badExport[len(badExport)-len(exportEOFMarker):]
		for i := range eofMarker {
			eofMarker[i] = '\xba'
		}

		ew := NewExportWriter(io.Discard)
		if err := ew.StoreImport(t.Context(), bytes.NewReader(badExport)); err == nil {
			t.Error("StoreImport did not return error")
		} else {
			t.Log("StoreImport:", err)
		}
	})
}

func verifyExport(tb testing.TB, got []byte) {
	tb.Helper()

	artifactDir, err := filepath.Abs(tb.ArtifactDir())
	if err != nil {
		tb.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "got.out")

	wantPath := filepath.Join("testdata", filepath.FromSlash(tb.Name())+".out")
	wantPath, err = filepath.Abs(wantPath)
	if err != nil {
		tb.Fatal(err)
	}
	want, err := os.ReadFile(wantPath)
	if errors.Is(err, os.ErrNotExist) {
		os.WriteFile(artifactPath, got, 0o666)
		tb.Errorf("%s does not exist. To fix: cp '%s' '%s'", wantPath, artifactPath, wantPath)
		return
	}
	if err != nil {
		tb.Fatal(err)
	}

	if !bytes.Equal(got, want) {
		os.WriteFile(artifactPath, got, 0o666)
		tb.Errorf("Export data %s did not match %s", artifactPath, wantPath)
	}
}
