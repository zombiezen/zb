// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/tools/txtar"
	. "zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/hal"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log/testlog"
	"zombiezen.com/go/nix"
)

const (
	shPath         = "/bin/sh"
	powershellPath = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
)

func TestRealizeSingleDerivation(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportArchive, err := txtar.ParseFile(filepath.Join("testdata", "TestRealizeSingleDerivation.txt"))
	if err != nil {
		t.Fatal(err)
	}
	objects, _, err := storetest.TxtarObjects(dir, exportArchive.Files)
	if err != nil {
		t.Fatal(err)
	}
	drvObject, err := findObjectWithName(derivationNameForCurrentSystem("hello2.txt"), slices.Values(objects))
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvObject.StorePath},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}
	if realizeResponse.BuildID == "" {
		t.Fatal("no build ID returned")
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLog, err := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvObject.StorePath); err != nil {
		t.Error(err)
	} else {
		if want := "catcat\n"; string(gotLog) != want {
			t.Errorf("build log:\n%s\n(want %q)", gotLog, want)
		}
	}

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	outputName, _ := drvObject.StorePath.DerivationName()
	wantOutputPath, err := singleFileOutputPath(dir, outputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvObject.StorePath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeReuse(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello2.txt"
	drvContent := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvContent.Builder, drvContent.Args = catcatBuilder()
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realize1Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize1Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("first RPC error:", err)
	}
	if _, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize1Response.BuildID); err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realize1Response.BuildID, drvPath)
		t.Fatalf("first build failed: %v\nlog:\n%s", err, gotLog)
	}

	realize2Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize2Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("second RPC error:", err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize2Response.BuildID)
	if err != nil {
		t.Error("second build failed:", err)
	}

	gotLog, err := backendtest.ReadLog(ctx, client, realize2Response.BuildID, drvPath)
	if err != nil {
		t.Error("accessing second build's logs:", err)
	}
	if want := ""; string(gotLog) != want {
		t.Errorf("build log:\n%s\n(want %q)", gotLog, want)
	}

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeDisableReuse(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello2.txt"
	drvContent := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvContent.Builder, drvContent.Args = catcatBuilder()
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realize1Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize1Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("first RPC error:", err)
	}
	if _, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize1Response.BuildID); err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realize1Response.BuildID, drvPath)
		t.Fatalf("first build failed: %v\nlog:\n%s", err, gotLog)
	}

	realize2Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize2Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
		Reuse:    nil,
	})
	if err != nil {
		t.Fatal("second RPC error:", err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize2Response.BuildID)
	if err != nil {
		t.Error("second build failed:", err)
	}

	gotLog, err := backendtest.ReadLog(ctx, client, realize2Response.BuildID, drvPath)
	if err != nil {
		t.Error("accessing second build's logs:", err)
	}
	if want := "catcat\n"; string(gotLog) != want {
		t.Errorf("build log:\n%s\n(want %q)", gotLog, want)
	}

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeMultiStep(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drv1Content := &zbstore.Derivation{
		Name:   "hello2.txt",
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv1Content.Builder, drv1Content.Args = catcatBuilder()
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello4.txt"
	drv2Content := &zbstore.Derivation{
		Name:   "hello4.txt",
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in": zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv1Path,
				OutputName: zbstore.DefaultDerivationOutputName,
			}),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
			drv1Path: sets.NewSorted(zbstore.DefaultDerivationOutputName),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv2Content.Builder, drv2Content.Args = catcatBuilder()
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog1, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv1Path)
		gotLog2, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv2Path)
		t.Fatalf("build failed: %v\ndrv1 log:\n%s\ndrv2 log:\n%s", err, gotLog1, gotLog2)
	}

	wantOutputContent := strings.Repeat(inputContent, 4)
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drv2Path, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeReferenceToDep(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	const drv1OutputName = "hello2.txt"
	drv1Content := &zbstore.Derivation{
		Name:   drv1OutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv1Content.Builder, drv1Content.Args = catcatBuilder()
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello-ref.txt"
	drv2Content := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in": zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv1Path,
				OutputName: zbstore.DefaultDerivationOutputName,
			}),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
			drv1Path: sets.NewSorted(zbstore.DefaultDerivationOutputName),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	if runtime.GOOS == "windows" {
		drv2Content.Builder = powershellPath
		drv2Content.Args = []string{
			"-Command",
			"(${env:in} + \"`n\")" + ` | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}`,
		}
	} else {
		drv2Content.Builder = shPath
		drv2Content.Args = []string{
			"-c",
			`echo "$in" > "$out"`,
		}
	}
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog1, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv1Path)
		gotLog2, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv2Path)
		t.Fatalf("build failed: %v\ndrv1 log:\n%s\ndrv2 log:\n%s", err, gotLog1, gotLog2)
	}

	drv1OutputContent := strings.Repeat(inputContent, 2)
	drv1OutputPath, err := singleFileOutputPath(dir, drv1OutputName, []byte(drv1OutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}

	wantOutputContent := append([]byte(drv1OutputPath), '\n')
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, wantOutputContent, zbstore.References{
		Others: *sets.NewSorted(
			drv1OutputPath,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drv2Path, wantOutputPath, wantOutputContent, got)

	var info zbstorerpc.InfoResponse
	err = jsonrpc.Do(ctx, client, zbstorerpc.InfoMethod, &info, &zbstorerpc.InfoRequest{
		Path: wantOutputPath,
	})
	if err != nil {
		t.Error(err)
	} else {
		buf := new(bytes.Buffer)
		if err := storetest.SingleFileNAR(buf, wantOutputContent); err != nil {
			t.Fatal(err)
		}
		narData := buf.Bytes()
		ca, _, err := zbstore.SourceSHA256ContentAddress(bytes.NewReader(narData), nil)
		if err != nil {
			t.Fatal(err)
		}

		want := wantObjectInfo(info.Info, &zbstore.Blob{
			NAR: narData,
			ExportTrailer: zbstore.ExportTrailer{
				StorePath:      wantOutputPath,
				ContentAddress: ca,
				References:     *sets.NewSorted(drv1OutputPath),
			},
		})
		if diff := cmp.Diff(want, info.Info); diff != "" {
			t.Errorf("info (-want +got):\n%s", diff)
		}
	}
}

func TestRealizeInputReference(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportArchive, err := txtar.ParseFile(filepath.Join("testdata", "TestRealizeInputReference.txt"))
	if err != nil {
		t.Fatal(err)
	}
	objects, _, err := storetest.TxtarObjects(dir, exportArchive.Files)
	if err != nil {
		t.Fatal(err)
	}
	inputObject, err := findObjectWithName("hello.txt", slices.Values(objects))
	if err != nil {
		t.Fatal(err)
	}
	drvObject, err := findObjectWithName(derivationNameForCurrentSystem("hello-path.txt"), slices.Values(objects))
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

	server, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Upload: &zbstorehttp.Store{
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
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, exportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvObject.StorePath},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}
	if realizeResponse.BuildID == "" {
		t.Fatal("no build ID returned")
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Fatal(err)
	}

	wantOutputContent := string(inputObject.StorePath) + "\n"
	wantOutputName, _ := drvObject.StorePath.DerivationName()
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{
		Others: *sets.NewSorted(inputObject.StorePath),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvObject.StorePath, wantOutputPath, []byte(wantOutputContent), got)

	// Wait until server has finished uploading.
	if err := server.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify that the realizations document that is uploaded is valid.
	if drv, err := zbstore.ParseDerivationObject(ctx, drvObject); err != nil {
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
							{Path: inputObject.StorePath},
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
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	const wantOutputName = "self.txt"
	drvContent := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	if runtime.GOOS == "windows" {
		drvContent.Builder = powershellPath
		drvContent.Args = []string{"-Command", "\"${env:out}`n\" | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}"}
	} else {
		drvContent.Builder = shPath
		drvContent.Args = []string{"-c", `echo "$out" > "$out"`}
	}
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}
	if realizeResponse.BuildID == "" {
		t.Fatal("no build ID returned")
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Fatal(err)
	}

	const fakeDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contentModuloHash := new(bytes.Buffer)
	if err := storetest.SingleFileNAR(contentModuloHash, []byte(dir.Join(fakeDigest+"-"+wantOutputName)+"\n")); err != nil {
		t.Fatal(err)
	}
	ca, _, err := zbstore.SourceSHA256ContentAddress(contentModuloHash, &zbstore.ContentAddressOptions{
		Digest: fakeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantOutputPath, err := zbstore.FixedCAOutputPath(dir, wantOutputName, ca, zbstore.References{Self: true})
	if err != nil {
		t.Fatal(err)
	}
	wantOutputContent := dir.Join(wantOutputPath.Digest()+"-"+wantOutputName) + "\n"

	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeFixed(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	const wantOutputName = "hello.txt"
	const wantOutputContent = "Hello, World!\n"
	wantOutputCA := nix.FlatFileContentAddress(mustParseHash(t, "sha256:c98c24b677eff44860afea6f493bbaec5bb1c4cbb209c6fc2bbb47f66ff2ad31"))
	drv1Content := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(wantOutputCA),
		},
	}
	if runtime.GOOS == "windows" {
		drv1Content.Builder = powershellPath
		drv1Content.Args = []string{
			"-Command",
			"\"Hello, World!`n\" | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}",
		}
	} else {
		drv1Content.Builder = shPath
		drv1Content.Args = []string{
			"-c",
			`echo 'Hello, World!' > $out`,
		}
	}
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	// Create a second derivation with the same output hash
	// but a totally failing builder.
	drv2Content := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(wantOutputCA),
		},
	}
	if runtime.GOOS == "windows" {
		drv2Content.Builder = powershellPath
		drv2Content.Args = []string{"-Command", "exit 1"}
	} else {
		drv2Content.Builder = shPath
		drv2Content.Args = []string{"-c", "exit 1"}
	}
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	wantOutputPath, err := zbstore.FixedCAOutputPath(dir, wantOutputName, wantOutputCA, zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realize1Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize1Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv1Path},
	})
	if err != nil {
		t.Fatal("build drv1:", err)
	}
	got1, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize1Response.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realize1Response.BuildID, drv1Path)
		t.Fatalf("build drv1: %v\nlog:\n%s", err, gotLog)
	}
	checkSingleFileOutput(t, drv1Path, wantOutputPath, []byte(wantOutputContent), got1)

	// Now let's build the second derivation to see whether the output gets reused.
	realize2Response := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realize2Response, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
	})
	if err != nil {
		t.Fatal("build drv2:", err)
	}
	got2, err := backendtest.WaitForSuccessfulBuild(ctx, client, realize2Response.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realize2Response.BuildID, drv2Path)
		t.Fatalf("build drv2: %v\nlog:\n%s", err, gotLog)
	}
	checkSingleFileOutput(t, drv2Path, wantOutputPath, []byte(wantOutputContent), got2)
}

