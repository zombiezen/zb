// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorehttp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log/testlog"
	"zombiezen.com/go/nix"
)

func TestStoreObject(t *testing.T) {
	t.Run("File", func(t *testing.T) {
		ctx := testcontext.New(t)
		store := httpStoreForDirectory(t, ".")
		caHash, err := nix.ParseHash("sha256:1qicirpsz48j7a2r5h9lj04kipdyvxanwglv9ymfq0qsv7isywdf")
		if err != nil {
			t.Fatal(err)
		}
		wantHash, err := nix.ParseHash("sha256:0xmvxmsmmc6n79sk2h3r6db3yp8drmxps61mdk7iqnvc6vcsww60")
		if err != nil {
			t.Fatal(err)
		}

		obj, err := store.Object(ctx, "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt")
		if err != nil {
			t.Fatal(err)
		}
		wantInfo := &zbstore.ObjectInfo{
			StorePath:      "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
			NARHash:        wantHash,
			NARSize:        128,
			ContentAddress: nix.RecursiveFileContentAddress(caHash),
		}
		if diff := cmp.Diff(wantInfo, obj.Info(), storetest.TransformSortedSet[zbstore.Path]()); diff != "" {
			t.Errorf("object info (-want +got):\n%s", diff)
		}
		h := nix.NewHasher(nix.SHA256)
		if err := obj.WriteNAR(ctx, h); err != nil {
			t.Error("write nar:", err)
		} else if gotHash := h.SumHash(); !gotHash.Equal(wantHash) {
			t.Errorf("written nar hash = %v; want %v", gotHash, wantHash)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		ctx := testcontext.New(t)
		store := httpStoreForDirectory(t, ".")
		_, err := store.Object(ctx, "/opt/zb/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bork")
		if err == nil {
			t.Error("no error returned")
		} else if !errors.Is(err, zbstore.ErrNotFound) {
			t.Error("unexpected error:", err)
		}
	})
}

func TestStoreFetchRealizations(t *testing.T) {
	t.Run("File", func(t *testing.T) {
		ctx := testcontext.New(t)
		store := httpStoreForDirectory(t, ".")
		drvHash := mustParseHash(t, "sha256:bd172e7b837e02a672e417976696642eaabb97847f61a77cf430f515efc97b61")
		got, err := store.FetchRealizations(ctx, drvHash)
		if err != nil {
			t.Error(err)
		}
		want := zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					{
						OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
					},
				},
			},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("realizations (-want +got):\n%s", diff)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		ctx := testcontext.New(t)
		store := httpStoreForDirectory(t, ".")
		drvHash := mustParseHash(t, "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
		got, err := store.FetchRealizations(ctx, drvHash)
		if err != nil {
			t.Error(err)
		}
		if len(got.Realizations) > 0 {
			realizationsJSON, _ := jsonv2.Marshal(got)
			t.Errorf("realizations = %s; want {}", realizationsJSON)
		}
		if !got.DerivationHash.Equal(drvHash) {
			t.Errorf("derivation hash = %v; want %v", got.DerivationHash, drvHash)
		}
	})
}

