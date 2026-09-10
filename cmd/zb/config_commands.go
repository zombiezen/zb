// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/tailscale/hujson"
)

type configurationCommand struct {
	Get  *configurationGetCommand  `kong:"cmd"`
	Set  *configurationSetCommand  `kong:"cmd"`
	Path *configurationPathCommand `kong:"cmd"`
}

func (c *configurationCommand) Signature() string {
	return `help:"Manage configuration."`
}

type configurationGetCommand struct {
	Key jsontext.Pointer `kong:"arg,optional,help=JSON pointer to get."`
}

func (c *configurationGetCommand) Signature() string {
	return `help:"Get a configuration value."`
}

func (c *configurationGetCommand) Run(g *globalConfig, stdio *standardStreams) error {
	data, err := jsonv2.Marshal(g, jsonv2.Deterministic(true))
	if err != nil {
		return err
	}
	v, err := findJSON(data, c.Key)
	if err != nil {
		return err
	}
	if err := v.Format(jsontext.Multiline(true)); err != nil {
		return err
	}
	v = append(v, '\n')
	if _, err := stdio.out.Write(v); err != nil {
		return err
	}
	return nil
}

func findJSON(value jsontext.Value, ptr jsontext.Pointer) (jsontext.Value, error) {
	if !ptr.IsValid() {
		return nil, fmt.Errorf("find %s: invalid json pointer", ptr)
	}
	ptrTokens := slices.Collect(ptr.Tokens())
	if len(ptrTokens) == 0 {
		return value, nil
	}

	dec := jsontext.NewDecoder(bytes.NewBuffer(value))
	for ptrTokenIndex, ptrToken := range ptrTokens {
		tok, err := dec.ReadToken()
		switch {
		case err != nil:
			return nil, fmt.Errorf("find %s: %v", ptr, err)
		case tok.Kind() == '{':
		keySearch:
			for {
				keyToken, err := dec.ReadToken()
				if err != nil {
					return nil, fmt.Errorf("find %s: %v", ptr, err)
				}
				switch {
				case keyToken.Kind() == '}':
					return nil, fmt.Errorf("find %s: not found", ptr)
				case keyToken.String() == ptrToken:
					break keySearch
				default:
					if err := dec.SkipValue(); err != nil {
						return nil, fmt.Errorf("find %s: %v", ptr, err)
					}
				}
			}
		case tok.Kind() == '[':
			want, err := strconv.Atoi(ptrToken)
			if err != nil || want < 0 {
				var parentPtr jsontext.Pointer
				for _, tok := range ptrTokens[:ptrTokenIndex] {
					parentPtr = parentPtr.AppendToken(tok)
				}
				return nil, fmt.Errorf("find %s: %s is an array", ptr, parentPtr)
			}
			for range want {
				if dec.PeekKind() == ']' {
					return nil, fmt.Errorf("find %s: not found", ptr)
				}
				if err := dec.SkipValue(); err != nil {
					return nil, fmt.Errorf("find %s: %v", ptr, err)
				}
			}
		default:
			return nil, fmt.Errorf("find %s: not an object or array", ptr)
		}
	}

	v, err := dec.ReadValue()
	if err != nil {
		return nil, fmt.Errorf("find %s: %v", ptr, err)
	}
	return v, nil
}

type configurationSetCommand struct {
	Key   jsontext.Pointer `kong:"arg,help=JSON pointer to set."`
	Value jsontext.Value   `kong:"arg,help=Set to value."`
}

func (c *configurationSetCommand) Signature() string {
	return `help:"Set a configuration value."`
}

func (c *configurationSetCommand) Run(zc *zbCommand, stdio *standardStreams) error {
	path, err := zc.outputConfigFilePath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return &os.PathError{
			Op:   "read",
			Path: path,
			Err:  err,
		}
	}
	value, err := hujson.Parse(data)
	if err != nil {
		return &os.PathError{
			Op:   "read",
			Path: path,
			Err:  err,
		}
	}
	v := value.Find(string(c.Key))
	// TODO(now): Walk up parent if needed.
	panic("TODO(now)")
	_ = v
	return nil
}

type configurationPathCommand struct {
}

func (c *configurationPathCommand) Signature() string {
	return `help:"Print paths read for configuration."`
}

func (c *configurationPathCommand) Run(zc *zbCommand, stdio *standardStreams) error {
	paths := slices.Collect(zc.configFilePaths())
	for _, path := range slices.Backward(paths) {
		if _, err := fmt.Fprintln(stdio.out, path); err != nil {
			return err
		}
	}
	return nil
}