func TestRealizeFailure(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	const drvName = "hello.txt"
	// Create a derivation that fails after creating its output.
	drvContent := &zbstore.Derivation{
		Name:   drvName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	if runtime.GOOS == "windows" {
		drvContent.Builder = powershellPath
		drvContent.Args = []string{"-Command", "New-Item ${env:out} -type file ; exit 1"}
	} else {
		drvContent.Builder = shPath
		drvContent.Args = []string{"-c", "echo > $out ; exit 1"}
	}
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("build drv:", err)
	}
	got, err := backendtest.WaitForBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Fatal("build drv:", err)
	}
	want := &zbstorerpc.Build{
		ID:     realizeResponse.BuildID,
		Status: zbstorerpc.BuildFail,
		Results: []*zbstorerpc.BuildResult{
			{
				DrvPath: drvPath,
				Status:  zbstorerpc.BuildFail,
				Outputs: []*zbstorerpc.RealizeOutput{
					{
						Name: zbstore.DefaultDerivationOutputName,
					},
				},
			},
		},
	}
	diff := cmp.Diff(
		want, got,
		cmp.FilterPath(
			func(p cmp.Path) bool {
				return isFieldAnyOf[zbstorerpc.Build](p, "StartedAt", "EndedAt")
			},
			cmp.Ignore(),
		),
		buildResultOption,
		ignoreDrvHashOption,
	)
	if diff != "" {
		t.Errorf("build (-want +got):\n%s", diff)
	}
	if !got.EndedAt.Valid {
		t.Error("build.endedAt = null")
	}
	// Ensure that the build didn't leave files in the store.
	storeListing, err := os.ReadDir(string(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range storeListing {
		name := ent.Name()
		if name != drvPath.Base() {
			t.Errorf("unknown object %s left in store", name)
		}
	}

	if gotLog, err := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath); err != nil {
		t.Error(err)
	} else if want := "exit status"; !bytes.Contains(gotLog, []byte(want)) {
		t.Errorf("Log does not contain phrase %q. Full output:\n%s", want, gotLog)
	}
}

