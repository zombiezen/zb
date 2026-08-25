// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/xio"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

func TestVerifyObject(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		ca, _, err := SourceSHA256ContentAddress(xio.Null(), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "empty", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, 0, &fakeObject{
			nar: nil,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
			},
		})
	})

	t.Run("NotNAR", func(t *testing.T) {
		badData := []byte("INVALID AS CAN BE")
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(badData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "invalid", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, 8, &fakeObject{
			nar: badData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
			},
		})
	})

	t.Run("SingleSourceFile", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
			},
		})
	})

	t.Run("SelfReference", func(t *testing.T) {
		content := func(digest string) []byte {
			return []byte("It's " + digest + "-hello.txt!\n")
		}

		fakeDigest := strings.Repeat("a", objectNameDigestLength)
		hashNARData := singleFileNAR(t, content(fakeDigest))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(hashNARData), &ContentAddressOptions{
			Digest: fakeDigest,
		})
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{
			Self: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		narData := singleFileNAR(t, content(path.Digest()))

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				References:     *sets.NewSorted(path),
			},
		})
	})

	t.Run("MissingSelfReference", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{
			Self: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				References:     *sets.NewSorted(path),
			},
		})
	})

	t.Run("OtherReference", func(t *testing.T) {
		refNARData := singleFileNAR(t, []byte("Hello, World!\n"))
		refCA, _, err := SourceSHA256ContentAddress(bytes.NewReader(refNARData), nil)
		if err != nil {
			t.Fatal(err)
		}
		refPath, err := FixedCAOutputPath(DefaultUnixDirectory, "ref.txt", refCA, References{})
		if err != nil {
			t.Fatal(err)
		}

		narData := singleFileNAR(t, []byte("Hello, "+refPath.Digest()+"!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{
			Others: *sets.NewSorted(refPath),
		})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				References:     *sets.NewSorted(refPath),
			},
		})
	})

	t.Run("MissingOtherReference", func(t *testing.T) {
		refNARData := singleFileNAR(t, []byte("this file isn't referenced :(\n"))
		refCA, _, err := SourceSHA256ContentAddress(bytes.NewReader(refNARData), nil)
		if err != nil {
			t.Fatal(err)
		}
		refPath, err := FixedCAOutputPath(DefaultUnixDirectory, "ref.txt", refCA, References{})
		if err != nil {
			t.Fatal(err)
		}

		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{
			Others: *sets.NewSorted(refPath),
		})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				References:     *sets.NewSorted(refPath),
			},
		})
	})

	t.Run("FixedHashFile", func(t *testing.T) {
		const content = "Hello, World!\n"
		narData := singleFileNAR(t, []byte(content))
		sum := sha256.Sum256([]byte(content))
		ca := nix.FlatFileContentAddress(nix.NewHash(nix.SHA256, sum[:]))
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
			},
		})
	})

	t.Run("TextFile", func(t *testing.T) {
		const content = "Hello, World!\n"
		narData := singleFileNAR(t, []byte(content))
		sum := sha256.Sum256([]byte(content))
		ca := nix.TextContentAddress(nix.NewHash(nix.SHA256, sum[:]))
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
			},
		})
	})

	t.Run("MismatchedContentAddress", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		badCA, _, err := SourceSHA256ContentAddress(xio.Null(), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: badCA,
			},
		})
	})

	t.Run("MissingContentAddress", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, 0, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath: path,
			},
		})
	})

	t.Run("MismatchedPath", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		badCA, _, err := SourceSHA256ContentAddress(xio.Null(), nil)
		if err != nil {
			t.Fatal(err)
		}
		badPath, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", badCA, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      badPath,
				ContentAddress: ca,
			},
		})
	})

	t.Run("WithSize", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARSize:        int64(len(narData)),
			},
		})
	})

	t.Run("ShorterThanInfoSize", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARSize:        int64(len(narData)) + 8,
			},
		})
	})

	t.Run("LongerThanInfoSize", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyInvalidObject(t, len(narData)-8, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARSize:        int64(len(narData)) - 8,
			},
		})
	})

	t.Run("WithSHA256Hash", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARHash:        nix.NewHash(nix.SHA256, new(sha256.Sum256(narData))[:]),
			},
		})
	})

	t.Run("WithMD5Hash", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}

		verifyValidObject(t, &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARHash:        nix.NewHash(nix.MD5, new(md5.Sum(narData))[:]),
			},
		})
	})

	t.Run("WithMismatchedHash", func(t *testing.T) {
		narData := singleFileNAR(t, []byte("Hello, World!\n"))
		ca, _, err := SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}
		path, err := FixedCAOutputPath(DefaultUnixDirectory, "hello.txt", ca, References{})
		if err != nil {
			t.Fatal(err)
		}
		badSum := sha256.Sum256(append(narData, make([]byte, 8)...))

		verifyInvalidObject(t, len(narData), &fakeObject{
			nar: narData,
			info: ObjectInfo{
				StorePath:      path,
				ContentAddress: ca,
				NARHash:        nix.NewHash(nix.SHA256, badSum[:]),
			},
		})
	})
}

