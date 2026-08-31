// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
)

func TestClientJSONRPC(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			var gotParams []any
			id, ok := validateRequest(t, jsonRequest, "subtract", &gotParams)
			wantParams := []any{42.0, 23.0}
			if diff := cmp.Diff(wantParams, gotParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			response := `{"jsonrpc":"2.0","id":` + id.String() + `,"result":19}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(response))},
				},
				strings.NewReader(response),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	var got int
	if err := jsonrpc.Do(ctx, client, "subtract", &got, []int{42, 23}); err != nil {
		t.Error(err)
	}
	if want := 19; got != want {
		t.Errorf("result = %d; want %d", got, want)
	}
}

func TestClientStoreExport(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			gotParams := new(ExportRequest)
			id, exportID, ok := validateExportRequest(t, jsonRequest, gotParams)
			wantParams := &ExportRequest{
				Paths:             []zbstore.Path{"/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"},
				ExcludeReferences: false,
			}
			if diff := cmp.Diff(wantParams, gotParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":     {exportContentType},
					exportIDHeaderName: {exportID},
				},
				strings.NewReader(emptyExport),
			)
			if err != nil {
				t.Error(err)
				return
			}
			response := `{"jsonrpc":"2.0","id":` + id.String() + `,"result":null}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(response))},
				},
				strings.NewReader(response),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	got := new(bytes.Buffer)
	paths := sets.New(zbstore.Path("/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"))
	if err := client.StoreExport(ctx, got, paths, nil); err != nil {
		t.Error(err)
	}
	if got.String() != emptyExport {
		t.Errorf("export = %q; want %q", got, emptyExport)
	}
}

func TestClientStoreExportUnreceived(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			gotParams := new(ExportRequest)
			id, _, ok := validateExportRequest(t, jsonRequest, gotParams)
			wantParams := &ExportRequest{
				Paths:             []zbstore.Path{"/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"},
				ExcludeReferences: false,
			}
			if diff := cmp.Diff(wantParams, gotParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			response := `{"jsonrpc":"2.0","id":` + id.String() + `,"result":null}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(response))},
				},
				strings.NewReader(response),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	got := new(bytes.Buffer)
	paths := sets.New(zbstore.Path("/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"))
	if err := client.StoreExport(ctx, got, paths, nil); err == nil {
		t.Error("StoreExport did not return an error")
	} else {
		t.Log("StoreExport:", err)
	}
}

func TestClientStoreExportError(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			gotParams := new(ExportRequest)
			id, _, ok := validateExportRequest(t, jsonRequest, gotParams)
			wantParams := &ExportRequest{
				Paths:             []zbstore.Path{"/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"},
				ExcludeReferences: false,
			}
			if diff := cmp.Diff(wantParams, gotParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			response := `{"jsonrpc":"2.0","id":` + id.String() + `,"error":{"code": -32603, "message": "BORK BORK"}}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(response))},
				},
				strings.NewReader(response),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	got := new(bytes.Buffer)
	paths := sets.New(zbstore.Path("/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"))
	if err := client.StoreExport(ctx, got, paths, nil); err == nil {
		t.Error("StoreExport did not return an error")
	} else if got := err.Error(); !strings.Contains(got, "BORK BORK") {
		t.Errorf("StoreExport: %s; want to contain BORK BORK", got)
	}
}