func TestRealizeNoOutput(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	const drvName = "hello.txt"
	// Create a derivation that no-ops.
	drvContent := &zbstore.Derivation{
		Name:   drvName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	if runtime.GOOS == "windows" {
		drvContent.Builder = powershellPath
		drvContent.Args = []string{"-Command", "exit"}
	} else {
		drvContent.Builder = shPath
		drvContent.Args = []string{"-c", "true"}
	}
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("build drv:", err)
	}
	got, err := backendtest.WaitForBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Fatal("build drv:", err)
	}
	want := &zbstorerpc.Build{
		ID:     realizeResponse.BuildID,
		Status: zbstorerpc.BuildFail,
		Results: []*zbstorerpc.BuildResult{
			{
				DrvPath: drvPath,
				Status:  zbstorerpc.BuildFail,
				Outputs: []*zbstorerpc.RealizeOutput{
					{
						Name: zbstore.DefaultDerivationOutputName,
					},
				},
			},
		},
	}
	diff := cmp.Diff(
		want, got,
		cmp.FilterPath(
			func(p cmp.Path) bool {
				return isFieldAnyOf[zbstorerpc.Build](p, "StartedAt", "EndedAt")
			},
			cmp.Ignore(),
		),
		buildResultOption,
		ignoreDrvHashOption,
	)
	if diff != "" {
		t.Errorf("build (-want +got):\n%s", diff)
	}
	if !got.EndedAt.Valid {
		t.Error("build.endedAt = null")
	}
	// Ensure that the build didn't leave files in the store.
	storeListing, err := os.ReadDir(string(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range storeListing {
		name := ent.Name()
		if name != drvPath.Base() {
			t.Errorf("unknown object %s left in store", name)
		}
	}

	if gotLog, err := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath); err != nil {
		t.Error(err)
	} else if want := "failed to produce output $out"; !bytes.Contains(gotLog, []byte(want)) {
		t.Errorf("Log does not contain phrase %q. Full output:\n%s", want, gotLog)
	}
}