func verifyValidObject(tb testing.TB, object Object) {
	tb.Helper()
	ctx := testcontext.New(tb)

	want := new(bytes.Buffer)
	if err := object.WriteNAR(ctx, want); err != nil {
		tb.Error("WriteNAR to check data:", err)
		return
	}

	got := new(bytes.Buffer)
	if n, err := VerifyObject(ctx, got, object, nil); n != int64(want.Len()) || err != nil {
		tb.Errorf("VerifyObject(...) = %d, %v; want %d, <nil>", n, err, want.Len())
	}

	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		dir := tb.ArtifactDir()
		os.WriteFile(filepath.Join(dir, "want.nar"), want.Bytes(), 0o666)
		artifactPath := filepath.Join(dir, "got.nar")
		os.WriteFile(artifactPath, got.Bytes(), 0o666)
		tb.Errorf("VerifyObject data did not match input NAR. Wrote to %s", artifactPath)
	}
}

func verifyInvalidObject(tb testing.TB, wantSize int, object Object) {
	tb.Helper()
	ctx := testcontext.New(tb)

	narBuffer := new(bytes.Buffer)
	if err := object.WriteNAR(ctx, narBuffer); err != nil {
		tb.Error("WriteNAR to check data:", err)
		return
	}

	got := new(bytes.Buffer)
	if n, err := VerifyObject(ctx, got, object, nil); n != int64(wantSize) || err == nil {
		tb.Errorf("VerifyObject(...) = %d, <nil>; want %d, <error>", n, wantSize)
	} else {
		tb.Log("VerifyObject(...):", err)
	}

	want := narBuffer.Bytes()[:wantSize]
	if !bytes.Equal(got.Bytes(), want) {
		dir := tb.ArtifactDir()
		os.WriteFile(filepath.Join(dir, "want.out"), want, 0o666)
		artifactPath := filepath.Join(dir, "got.out")
		os.WriteFile(artifactPath, got.Bytes(), 0o666)

		if got.Len() != len(want) {
			tb.Errorf("VerifyObject wrote %d bytes; want %d bytes", got.Len(), len(want))
		}
		if n := min(got.Len(), len(want)); !bytes.Equal(got.Bytes()[:n], want[:n]) {
			tb.Error("VerifyObject copied incorrect data")
		}
		tb.Logf("Wrote to %s", artifactPath)
	}
}

func singleFileNAR(tb testing.TB, data []byte) []byte {
	tb.Helper()

	buf := new(bytes.Buffer)
	nw := nar.NewWriter(buf)
	if err := nw.WriteHeader(&nar.Header{Size: int64(len(data))}); err != nil {
		tb.Fatal(err)
	}
	if _, err := nw.Write(data); err != nil {
		tb.Fatal(err)
	}
	if err := nw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// fakeObject is an in-memory implementation of [Object]
// that intentionally permits incorrect information.
type fakeObject struct {
	info ObjectInfo
	nar  []byte
}

func (obj *fakeObject) Info() *ObjectInfo {
	return &obj.info
}

func (obj *fakeObject) WriteNAR(ctx context.Context, w io.Writer) error {
	for i := range obj.nar {
		if _, err := w.Write(obj.nar[i : i+1]); err != nil {
			return err
		}
	}
	return nil
}