func TestClientStoreImport(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, _, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), exportContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
				return
			}
			gotExport := new(bytes.Buffer)
			if err := (zbstore.Null{}).StoreImport(ctx, io.TeeReader(r, gotExport)); err != nil {
				t.Error("Receive export:", err)
			}
			if gotExport.String() != emptyExport {
				t.Errorf("export = %q; want %q", gotExport, emptyExport)
			}

			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			var gotParams jsontext.Value
			id, ok := validateRequest(t, jsonRequest, NopMethod, &gotParams)
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			response := `{"jsonrpc":"2.0","id":` + id.String() + `,"result":null}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(response))},
				},
				strings.NewReader(response),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	if err := client.StoreImport(ctx, strings.NewReader(emptyExport)); err != nil {
		t.Error("StoreImport:", err)
	}
}

func TestClientObject(t *testing.T) {
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait()

	archive := txtar.Parse([]byte("" +
		"-- mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt --\n" +
		"Hello, World!\n"))
	objects, _, err := storetest.TxtarObjects(zbstore.DefaultUnixDirectory, archive.Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		object.NARHash = nix.NewHash(nix.SHA256, new(sha256.Sum256(object.NAR))[:])
	}

	client := NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		serverConn, clientConn := net.Pipe()
		wg.Go(func() {
			defer serverConn.Close()

			r := jsonrpc.NewReader(serverConn)
			header, bodySize, err := r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err := io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			gotInfoParams := new(InfoRequest)
			id, ok := validateRequest(t, jsonRequest, InfoMethod, gotInfoParams)
			wantInfoParams := &InfoRequest{
				Path: objects[0].StorePath,
			}
			if diff := cmp.Diff(wantInfoParams, gotInfoParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			w := jsonrpc.NewWriter(serverConn)
			infoResponse, err := jsonv2.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": &InfoResponse{
					Info: NewObjectInfo(objects[0].Info()),
				},
			})
			if err != nil {
				t.Error(err)
				return
			}
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(infoResponse))},
				},
				bytes.NewReader(infoResponse),
			)
			if err != nil {
				t.Error(err)
				return
			}

			header, bodySize, err = r.NextMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if got, want := header.Get("Content-Type"), rpcContentType; got != want {
				t.Errorf("Content-Type = %+q; want %+q", got, want)
			}
			if bodySize < 0 {
				t.Error("unsized response")
				return
			}
			jsonRequest, err = io.ReadAll(r)
			if err != nil {
				t.Error(err)
			}
			gotExportParams := new(ExportRequest)
			id, exportID, ok := validateExportRequest(t, jsonRequest, gotExportParams)
			wantExportParams := &ExportRequest{
				Paths:             []zbstore.Path{objects[0].StorePath},
				ExcludeReferences: true,
			}
			if diff := cmp.Diff(wantExportParams, gotExportParams); diff != "" {
				t.Errorf("params (-want +got):\n%s", diff)
			}
			if !ok {
				return
			}

			exportBuffer := new(bytes.Buffer)
			exportWriter := zbstore.NewExportWriter(exportBuffer)
			if err := exportWriter.WriteObject(ctx, objects[0]); err != nil {
				t.Error(err)
			}
			if err := exportWriter.Close(); err != nil {
				t.Error(err)
			}
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":     {exportContentType},
					exportIDHeaderName: {exportID},
				},
				exportBuffer,
			)
			if err != nil {
				t.Error(err)
				return
			}

			exportRPCResponse := `{"jsonrpc":"2.0","id":` + id.String() + `,"result":null}`
			err = w.WriteMessage(
				jsonrpc.Header{
					"Content-Type":   {rpcContentType},
					"Content-Length": {strconv.Itoa(len(exportRPCResponse))},
				},
				strings.NewReader(exportRPCResponse),
			)
			if err != nil {
				t.Error(err)
				return
			}

			// Wait until client closes connection.
			io.Copy(io.Discard, serverConn)
		})
		return clientConn, nil
	})
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("client.Close:", err)
		}
	}()

	object, err := client.Object(ctx, objects[0].StorePath)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(objects[0].Info(), object.Info()); diff != "" {
		t.Errorf("object info (-want +got):\n%s", diff)
	}
	gotNAR := new(bytes.Buffer)
	if err := object.WriteNAR(ctx, gotNAR); err != nil {
		t.Error("WriteNAR:", err)
	}
	if !bytes.Equal(gotNAR.Bytes(), objects[0].NAR) {
		artifactPath := filepath.Join(t.ArtifactDir(), "got.nar")
		os.WriteFile(artifactPath, gotNAR.Bytes(), 0o666)
		os.WriteFile(filepath.Join(t.ArtifactDir(), "want.nar"), objects[0].NAR, 0o666)
		t.Errorf("WriteNAR data did not match want. Wrote to %s", artifactPath)
	}
}

func validateRequest[Params any](tb testing.TB, requestJSON jsontext.Value, method string, args *Params) (id jsontext.Value, ok bool) {
	tb.Helper()

	var m map[string]jsontext.Value
	if err := jsonv2.Unmarshal(requestJSON, &m); err != nil {
		tb.Error(err)
		return nil, false
	}

	var version string
	if err := jsonv2.Unmarshal(m["jsonrpc"], &version); err != nil {
		tb.Error("jsonrpc:", err)
	} else if want := "2.0"; version != want {
		tb.Errorf("jsonrpc = %+q; want %+q", version, want)
	}

	var gotMethod string
	if err := jsonv2.Unmarshal(m["method"], &gotMethod); err != nil {
		tb.Error("method:", err)
	} else if gotMethod != method {
		tb.Errorf("method = %+q; want %+q", gotMethod, method)
	}

	id = m["id"]
	if len(id) == 0 {
		tb.Error("id not set")
	} else if err := jsonv2.Unmarshal(id, new(jsonrpc.RequestID)); err != nil {
		tb.Error("id:", err)
	} else {
		ok = true
	}

	for k := range m {
		if k != "jsonrpc" && k != "id" && k != "method" && k != "params" {
			tb.Errorf("unknown field %q", k)
		}
	}

	if paramsValue := m["params"]; len(paramsValue) > 0 {
		err := jsonv2.Unmarshal(paramsValue, args, jsonv2.RejectUnknownMembers(true))
		if err != nil {
			tb.Error("params:", err)
		}
	}

	return id, ok
}

func validateExportRequest(tb testing.TB, requestJSON jsontext.Value, args *ExportRequest) (id jsontext.Value, exportID string, ok bool) {
	tb.Helper()

	var m map[string]jsontext.Value
	if err := jsonv2.Unmarshal(requestJSON, &m); err != nil {
		tb.Error(err)
		return nil, "", false
	}

	var version string
	if err := jsonv2.Unmarshal(m["jsonrpc"], &version); err != nil {
		tb.Error("jsonrpc:", err)
	} else if want := "2.0"; version != want {
		tb.Errorf("jsonrpc = %+q; want %+q", version, want)
	}

	var gotMethod string
	if err := jsonv2.Unmarshal(m["method"], &gotMethod); err != nil {
		tb.Error("method:", err)
	} else if want := ExportMethod; gotMethod != want {
		tb.Errorf("method = %+q; want %+q", gotMethod, want)
	}

	id = m["id"]
	idOK := false
	if len(id) == 0 {
		tb.Error("id not set")
	} else if err := jsonv2.Unmarshal(id, new(jsonrpc.RequestID)); err != nil {
		tb.Error("id:", err)
	} else {
		idOK = true
	}

	exportIDOK := false
	if exportIDJSON := m[exportIDExtraFieldName]; len(exportIDJSON) == 0 {
		tb.Errorf("%s not set", exportIDExtraFieldName)
	} else if err := jsonv2.Unmarshal(exportIDJSON, &exportID); err != nil {
		tb.Errorf("%s: %v", exportIDExtraFieldName, err)
	} else if exportID == "" {
		tb.Errorf("%s is empty", exportIDExtraFieldName)
	} else {
		exportIDOK = true
	}

	for k := range m {
		if k != "jsonrpc" && k != "id" && k != "method" && k != "params" && k != exportIDExtraFieldName {
			tb.Errorf("unknown field %q", k)
		}
	}

	err := jsonv2.Unmarshal(m["params"], args, jsonv2.RejectUnknownMembers(true))
	if err != nil {
		tb.Error("params:", err)
	}

	return id, exportID, idOK && exportIDOK
}
