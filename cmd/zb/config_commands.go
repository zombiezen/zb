// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
)

type configurationCommand struct {
	List *configurationListCommand `kong:"cmd"`
}

func (c *configurationCommand) Signature() string {
	return `help:"Manage configuration."`
}

type configurationListCommand struct {
}

func (c *configurationListCommand) Signature() string {
	return `help:"List variables set in configuration."`
}

func (c *configurationListCommand) Run(g *globalConfig, stdio *standardStreams) error {
	data, err := jsonv2.Marshal(g, jsonv2.Deterministic(true), jsontext.Multiline(true))
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := stdio.out.Write(data); err != nil {
		return err
	}
	return nil
}