func TestStorePutRealizations(t *testing.T) {
	t.Run("Create", func(t *testing.T) {
		ctx := testcontext.New(t)

		dir := t.TempDir()
		copyToDir(t, dir, "discovery.json")
		discoveryPath, err := filepath.Abs(filepath.Join(dir, "discovery.json"))
		if err != nil {
			t.Fatal(err)
		}
		discoveryURL := fileurl.FromPath(discoveryPath)
		store := &Store{
			URL: discoveryURL,
			HTTPClient: &http.Client{
				Transport: fileurl.Transport{},
			},
		}

		drvHash := mustParseHash(t, "sha256:bd172e7b837e02a672e417976696642eaabb97847f61a77cf430f515efc97b61")
		err = store.PutRealizations(ctx, zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					{
						OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
					},
				},
			},
		})
		if err != nil {
			t.Error("PutRealizations:", err)
		}

		gotData, err := os.ReadFile(filepath.Join(dir, "realizations", "0qbvr7pibx9hyiyafqbzhjbvpaifcjb6d5qpwirac0kyhdxjw5xx.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got zbstore.RealizationMap
		unmarshalers := jsonv2.UnmarshalFromFunc(zbstore.UnmarshalHashJSONFrom)
		if err := jsonv2.Unmarshal(gotData, &got, jsonv2.WithUnmarshalers(unmarshalers)); err != nil {
			t.Fatal(err)
		}
		want := zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					{
						OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
					},
				},
			},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("realizations (-want +got):\n%s", diff)
		}
	})

	t.Run("Update", func(t *testing.T) {
		ctx := testcontext.New(t)

		dir := t.TempDir()
		copyToDir(t, dir, "discovery.json")
		copyToDir(t, dir, "realizations/0qbvr7pibx9hyiyafqbzhjbvpaifcjb6d5qpwirac0kyhdxjw5xx.json")
		discoveryPath, err := filepath.Abs(filepath.Join(dir, "discovery.json"))
		if err != nil {
			t.Fatal(err)
		}
		discoveryURL := fileurl.FromPath(discoveryPath)
		store := &Store{
			URL: discoveryURL,
			HTTPClient: &http.Client{
				Transport: fileurl.Transport{},
			},
		}

		drvHash := mustParseHash(t, "sha256:bd172e7b837e02a672e417976696642eaabb97847f61a77cf430f515efc97b61")
		err = store.PutRealizations(ctx, zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					{
						OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
						Signatures: []*zbstore.RealizationSignature{
							{
								PublicKey: zbstore.RealizationPublicKey{
									Format: "nonsense",
									Data:   []byte{0x13, 0x37},
								},
								Signature: []byte{0xca, 0xfe},
							},
						},
					},
				},
			},
		})
		if err != nil {
			t.Error("PutRealizations:", err)
		}

		gotData, err := os.ReadFile(filepath.Join(dir, "realizations", "0qbvr7pibx9hyiyafqbzhjbvpaifcjb6d5qpwirac0kyhdxjw5xx.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got zbstore.RealizationMap
		unmarshalers := jsonv2.UnmarshalFromFunc(zbstore.UnmarshalHashJSONFrom)
		if err := jsonv2.Unmarshal(gotData, &got, jsonv2.WithUnmarshalers(unmarshalers)); err != nil {
			t.Fatal(err)
		}
		want := zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					{
						OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
						Signatures: []*zbstore.RealizationSignature{
							{
								PublicKey: zbstore.RealizationPublicKey{
									Format: "nonsense",
									Data:   []byte{0xde, 0xad, 0xbe, 0xef},
								},
								Signature: []byte{0xca, 0xfe},
							},
							{
								PublicKey: zbstore.RealizationPublicKey{
									Format: "nonsense",
									Data:   []byte{0x13, 0x37},
								},
								Signature: []byte{0xca, 0xfe},
							},
						},
					},
				},
			},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("realizations (-want +got):\n%s", diff)
		}
	})

	t.Run("ClientError", func(t *testing.T) {
		tests := []struct {
			code               int
			wantPermanentError bool
		}{
			{http.StatusUnauthorized, true},
			{http.StatusForbidden, true},
			{http.StatusPreconditionFailed, false},
			{http.StatusTooManyRequests, false},
		}

		for _, test := range tests {
			for _, failGet := range [...]bool{false, true} {
				name := strings.ReplaceAll(http.StatusText(test.code), " ", "")
				failGetLabel := "Put"
				if failGet {
					failGetLabel = "Get"
				}
				t.Run(name+"/"+failGetLabel, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx := testlog.WithTB(t.Context(), t)

						mux := http.NewServeMux()
						mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
							if !failGet && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
								http.NotFound(w, r)
							} else {
								http.Error(w, http.StatusText(test.code), test.code)
							}
						})
						mux.HandleFunc("/discovery.json", func(w http.ResponseWriter, r *http.Request) {
							http.ServeFile(w, r, testdataPath(t, "../../discovery.json"))
						})
						srv := httptest.NewServer(mux)
						t.Cleanup(srv.Close)
						discoveryURL, err := url.Parse(srv.URL + "/discovery.json")
						if err != nil {
							t.Fatal(err)
						}

						store := &Store{
							URL:        discoveryURL,
							HTTPClient: srv.Client(),
						}

						drvHash := mustParseHash(t, "sha256:bd172e7b837e02a672e417976696642eaabb97847f61a77cf430f515efc97b61")
						err = store.PutRealizations(ctx, zbstore.RealizationMap{
							DerivationHash: drvHash,
							Realizations: map[string][]*zbstore.Realization{
								zbstore.DefaultDerivationOutputName: {
									{
										OutputPath: "/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt",
									},
								},
							},
						})
						if err == nil {
							t.Error("store.PutRealizations did not return an error")
						} else {
							t.Log("store.PutRealizations:", err)
						}

						if got := IsPermanentError(err); got && !test.wantPermanentError {
							t.Error("Error is permanent")
						} else if !got && test.wantPermanentError {
							t.Error("Error is not permanent")
						}
					})
				})
			}
		}
	})
}

func testdataPath(tb testing.TB, path string) string {
	tb.Helper()
	return filepath.Join("testdata", filepath.FromSlash(tb.Name()), filepath.FromSlash(path))
}

func copyToDir(tb testing.TB, dstDir string, path string) {
	tb.Helper()
	srcPath := testdataPath(tb, path)
	dstPath := filepath.Join(dstDir, filepath.ToSlash(path))
	if err := copyFile(dstPath, srcPath); err != nil {
		tb.Fatal(err)
	}
}

func copyFile(dstPath, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o777); err != nil {
		return err
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return &os.LinkError{
			Op:  "copy",
			Old: srcPath,
			New: dstPath,
			Err: err,
		}
	}
	if err := dst.Close(); err != nil {
		return &os.PathError{
			Op:   "write",
			Path: dstPath,
			Err:  err,
		}
	}
	return nil
}

func httpStoreForDirectory(tb testing.TB, path string) *Store {
	tb.Helper()

	root, err := os.OpenRoot(testdataPath(tb, path))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := root.Close(); err != nil {
			tb.Error(err)
		}
	})

	srv := httptest.NewServer(http.FileServerFS(root.FS()))
	tb.Cleanup(srv.Close)
	srvURL, err := url.Parse(srv.URL + "/discovery.json")
	if err != nil {
		tb.Fatal(err)
	}

	return &Store{
		URL:        srvURL,
		HTTPClient: srv.Client(),
	}
}

func TestMain(m *testing.M) {
	testlog.Main(nil)
	os.Exit(m.Run())
}