func TestRealizeCores(t *testing.T) {
	tests := []int{1, 2}
	for _, n := range tests {
		t.Run(fmt.Sprintf("N%d", n), func(t *testing.T) {
			ctx := testcontext.New(t)
			dir := backendtest.NewStoreDirectory(t)

			exportBuffer := new(bytes.Buffer)
			exporter := zbstore.NewExportWriter(exportBuffer)
			const drvName = "cores.txt"
			// Create a derivation that fails after creating its output.
			drvContent := &zbstore.Derivation{
				Name:   drvName,
				Dir:    dir,
				System: system.Current().String(),
				Env: map[string]string{
					"out": zbstore.HashPlaceholder("out"),
				},
				Outputs: map[string]*zbstore.DerivationOutputType{
					zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
				},
			}
			if runtime.GOOS == "windows" {
				drvContent.Builder = powershellPath
				drvContent.Args = []string{"-Command", "\"${env:ZB_BUILD_CORES}`n\" | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}"}
			} else {
				drvContent.Builder = shPath
				drvContent.Args = []string{"-c", `echo "$ZB_BUILD_CORES" > "$out"`}
			}
			drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := exporter.Close(); err != nil {
				t.Fatal(err)
			}

			_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
				TempDir: t.TempDir(),
				Options: Options{
					CoresPerBuild: n,
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

			realizeResponse := new(zbstorerpc.RealizeResponse)
			err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
				DrvPaths: []zbstore.Path{drvPath},
			})
			if err != nil {
				t.Fatal("build drv:", err)
			}
			got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
			if err != nil {
				gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
				t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
			}
			wantOutputContent := fmt.Sprintf("%d\n", n)
			wantOutputPath, err := singleFileOutputPath(dir, drvName, []byte(wantOutputContent), zbstore.References{})
			if err != nil {
				t.Fatal(err)
			}
			checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
		})
	}
}

func TestRealizeFetchURL(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const fileContent = "Hello, World!\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/hello.txt", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "hello.txt", time.Time{}, strings.NewReader(fileContent))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	const wantOutputName = "hello.txt"
	wantOutputCA := nix.FlatFileContentAddress(mustParseHash(t, "sha256:c98c24b677eff44860afea6f493bbaec5bb1c4cbb209c6fc2bbb47f66ff2ad31"))
	drvContent := &zbstore.Derivation{
		Name:    wantOutputName,
		Dir:     dir,
		Builder: "builtin:fetchurl",
		System:  "builtin",
		Env: map[string]string{
			"url": string(srv.URL + "/hello.txt"),
			"out": zbstore.HashPlaceholder("out"),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(wantOutputCA),
		},
	}
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("build drv:", err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
		t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
	}

	wantOutputPath, err := zbstore.FixedCAOutputPath(dir, wantOutputName, wantOutputCA, zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(fileContent), got)
}

