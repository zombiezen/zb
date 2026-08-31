// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/hal"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
)

func TestRealizeSingleDerivation(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeReuse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeDisableReuse(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeMultiStep(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeReferenceToDep(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeInputReference(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	uploadDir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	discoveryDocument := hal.Resource{
		Links: map[string]hal.ArrayOrObject[*hal.Link]{
			"https://zb-build.dev/api/rel/realization": hal.Array([]*hal.Link{
				{HRef: "realizations/{hashDigest}.json", Templated: true},
			}),
		},
	}
	discoveryJSON, err := jsonv2.Marshal(discoveryDocument)
	if err != nil {
		t.Fatal(err)
	}
	discoveryPath := filepath.Join(uploadDir, "discovery.json")
	if err := os.WriteFile(discoveryPath, discoveryJSON, 0o666); err != nil {
		t.Fatal(err)
	}

	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Writer: &zbstorehttp.Store{
				URL: fileurl.FromPath(discoveryPath),
				HTTPClient: &http.Client{
					Transport: fileurl.Transport{},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	vars := runScriptTest(ctx, t, dir, server, data, nil)
	inputPath, _, err := dir.ParsePath(vars["in"])
	if err != nil {
		t.Fatal("in:", err)
	}
	drvPath, _, err := dir.ParsePath(vars["drvPath"])
	if err != nil {
		t.Fatal("drvPath:", err)
	}
	wantOutputPath, _, err := dir.ParsePath(vars["out"])
	if err != nil {
		t.Fatal("out", err)
	}

	// Wait until server has finished uploading.
	if err := server.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify that the realizations document that is uploaded is valid.
	drvData, err := os.ReadFile(string(drvPath))
	if err != nil {
		t.Fatal(err)
	}
	drvName, _ := drvPath.DerivationName()
	if drv, err := zbstore.ParseDerivation(dir, drvName, drvData); err != nil {
		t.Error(err)
	} else {
		drvHash, err := drv.SHA256RealizationHash(nil)
		if err != nil {
			t.Fatal(err)
		}
		realizationsFilename := drvHash.RawBase32() + ".json"
		realizationsPath := filepath.Join(uploadDir, "realizations", realizationsFilename)
		if realizationsJSON, err := os.ReadFile(realizationsPath); err != nil {
			t.Error(err)
		} else {
			var got zbstore.RealizationMap
			want := zbstore.RealizationMap{
				DerivationHash: drvHash,
				Realizations: map[string][]*zbstore.Realization{
					zbstore.DefaultDerivationOutputName: {{
						OutputPath: wantOutputPath,
						ReferenceClasses: []*zbstore.ReferenceClass{
							{Path: inputPath},
						},
					}},
				},
			}
			err := jsonv2.Unmarshal(
				realizationsJSON, &got,
				jsonv2.RejectUnknownMembers(true),
				jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(zbstore.UnmarshalHashJSONFrom)),
			)
			if err != nil {
				t.Error(err)
			} else if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s (-want +got):\n%s", realizationsFilename, diff)
			}
		}
	}
}

func TestRealizeSelfReference(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeFixed(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeFailure(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeNoOutput(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeCores(t *testing.T) {
	t.Parallel()

	tests := []int{1, 2}
	for _, n := range tests {
		t.Run(fmt.Sprintf("N%d", n), func(t *testing.T) {
			ctx := testcontext.New(t)
			dir := backendtest.NewStoreDirectory(t)
			server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
				TempDir: t.TempDir(),
				Options: Options{
					CoresPerBuild: n,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := readTestData(dir, "TestRealizeCores.txt", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := data.writeTo(ctx, server, nil); err != nil {
				t.Fatal(err)
			}
			runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
				initialEnv: map[string]string{
					"cores": strconv.Itoa(n),
				},
			})
		})
	}
}

func TestRealizeFetchURL(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)

	const fileContent = "Hello, World!\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/hello.txt", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "hello.txt", time.Time{}, strings.NewReader(fileContent))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := backendtest.NewStoreDirectory(t)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), map[string]string{
		"@url@": srv.URL + "/hello.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeSignature(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	testKey := ed25519.PrivateKey{
		0xf8, 0xd3, 0x03, 0x35, 0xfb, 0xe3, 0x0a, 0x67,
		0x53, 0xf6, 0x62, 0xeb, 0xf7, 0x36, 0x9d, 0x61,
		0x05, 0xf0, 0x17, 0xf9, 0x8f, 0x2e, 0xc4, 0xe8,
		0x33, 0x0d, 0xfa, 0xc9, 0x7e, 0xf0, 0xe8, 0x70,
		0x95, 0x09, 0x22, 0xbd, 0x27, 0x65, 0xac, 0x30,
		0x63, 0xc2, 0x01, 0x3f, 0x54, 0xd9, 0x8f, 0x79,
		0xf4, 0xd1, 0x60, 0x01, 0xf7, 0x62, 0x49, 0x61,
		0x91, 0xbd, 0x66, 0xd7, 0x62, 0x51, 0x94, 0x70,
	}
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Keyring: &Keyring{
				Ed25519: []ed25519.PrivateKey{testKey},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	vars := runScriptTest(ctx, t, dir, server, data, nil)

	got := new(zbstorerpc.Build)
	if err := jsonv2.Unmarshal([]byte(vars["build"]), got); err != nil {
		t.Fatal(err)
	}
	drvPath, err := zbstore.ParsePath(vars["drvPath"])
	if err != nil {
		t.Fatal(err)
	}
	drvHash, _, err := hashDerivationFromFetcher(ctx, data.objects, zbstore.Null{}, drvPath)
	if err != nil {
		t.Fatal(err)
	}

	gotResult, err := got.ResultForPath(drvPath)
	if err != nil {
		t.Error(err)
	}
	if gotResult == nil {
		return
	}
	output, err := gotResult.OutputForName(zbstore.DefaultDerivationOutputName)
	if err != nil {
		t.Fatal(err)
	}
	if !output.Path.Valid {
		t.Errorf("no output path for %v", zbstore.OutputReference{
			DrvPath:    drvPath,
			OutputName: zbstore.DefaultDerivationOutputName,
		})
	}

	outputRef := zbstore.RealizationOutputReference{
		DerivationHash: drvHash,
		OutputName:     "out",
	}
	realization := &zbstore.Realization{
		OutputPath: output.Path.X,
	}
	sig, err := zbstore.SignRealizationWithEd25519(outputRef, realization, testKey)
	if err != nil {
		t.Fatal(err)
	}
	want := &zbstorerpc.BuildResult{
		DrvPath: drvPath,
		DrvHash: zbstorerpc.NonNull(drvHash),
		Status:  zbstorerpc.BuildSuccess,
		Outputs: []*zbstorerpc.RealizeOutput{
			{
				Name:       zbstore.DefaultDerivationOutputName,
				Path:       output.Path,
				Signatures: []*zbstore.RealizationSignature{sig},
			},
		},
	}
	diff := cmp.Diff(
		want, gotResult,
		buildResultOption,
	)
	if diff != "" {
		t.Errorf("realize response (-want +got):\n%s", diff)
	}
}

func TestRealizeSingleDerivationFallback(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	fallbackStore := new(storetest.Store)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, fallbackStore); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
		fallback: fallbackStore,
	})
}

func TestRealizeWithImproperlyNamedFallback(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	fallbackStore := new(storetest.Store)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, fallbackStore); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
		fallback: fallbackStore,
	})
}

