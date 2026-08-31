// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/google/go-cmp/cmp"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/zbstore"
)

const emptyExport = "\x00\x00\x00\x00\x00\x00\x00\x00"

func TestServeJSONRPC(t *testing.T) {
	ctx := t.Context()
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	defer func() {
		clientConn.Close()
		wg.Wait()
	}()

	srv := new(fakeServer)
	wg.Go(func() {
		Serve(ctx, serverConn, srv)
	})

	w := jsonrpc.NewWriter(clientConn)
	const request = `{"jsonrpc":"2.0","id":100,"method":"subtract","params":[42,23]}`
	err := w.WriteMessage(
		jsonrpc.Header{
			"Content-Type":   {rpcContentType},
			"Content-Length": {strconv.Itoa(len(request))},
		},
		strings.NewReader(request),
	)
	if err != nil {
		t.Fatal(err)
	}

	r := jsonrpc.NewReader(clientConn)
	header, bodySize, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := header.Get("Content-Type"), rpcContentType; got != want {
		t.Errorf("Content-Type = %+q; want %+q", got, want)
	}
	if bodySize < 0 {
		t.Fatal("unsized response")
	}
	jsonResponse, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := jsonv2.Unmarshal(jsonResponse, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"jsonrpc": "2.0",
		"id":      100.0,
		"result":  19.0,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
}

func TestServeImport(t *testing.T) {
	ctx := t.Context()
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	defer func() {
		clientConn.Close()
		wg.Wait()
	}()

	srv := new(fakeServer)
	wg.Go(func() {
		Serve(ctx, serverConn, srv)
	})

	w := jsonrpc.NewWriter(clientConn)
	err := w.WriteMessage(
		jsonrpc.Header{
			"Content-Type": {exportContentType},
		},
		strings.NewReader(emptyExport),
	)
	if err != nil {
		t.Fatal(err)
	}
	const request = `{"jsonrpc":"2.0","id":100,"method":"` + NopMethod + `"}`
	err = w.WriteMessage(
		jsonrpc.Header{
			"Content-Type":   {rpcContentType},
			"Content-Length": {strconv.Itoa(len(request))},
		},
		strings.NewReader(request),
	)
	if err != nil {
		t.Fatal(err)
	}

	r := jsonrpc.NewReader(clientConn)
	header, bodySize, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := header.Get("Content-Type"), rpcContentType; got != want {
		t.Errorf("Content-Type = %+q; want %+q", got, want)
	}
	if bodySize < 0 {
		t.Fatal("unsized response")
	}
	jsonResponse, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := jsonv2.Unmarshal(jsonResponse, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"jsonrpc": "2.0",
		"id":      100.0,
		"result":  nil,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
}

func TestServeExport(t *testing.T) {
	ctx := t.Context()
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	defer func() {
		clientConn.Close()
		wg.Wait()
	}()

	srv := new(fakeServer)
	wg.Go(func() {
		Serve(ctx, serverConn, srv)
	})

	w := jsonrpc.NewWriter(clientConn)
	const request = "" +
		`{"jsonrpc":"2.0",` +
		`"id":100,` +
		`"method":"` + ExportMethod + `",` +
		`"params":{"paths":["/opt/zb/store/mv4z5c5znjdnc40fvqfl1qknszgbdyxd-hello.txt"]},` +
		`"` + exportIDExtraFieldName + `": "xyzzy"}`
	err := w.WriteMessage(
		jsonrpc.Header{
			"Content-Type":   {rpcContentType},
			"Content-Length": {strconv.Itoa(len(request))},
		},
		strings.NewReader(request),
	)
	if err != nil {
		t.Fatal(err)
	}

	r := jsonrpc.NewReader(clientConn)
	header, _, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := header.Get("Content-Type"), exportContentType; got != want {
		t.Fatalf("Content-Type = %+q; want %+q", got, want)
	}
	if got, want := header.Get(exportIDHeaderName), "xyzzy"; got != want {
		t.Errorf("%s = %+q; want %+q", exportIDHeaderName, got, want)
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
		t.Fatal(err)
	}
	if got, want := header.Get("Content-Type"), rpcContentType; got != want {
		t.Errorf("Content-Type = %+q; want %+q", got, want)
	}
	if bodySize < 0 {
		t.Fatal("unsized response")
	}
	jsonResponse, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := jsonv2.Unmarshal(jsonResponse, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"jsonrpc": "2.0",
		"id":      100.0,
		"result":  nil,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
}

type fakeServer struct {
	imports [][]byte
}

func (srv *fakeServer) JSONRPC(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.ServeMux{
		NopMethod: jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
			return nil, nil
		}),
		"subtract":   jsonrpc.HandlerFunc(srv.subtract),
		ExportMethod: jsonrpc.HandlerFunc(srv.export),
	}.JSONRPC(ctx, req)
}

func (srv *fakeServer) subtract(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var params []int64
	if err := jsonv2.Unmarshal(req.Params, &params); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	if len(params) == 0 {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("empty arguments"))
	}
	result := params[0]
	for _, arg := range params[1:] {
		result -= arg
	}
	return &jsonrpc.Response{
		Result: jsontext.Value(strconv.FormatInt(result, 10)),
	}, nil
}

func (srv *fakeServer) export(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	err := ServeExport(ctx, strings.NewReader(emptyExport))
	return nil, err
}

func (srv *fakeServer) StoreImport(ctx context.Context, r io.Reader) error {
	got := new(bytes.Buffer)
	err := zbstore.Null{}.StoreImport(ctx, io.TeeReader(r, got))
	srv.imports = append(srv.imports, got.Bytes())
	return err
}