func TestRealizeSignature(t *testing.T) {
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

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello2.txt"
	drvContent := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvContent.Builder, drvContent.Args = catcatBuilder()
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	drvHash, err := drvContent.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
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
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, exportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}
	if realizeResponse.BuildID == "" {
		t.Fatal("no build ID returned")
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
		t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
	}

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{})
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

	outputRef := zbstore.RealizationOutputReference{
		DerivationHash: drvHash,
		OutputName:     "out",
	}
	realization := &zbstore.Realization{
		OutputPath: wantOutputPath,
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
				Path:       zbstorerpc.NonNull(wantOutputPath),
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
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	localExportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(localExportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drvContent := &zbstore.Derivation{
		Name:    "hello2.txt",
		Dir:     dir,
		Builder: "false", // Ensure can't run as-is.
		System:  system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder(zbstore.DefaultDerivationOutputName),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	drvHash, err := drvContent.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}

	fallbackExportBuffer := new(bytes.Buffer)
	exporter = zbstore.NewExportWriter(fallbackExportBuffer)
	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, _, err := storetest.ExportSourceFile(exporter, []byte(wantOutputContent), storetest.SourceExportOptions{
		Name:      drvContent.Name,
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	fallbackStore := new(storetest.Store)
	if err := fallbackStore.StoreImport(ctx, fallbackExportBuffer); err != nil {
		t.Fatal(err)
	}
	ref := zbstore.RealizationOutputReference{
		DerivationHash: drvHash,
		OutputName:     zbstore.DefaultDerivationOutputName,
	}
	fallbackStore.AddRealization(ref, &zbstore.Realization{
		OutputPath: wantOutputPath,
	})

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, localExportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
		t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
	}

	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeWithImproperlyNamedFallback(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	localExportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(localExportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drvContent := &zbstore.Derivation{
		Name:   "hello2.txt",
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder(zbstore.DefaultDerivationOutputName),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvContent.Builder, drvContent.Args = catcatBuilder()
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	drvHash, err := drvContent.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}

	fallbackExportBuffer := new(bytes.Buffer)
	exporter = zbstore.NewExportWriter(fallbackExportBuffer)
	incorrectOutputPath, _, err := storetest.ExportSourceFile(exporter, []byte("INCORRECT\n"), storetest.SourceExportOptions{
		Name:      "bork.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	fallbackStore := new(storetest.Store)
	if err := fallbackStore.StoreImport(ctx, fallbackExportBuffer); err != nil {
		t.Fatal(err)
	}
	ref := zbstore.RealizationOutputReference{
		DerivationHash: drvHash,
		OutputName:     zbstore.DefaultDerivationOutputName,
	}
	fallbackStore.AddRealization(ref, &zbstore.Realization{
		OutputPath: incorrectOutputPath,
	})

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, localExportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drvPath},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
		t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
	}

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, err := singleFileOutputPath(dir, drvContent.Name, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drvPath, wantOutputPath, []byte(wantOutputContent), got)
}

// TestRealizeMultiStepFallback tests a build of drv2 depending on drv1,
// with a fallback store that has a full realization chain
// and only includes the output store object for drv2.
func TestRealizeMultiStepFallback(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	localExportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(localExportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drv1Content := &zbstore.Derivation{
		Name:    "hello2.txt",
		Dir:     dir,
		Builder: "false", // Prevent from running.
		System:  system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	drv2Content := &zbstore.Derivation{
		Name:    "hello4.txt",
		Dir:     dir,
		Builder: "false", // Prevent from running.
		System:  system.Current().String(),
		Env: map[string]string{
			"in": zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv1Path,
				OutputName: zbstore.DefaultDerivationOutputName,
			}),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
			drv1Path: sets.NewSorted(zbstore.DefaultDerivationOutputName),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	fallbackExportBuffer := new(bytes.Buffer)
	exporter = zbstore.NewExportWriter(fallbackExportBuffer)
	const wantOutputContent1 = inputContent + inputContent
	// Shouldn't need the intermediate store object.
	wantOutputPath1, err := singleFileOutputPath(dir, drv1Content.Name, []byte(wantOutputContent1), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputContent2 = wantOutputContent1 + wantOutputContent1
	wantOutputPath2, _, err := storetest.ExportSourceFile(exporter, []byte(wantOutputContent2), storetest.SourceExportOptions{
		Name:      drv2Content.Name,
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	fallbackStore := new(storetest.Store)
	if err := fallbackStore.StoreImport(ctx, fallbackExportBuffer); err != nil {
		t.Fatal(err)
	}
	drv1Hash, err := drv1Content.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	drv2Hash, err := drv2Content.SHA256RealizationHash(func(ref zbstore.OutputReference) (zbstore.Path, error) {
		switch {
		case ref.DrvPath == drv1Path && ref.OutputName == zbstore.DefaultDerivationOutputName:
			return wantOutputPath1, nil
		}
		return "", fmt.Errorf("missing realization for %v", ref)
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackStore.AddRealization(
		zbstore.RealizationOutputReference{
			DerivationHash: drv1Hash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		},
		&zbstore.Realization{OutputPath: wantOutputPath1},
	)
	fallbackStore.AddRealization(
		zbstore.RealizationOutputReference{
			DerivationHash: drv2Hash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		},
		&zbstore.Realization{OutputPath: wantOutputPath2},
	)

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, localExportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog1, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv1Path)
		gotLog2, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv2Path)
		t.Fatalf("build drv: %v\nlog 1:\n%s\nlog 2:\n%s", err, gotLog1, gotLog2)
	}

	checkSingleFileOutput(t, drv2Path, wantOutputPath2, []byte(wantOutputContent2), got)
}

// TestRealizeMultiStepFallbackIntermediate tests a build of drv2 depending on drv1,
// with a fallback store that only has a realization for drv1.
func TestRealizeMultiStepFallbackIntermediate(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	localExportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(localExportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drv1Content := &zbstore.Derivation{
		Name:    "hello2.txt",
		Dir:     dir,
		Builder: "false", // Prevent from running.
		System:  system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	drv2Content := &zbstore.Derivation{
		Name:   "hello4.txt",
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in": zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv1Path,
				OutputName: zbstore.DefaultDerivationOutputName,
			}),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
			drv1Path: sets.NewSorted(zbstore.DefaultDerivationOutputName),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv2Content.Builder, drv2Content.Args = catcatBuilder()
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	fallbackExportBuffer := new(bytes.Buffer)
	exporter = zbstore.NewExportWriter(fallbackExportBuffer)
	const wantOutputContent1 = inputContent + inputContent
	wantOutputPath1, _, err := storetest.ExportSourceFile(exporter, []byte(wantOutputContent1), storetest.SourceExportOptions{
		Name:      drv1Content.Name,
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	fallbackStore := new(storetest.Store)
	if err := fallbackStore.StoreImport(ctx, fallbackExportBuffer); err != nil {
		t.Fatal(err)
	}
	drv1Hash, err := drv1Content.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	fallbackStore.AddRealization(
		zbstore.RealizationOutputReference{
			DerivationHash: drv1Hash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		},
		&zbstore.Realization{OutputPath: wantOutputPath1},
	)

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, localExportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog1, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv1Path)
		gotLog2, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv2Path)
		t.Fatalf("build drv: %v\nlog 1:\n%s\nlog 2:\n%s", err, gotLog1, gotLog2)
	}

	const wantOutputContent2 = wantOutputContent1 + wantOutputContent1
	wantOutputPath2, err := singleFileOutputPath(dir, drv2Content.Name, []byte(wantOutputContent2), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drv2Path, wantOutputPath2, []byte(wantOutputContent2), got)
}

// TestRealizeMultiStepFallbackMissingObject tests a build of drv2 depending on drv1,
// with a fallback store that has a full realization chain
// and only includes the output store object for drv1.
// This checks whether the build can recover from having realizations that can't actually be used.
func TestRealizeMultiStepFallbackMissingObject(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	const inputContent = "Hello, World!\n"
	localExportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(localExportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	drv1Content := &zbstore.Derivation{
		Name:    "hello2.txt",
		Dir:     dir,
		Builder: "false", // Prevent from running.
		System:  system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv1Path, _, err := storetest.ExportDerivation(exporter, drv1Content)
	if err != nil {
		t.Fatal(err)
	}
	drv2Content := &zbstore.Derivation{
		Name:   "hello4.txt",
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in": zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv1Path,
				OutputName: zbstore.DefaultDerivationOutputName,
			}),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
			drv1Path: sets.NewSorted(zbstore.DefaultDerivationOutputName),
		},
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drv2Content.Builder, drv2Content.Args = catcatBuilder()
	drv2Path, _, err := storetest.ExportDerivation(exporter, drv2Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	fallbackExportBuffer := new(bytes.Buffer)
	exporter = zbstore.NewExportWriter(fallbackExportBuffer)
	const wantOutputContent1 = inputContent + inputContent
	wantOutputPath1, _, err := storetest.ExportSourceFile(exporter, []byte(wantOutputContent1), storetest.SourceExportOptions{
		Name:      drv1Content.Name,
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	fallbackStore := new(storetest.Store)
	if err := fallbackStore.StoreImport(ctx, fallbackExportBuffer); err != nil {
		t.Fatal(err)
	}
	drv1Hash, err := drv1Content.SHA256RealizationHash(nil)
	if err != nil {
		t.Fatal(err)
	}
	drv2Hash, err := drv2Content.SHA256RealizationHash(func(ref zbstore.OutputReference) (zbstore.Path, error) {
		switch {
		case ref.DrvPath == drv1Path && ref.OutputName == zbstore.DefaultDerivationOutputName:
			return wantOutputPath1, nil
		}
		return "", fmt.Errorf("missing realization for %v", ref)
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackStore.AddRealization(
		zbstore.RealizationOutputReference{
			DerivationHash: drv1Hash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		},
		&zbstore.Realization{OutputPath: wantOutputPath1},
	)
	// Intentionally using a bad path here to see if it poisons the builder state.
	phonyPath, err := dir.Object("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-" + drv2Content.Name)
	if err != nil {
		t.Fatal(err)
	}
	fallbackStore.AddRealization(
		zbstore.RealizationOutputReference{
			DerivationHash: drv2Hash,
			OutputName:     zbstore.DefaultDerivationOutputName,
		},
		&zbstore.Realization{OutputPath: phonyPath},
	)

	_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
		TempDir: t.TempDir(),
		Options: Options{
			Fallback: fallbackStore,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, releaseCodec, err := storeCodec(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	err = codec.Export(nil, localExportBuffer)
	releaseCodec()
	if err != nil {
		t.Fatal(err)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{drv2Path},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal("RPC error:", err)
	}

	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		gotLog1, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv1Path)
		gotLog2, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drv2Path)
		t.Fatalf("build drv: %v\nlog 1:\n%s\nlog 2:\n%s", err, gotLog1, gotLog2)
	}

	const wantOutputContent2 = wantOutputContent1 + wantOutputContent1
	wantOutputPath2, err := singleFileOutputPath(dir, drv2Content.Name, []byte(wantOutputContent2), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, drv2Path, wantOutputPath2, []byte(wantOutputContent2), got)
}

// TestRealizeIssue288 is a regression test for https://github.com/256lights/zb/issues/288.
func TestRealizeIssue288(t *testing.T) {
	ctx := testcontext.New(t)
	dir := backendtest.NewStoreDirectory(t)

	exportArchive, err := txtar.ParseFile(filepath.Join("testdata", "TestRealizeIssue288.txt"))
	if err != nil {
		t.Fatal(err)
	}
	objects, _, err := storetest.TxtarObjects(dir, exportArchive.Files)
	if err != nil {
		t.Fatal(err)
	}
	partialDrvObject, err := findObjectWithName(derivationNameForCurrentSystem("bye2.txt"), slices.Values(objects))
	if err != nil {
		t.Fatal(err)
	}
	finalDrvObject, err := findObjectWithName(derivationNameForCurrentSystem("final.txt"), slices.Values(objects))
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

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{partialDrvObject.StorePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID); err != nil {
		t.Errorf("build failed: %v", err)
		for obj := range derivationsForCurrentSystem(slices.Values(objects)) {
			gotLog, err := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, obj.StorePath)
			if err == nil {
				t.Logf("log for %s:\n%s", obj.StorePath, gotLog)
			}
		}
		t.FailNow()
	}
	realizeResponse = new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: []zbstore.Path{finalDrvObject.StorePath},
		Reuse:    &zbstorerpc.ReusePolicy{All: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
	if err != nil {
		t.Errorf("build failed: %v", err)
		for obj := range derivationsForCurrentSystem(slices.Values(objects)) {
			gotLog, err := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, obj.StorePath)
			if err == nil {
				t.Logf("log for %s:\n%s", obj.StorePath, gotLog)
			}
		}
		t.FailNow()
	}

	wantOutputContent := strings.Repeat("Hello, World!\n", 4) + strings.Repeat("Good-bye\n", 2)
	finalDrvOutputName, _ := finalDrvObject.StorePath.DerivationName()
	wantOutputPath, err := singleFileOutputPath(dir, finalDrvOutputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}
	checkSingleFileOutput(t, finalDrvObject.StorePath, wantOutputPath, []byte(wantOutputContent), got)
}

func TestRealizeUpload(t *testing.T) {
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

	const inputContent = "Hello, World!\n"
	exportBuffer := new(bytes.Buffer)
	exporter := zbstore.NewExportWriter(exportBuffer)
	inputFilePath, _, err := storetest.ExportSourceFile(exporter, []byte(inputContent), storetest.SourceExportOptions{
		Name:      "hello.txt",
		Directory: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantOutputName = "hello2.txt"
	drvContent := &zbstore.Derivation{
		Name:   wantOutputName,
		Dir:    dir,
		System: system.Current().String(),
		Env: map[string]string{
			"in":  string(inputFilePath),
			"out": zbstore.HashPlaceholder("out"),
		},
		InputSources: *sets.NewSorted(
			inputFilePath,
		),
		Outputs: map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		},
	}
	drvContent.Builder, drvContent.Args = catcatBuilder()
	drvPath, _, err := storetest.ExportDerivation(exporter, drvContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
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

	synctest.Test(t, func(t *testing.T) {
		ctx := testlog.WithTB(t.Context(), t)
		_, client, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
			TempDir: t.TempDir(),
			Options: Options{
				Upload: uploadStore,
				Keyring: &Keyring{
					Ed25519: []ed25519.PrivateKey{testKey},
				},
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

		realizeResponse := new(zbstorerpc.RealizeResponse)
		err = jsonrpc.Do(ctx, client, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
			DrvPaths: []zbstore.Path{drvPath},
		})
		if err != nil {
			t.Fatal("RPC error:", err)
		}
		if realizeResponse.BuildID == "" {
			t.Fatal("no build ID returned")
		}

		_, err = backendtest.WaitForSuccessfulBuild(ctx, client, realizeResponse.BuildID)
		if err != nil {
			gotLog, _ := backendtest.ReadLog(ctx, client, realizeResponse.BuildID, drvPath)
			t.Fatalf("build drv: %v\nlog:\n%s", err, gotLog)
		}

		// Wait for background goroutines to finish.
		synctest.Wait()
	})

	const wantOutputContent = "Hello, World!\nHello, World!\n"
	wantOutputPath, err := singleFileOutputPath(dir, wantOutputName, []byte(wantOutputContent), zbstore.References{})
	if err != nil {
		t.Fatal(err)
	}

	// Object existence is sufficient: [zbstorehttp.Store] already verifies.
	if _, err := uploadStore.Object(ctx, wantOutputPath); err != nil {
		t.Error(err)
	}

	// Verify realizations.
	drvHash, err := drvContent.SHA256RealizationHash(nil)
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

var ignoreDrvHashOption = cmp.FilterPath(func(p cmp.Path) bool {
	return isFieldAnyOf[zbstorerpc.BuildResult](p, "DrvHash")
}, cmp.Ignore())

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

func checkSingleFileOutput(tb testing.TB, drvPath, wantOutputPath zbstore.Path, wantOutputContent []byte, resp *zbstorerpc.Build) {
	tb.Helper()

	got, err := resp.ResultForPath(drvPath)
	if err != nil {
		tb.Error(err)
	}
	if got == nil {
		return
	}

	want := &zbstorerpc.BuildResult{
		DrvPath: drvPath,
		Status:  zbstorerpc.BuildSuccess,
		Outputs: []*zbstorerpc.RealizeOutput{
			{
				Name: zbstore.DefaultDerivationOutputName,
				Path: zbstorerpc.NonNull(wantOutputPath),
			},
		},
	}
	diff := cmp.Diff(
		want, got,
		buildResultOption,
		ignoreDrvHashOption,
		cmp.FilterPath(isRealizeOutputSignaturesField, cmp.Ignore()),
	)
	if diff != "" {
		tb.Errorf("realize response (-want +got):\n%s", diff)
	}

	// Try to compare the file if the response is the right shape.
	gotOutput, err := got.OutputForName(zbstore.DefaultDerivationOutputName)
	if err != nil {
		tb.Errorf("%s: %v", drvPath, err)
		return
	}
	if !gotOutput.Path.Valid {
		tb.Errorf("%s: output %s path is null", drvPath, zbstore.DefaultDerivationOutputName)
		return
	}
	gotOutputPath := gotOutput.Path.X

	if got, err := os.ReadFile(string(gotOutputPath)); err != nil {
		tb.Error(err)
	} else if !bytes.Equal(got, wantOutputContent) {
		tb.Errorf("%s content = %q; want %q", wantOutputPath, got, wantOutputContent)
	}
	if info, err := os.Lstat(string(gotOutputPath)); err != nil {
		tb.Error(err)
	} else if got := info.Mode(); got&0o111 != 0 {
		tb.Errorf("%s mode = %v; want non-executable", gotOutputPath, got)
	}
}

// catcatBuilder returns a builder that writes $in twice to $out
// with no dependencies other than the system shell.
// As a side-effect, it echoes "catcat" to its log to signal its execution.
func catcatBuilder() (builder string, builderArgs []string) {
	if runtime.GOOS == "windows" {
		return powershellPath, []string{
			"-Command",
			`Write-Output "catcat" ; $x = Get-Content -Raw ${env:in} ; ($x + $x) | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}`,
		}
	}
	return shPath, []string{
		"-c",
		`echo catcat >&2 ; while read line; do echo "$line"; echo "$line"; done < $in > $out`,
	}
}

func singleFileOutputPath(dir zbstore.Directory, name string, data []byte, refs zbstore.References) (zbstore.Path, error) {
	wantOutputNAR := new(bytes.Buffer)
	if err := storetest.SingleFileNAR(wantOutputNAR, []byte(data)); err != nil {
		return "", err
	}
	ca, _, err := zbstore.SourceSHA256ContentAddress(bytes.NewReader(wantOutputNAR.Bytes()), nil)
	if err != nil {
		return "", err
	}
	p, err := zbstore.FixedCAOutputPath(dir, name, ca, refs)
	if err != nil {
		return "", err
	}
	return p, nil
}

func derivationNameForCurrentSystem(name string) string {
	suffixStart := strings.LastIndexByte(name, '.')
	if suffixStart == -1 {
		suffixStart = len(name)
	}
	return name[:suffixStart] + "-" + system.Current().String() + name[suffixStart:] + zbstore.DerivationExt
}

func derivationsForCurrentSystem[O zbstore.Object](objects iter.Seq[O]) iter.Seq[O] {
	systemString := "-" + system.Current().String()
	return func(yield func(O) bool) {
		for obj := range objects {
			name := obj.Info().StorePath.Name()
			if !strings.Contains(name, systemString) || !strings.HasSuffix(name, zbstore.DerivationExt) {
				continue
			}
			if !yield(obj) {
				return
			}
		}
	}
}

func findObjectWithName[O zbstore.Object](name string, objects iter.Seq[O]) (O, error) {
	for obj := range objects {
		if obj.Info().StorePath.Name() == name {
			return obj, nil
		}
	}
	var zero O
	return zero, fmt.Errorf("%s not found", name)
}

func mustParseHash(tb testing.TB, s string) nix.Hash {
	tb.Helper()
	h, err := nix.ParseHash(s)
	if err != nil {
		tb.Fatal(err)
	}
	return h
}