// TestRealizeMultiStepFallback tests a build of drv2 depending on drv1,
// with a fallback store that has a full realization chain
// and only includes the output store object for drv2.
func TestRealizeMultiStepFallback(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	fallbackStore := new(storetest.Store)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, fallbackStore); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
		fallback: fallbackStore,
	})
}

// TestRealizeMultiStepFallbackIntermediate tests a build of drv2 depending on drv1,
// with a fallback store that only has a realization for drv1.
func TestRealizeMultiStepFallbackIntermediate(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	fallbackStore := new(storetest.Store)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, fallbackStore); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
		fallback: fallbackStore,
	})
}

// TestRealizeMultiStepFallbackMissingObject tests a build of drv2 depending on drv1,
// with a fallback store that has a full realization chain
// and only includes the output store object for drv1.
// This checks whether the build can recover from having realizations that can't actually be used.
func TestRealizeMultiStepFallbackMissingObject(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	fallbackStore := new(storetest.Store)
	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, fallbackStore); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
		fallback: fallbackStore,
	})
}

func TestRealizeIssue288(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTestData(dir, t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	runScriptTest(ctx, t, dir, server, data, nil)
}

func TestRealizeUpload(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)
	testKey := ed25519.PrivateKey{
		0xf8, 0xd3, 0x03, 0x35, 0xfb, 0xe3, 0x0a, 0x67,
		0x53, 0xf6, 0x62, 0xeb, 0xf7, 0x36, 0x9d, 0x61,
		0x05, 0xf0, 0x17, 0xf9, 0x8f, 0x2e, 0xc4, 0xe8,
		0x33, 0x0d, 0xfa, 0xc9, 0x7e, 0xf0, 0xe8, 0x70,
		0x95, 0x09, 0x22, 0xbd, 0x27, 0x65, 0xac, 0x30,
		0x63, 0xc2, 0x01, 0x3f, 0x54, 0xd9, 0x8f, 0x79,
		0xf4, 0xd1, 0x60, 0x01, 0xf7, 0x62, 0x49, 0x61,
		0x91, 0xbd, 0x66, 0xd7, 0x62, 0x51, 0x94, 0x70,
	}

	uploadStoreDir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	discoveryData, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(t.Name()), "discovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	discoveryPath := filepath.Join(uploadStoreDir, "discovery.json")
	err = os.WriteFile(discoveryPath, discoveryData, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	uploadStore := &zbstorehttp.Store{
		URL: fileurl.FromPath(discoveryPath),
		HTTPClient: &http.Client{
			Transport: fileurl.Transport{},
		},
	}

	server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Writer: uploadStore,
			Keyring: &Keyring{
				Ed25519: []ed25519.PrivateKey{testKey},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run script and capture objects from test file.
	data, err := readTestData(dir, t.Name()+"/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.writeTo(ctx, server, nil); err != nil {
		t.Fatal(err)
	}
	vars := runScriptTest(ctx, t, dir, server, data, nil)

	// Wait for uploads to finish.
	if err := server.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	// Object existence is sufficient: [zbstorehttp.Store] already verifies.
	wantOutputPath, err := zbstore.ParsePath(vars["out"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uploadStore.Object(ctx, wantOutputPath); err != nil {
		t.Error(err)
	}

	// Verify realizations.
	drvPath, err := zbstore.ParsePath(vars["drvPath"])
	if err != nil {
		t.Fatal(err)
	}
	drvHash, _, err := hashDerivationFromFetcher(ctx, data.objects, uploadStore, drvPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := uploadStore.FetchRealizations(ctx, drvHash); err != nil {
		t.Error(err)
	} else {
		outputRef := zbstore.RealizationOutputReference{
			DerivationHash: drvHash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		}
		wantRealization := &zbstore.Realization{
			OutputPath: wantOutputPath,
		}
		sig, err := zbstore.SignRealizationWithEd25519(outputRef, wantRealization, testKey)
		if err != nil {
			t.Fatal(err)
		}
		wantRealization.Signatures = append(wantRealization.Signatures, sig)
		want := zbstore.RealizationMap{
			DerivationHash: drvHash,
			Realizations: map[string][]*zbstore.Realization{
				zbstore.DefaultDerivationOutputName: {
					wantRealization,
				},
			},
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("realizations for %v (-want +got):\n%s", drvHash, diff)
		}
	}
}

var buildResultOption = cmp.Options{
	cmp.FilterPath(func(p cmp.Path) bool {
		return isFieldAnyOf[zbstorerpc.BuildResult](p, "LogSize")
	}, cmp.Ignore()),
	cmp.FilterPath(isRealizeOutputSignaturesField, cmpopts.EquateEmpty()),
}

func isRealizeOutputSignaturesField(p cmp.Path) bool {
	return isFieldAnyOf[zbstorerpc.RealizeOutput](p, "Signatures")
}

func isFieldAnyOf[T any](p cmp.Path, names ...string) bool {
	if p.Index(-2).Type() != reflect.TypeFor[T]() {
		return false
	}
	name := p.Last().(cmp.StructField).Name()
	return slices.Contains(names, name)
}
