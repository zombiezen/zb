// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/zbstore"
)

type privateKeyFile struct {
	Format zbstore.RealizationSignatureFormat `json:"format"`
	Key    []byte                             `json:"key,format:base64"`
}

func (f *privateKeyFile) appendToKeyring(dst *backend.Keyring) error {
	switch f.Format {
	case zbstore.Ed25519SignatureFormat:
		if got, want := len(f.Key), ed25519.SeedSize; got != want {
			return fmt.Errorf("key is wrong size (decoded is %d instead of %d bytes)", got, want)
		}
		dst.Ed25519 = append(dst.Ed25519, ed25519.NewKeyFromSeed(f.Key))
	default:
		return fmt.Errorf("unknown format %q", f.Format)
	}
	return nil
}

func readKeyringFromFiles(files []string) (*backend.Keyring, error) {
	result := new(backend.Keyring)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var parsed privateKeyFile
		if err := jsonv2.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("read %s: %v", path, err)
		}
		if err := parsed.appendToKeyring(result); err != nil {
			return nil, fmt.Errorf("read %s: %v", path, err)
		}
	}
	return result, nil
}

type keyCommand struct {
	Generate   generateKeyCommand   `kong:"cmd"`
	ShowPublic showPublicKeyCommand `kong:"cmd"`
}

func (*keyCommand) Signature() string {
	return `kong:"help=Operate on signing key files."`
}

type generateKeyCommand struct {
	OutputPath string `kong:"name=output,short=o,placeholder=file,help=File to write to. (Default: stdout)"`
}

func (c *generateKeyCommand) Signature() string {
	return `help:"Generate a new signing key."`
}

func (c *generateKeyCommand) Run(ctx context.Context, stdio *standardStreams) error {
	outputFile, err := stdio.openOutputFile(cmp.Or(c.OutputPath, "-"))
	if err != nil {
		return err
	}
	defer outputFile.Close()

	_, newKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	keyFile := &privateKeyFile{
		Format: zbstore.Ed25519SignatureFormat,
		Key:    newKey.Seed(),
	}
	keyFileData, err := jsonv2.Marshal(keyFile, jsontext.Multiline(true))
	if err != nil {
		return err
	}
	keyFileData = append(keyFileData, '\n')
	_, err = outputFile.Write(keyFileData)
	err = errors.Join(err, outputFile.Close())
	return err
}

type showPublicKeyCommand struct {
	Paths []string `kong:"arg,optional,name=file,help=Signing key files."`
}

func (c *showPublicKeyCommand) Signature() string {
	return `help:"Print public key of signing keys."`
}

func (c *showPublicKeyCommand) Run(stdio *standardStreams) error {
	inputPaths := c.Paths
	if len(inputPaths) == 0 {
		inputPaths = []string{"-"}
	}
	for _, path := range inputPaths {
		f, err := stdio.openInputFile(path)
		if err != nil {
			return err
		}
		err = c.run(stdio.out, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *showPublicKeyCommand) run(dst io.Writer, src io.Reader) error {
	keyFile := new(privateKeyFile)
	if err := jsonv2.UnmarshalRead(src, keyFile, jsonv2.RejectUnknownMembers(false)); err != nil {
		return err
	}
	k := new(backend.Keyring)
	if err := keyFile.appendToKeyring(k); err != nil {
		return err
	}
	var result zbstore.RealizationPublicKey
	switch {
	case len(k.Ed25519) > 0:
		result.Format = zbstore.Ed25519SignatureFormat
		result.Data = k.Ed25519[0].Public().(ed25519.PublicKey)
	default:
		return nil
	}
	data, err := jsonv2.Marshal(result, jsontext.Multiline(true))
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = dst.Write(data)
	return err
}
