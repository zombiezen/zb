// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorerpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/zbstore"
)

const (
	// rpcContentType is the MIME media type for zb store API requests.
	rpcContentType = "application/zb-store-rpc+json"
	// exportContentType is the MIME media type for a `nix-store --export` stream.
	exportContentType = "application/zb-store-export"
)

// exportIDHeaderName is the name of the header used to correlate a [jsonrpc.Request]
// with an export message.
// The request should use [exportIDExtraFieldName].
const exportIDHeaderName = "Zb-Export-Id"

const maxAPIMessageSize = 1 << 20 // 1 MiB

func writeRPCMessage(w *jsonrpc.Writer, value jsontext.Value) error {
	hdr := jsonrpc.Header{
		"Content-Length": {strconv.Itoa(len(value))},
		"Content-Type":   {rpcContentType},
	}
	return w.WriteMessage(hdr, bytes.NewReader(value))
}

func writeExport(ctx context.Context, w *jsonrpc.Writer, id string, r io.Reader) error {
	header := make(jsonrpc.Header)
	header.Set("Content-Type", exportContentType)
	if id != "" {
		header.Set(exportIDHeaderName, id)
	}

	// Pass through an importer to stop at the end of the export.
	pr, pw := io.Pipe()
	done := make(chan struct{})
	defer func() { <-done }()
	go func() {
		defer close(done)
		err := zbstore.Null{}.StoreImport(ctx, io.TeeReader(r, pw))
		pw.CloseWithError(err)
	}()

	exportError := w.WriteMessage(header, pr)
	pr.Close()
	return exportError
}

type exportRPCRequest struct {
	id     string
	params *ExportRequest
}

func (req *exportRPCRequest) MarshalJSONTo(enc *jsontext.Encoder) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("marshal json-rpc export request: %v", err)
		}
	}()

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("jsonrpc")); err != nil {
		return fmt.Errorf("marshal json-rpc version: %v", err)
	}
	if err := enc.WriteToken(jsontext.String("2.0")); err != nil {
		return fmt.Errorf("marshal json-rpc version: %v", err)
	}
	if err := enc.WriteToken(jsontext.String("method")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(ExportMethod)); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("id")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(req.id)); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("params")); err != nil {
		return err
	}
	if err := jsonv2.MarshalEncode(enc, req.params); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(exportIDExtraFieldName)); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(req.id)); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	return nil
}
