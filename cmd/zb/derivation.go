// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/frontend"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/xiter"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

type derivationCommand struct {
	Env  derivationEnvCommand  `kong:"cmd"`
	Show derivationShowCommand `kong:"cmd"`
}

func (c *derivationCommand) Signature() string {
	return `help:"Query derivations."`
}

type derivationShowCommand struct {
	evalOptions `kong:"embed"`
	JSONFormat  bool `kong:"name=json,help=Print derivation as JSON."`
}

func (c *derivationShowCommand) Signature() string {
	return `help:"Show the contents of one or more derivations."`
}

func (c *derivationShowCommand) Run(ctx context.Context, g *globalConfig, stdio *standardStreams, env envLookupFunc) error {
	args, err := c.resolveArgs(stdio.workdir)
	if err != nil {
		return err
	}
	var drvPaths []string
	if !c.Expression {
		// The handling of arguments for `derivation show` is slightly different than other commands.
		// If the user passes .drv file paths as arguments,
		// we'll show the .drv file directly rather than trying to interpret it as a Lua file.
		// These can be interspersed with other URLs.
		drvPaths = make([]string, len(args))
		for i, arg := range args {
			u, err := url.Parse(arg)
			if err != nil {
				return err
			}
			if (u.Scheme == "" || u.Scheme == fileurl.Scheme) && u.Fragment == "" &&
				strings.HasSuffix(u.Path, zbstore.DerivationExt) {
				drvPaths[i], err = frontend.URLToPath(u)
				if err != nil {
					return err
				}
			}
		}
		if !slices.Contains(drvPaths, "") {
			// Fast path: don't connect to the store. All arguments are local paths to .drv files.
			for _, drvPath := range drvPaths {
				drvBytes, err := showDerivationFile(stdio.abs(drvPath), c.JSONFormat)
				if err != nil {
					return err
				}
				if !c.JSONFormat && len(args) > 1 {
					drvBytes = append(drvBytes, '\n')
				}
				if _, err := stdio.out.Write(drvBytes); err != nil {
					return err
				}
			}
			return nil
		}
	}

	httpClient, cleanup, err := g.newHTTPClient()
	if err != nil {
		return err
	}
	defer cleanup()
	storeClient := g.openLocalStore(ctx)
	defer storeClient.Close()
	eval, err := c.newEval(g, stdio, env, httpClient, storeClient)
	if err != nil {
		return err
	}
	defer func() {
		if err := eval.Close(); err != nil {
			log.Errorf(ctx, "%v", err)
		}
	}()

	var results frontend.OutputMap
	if c.Expression {
		results, err = eval.Expression(ctx, args[0], system.Current())
	} else {
		urls := make([]string, 0, len(args))
		for i, arg := range args {
			if drvPaths[i] != "" {
				continue
			}
			u, err := resolveURL(stdio.workdir, arg)
			if err != nil {
				return err
			}
			urls = append(urls, u.String())
		}
		results, err = eval.URLs(ctx, urls, system.Current())
	}
	if err != nil {
		return err
	}
	hasMultiple := len(args) > 1 || len(results.DerivationPaths(1)) > 1

	groupIndex := 1
	for i := range args {
		var curr iter.Seq[string]
		if i < len(drvPaths) && drvPaths[i] != "" {
			curr = slices.Values(drvPaths[i : i+1])
		} else {
			storePaths := results.DerivationPaths(groupIndex)
			groupIndex++
			curr = func(yield func(string) bool) {
				for _, p := range storePaths {
					if !yield(string(p)) {
						return
					}
				}
			}
		}
		for p := range curr {
			drvBytes, err := showDerivationFile(stdio.abs(p), c.JSONFormat)
			if err != nil {
				return err
			}
			if !c.JSONFormat && hasMultiple {
				drvBytes = append(drvBytes, '\n')
			}
			if _, err := stdio.out.Write(drvBytes); err != nil {
				return err
			}
		}
	}

	return nil
}

func showDerivationFile(drvPath string, jsonFormat bool) ([]byte, error) {
	drvPath, err := filepath.Abs(drvPath)
	if err != nil {
		return nil, err
	}
	dir, err := zbstore.CleanDirectory(filepath.Dir(drvPath))
	if err != nil {
		return nil, err
	}
	drvBytes, err := os.ReadFile(drvPath)
	if err != nil {
		return nil, err
	}
	if !jsonFormat {
		// If we're not outputting JSON, no need to parse. Pass through, even if it's invalid.
		return drvBytes, nil
	}
	drv, err := zbstore.ParseDerivation(dir, inferDerivationName(drvPath), drvBytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %v", drvPath, err)
	}

	jsonData, err := marshalDerivationJSON(drvPath, drv)
	if err != nil {
		return nil, err
	}
	jsonData = append(jsonData, '\n')
	return jsonData, nil
}

func inferDerivationName(path string) string {
	baseName := filepath.Base(path)
	// Strip digest if the path looks like a store object.
	if path, err := zbstore.DefaultDirectory().Object(baseName); err == nil {
		baseName = path.Name()
	}
	return strings.TrimSuffix(baseName, zbstore.DerivationExt)
}

func marshalDerivationJSON(drvPath string, drv *zbstore.Derivation) ([]byte, error) {
	type jsonDerivationOutputType struct {
		Path          string `json:"path,omitempty"`
		HashType      string `json:"hashAlgo,omitempty"`
		HashRawBase16 string `json:"hash,omitempty"`
	}

	type jsonOutputReference struct {
		DrvPath    string `json:"drvPath"`
		OutputName string `json:"outputName"`
	}

	type jsonDerivation struct {
		Path    string            `json:"drvPath"`
		Name    string            `json:"name"`
		System  string            `json:"system"`
		Builder string            `json:"builder"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`

		InputSources     []string            `json:"inputSrcs"`
		InputDerivations map[string][]string `json:"inputDrvs"`

		Outputs map[string]jsonDerivationOutputType `json:"outputs"`

		Placeholders map[string]jsonOutputReference `json:"placeholders"`
	}

	j := &jsonDerivation{
		Path:    drvPath,
		Name:    drv.Name,
		System:  drv.System,
		Builder: drv.Builder,
		Args:    drv.Args,
		Env:     drv.Env,

		InputSources: collectStringSlice(drv.InputSources.Values()),
		InputDerivations: maps.Collect(func(yield func(string, []string) bool) {
			for drvPath, outputs := range drv.InputDerivations {
				if !yield(string(drvPath), collectStringSlice(outputs.Values())) {
					return
				}
			}
		}),
		Outputs: maps.Collect(func(yield func(string, jsonDerivationOutputType) bool) {
			for outputName, outputType := range drv.Outputs {
				var j jsonDerivationOutputType
				if p, err := drv.OutputPath(outputName); err == nil {
					j.Path = string(p)
				}
				if ht, ok := outputType.HashType(); ok {
					j.HashType = ht.String()
					if outputType.IsRecursiveFile() {
						j.HashType = "r:" + j.HashType
					}
				}
				if ca, ok := outputType.FixedCA(); ok {
					j.HashRawBase16 = ca.Hash().RawBase16()
				}
				if !yield(outputName, j) {
					return
				}
			}
		}),
		Placeholders: maps.Collect(func(yield func(string, jsonOutputReference) bool) {
			for outputName := range drv.Outputs {
				placeholder := zbstore.HashPlaceholder(outputName)
				jref := jsonOutputReference{
					DrvPath:    drvPath,
					OutputName: outputName,
				}
				if !yield(placeholder, jref) {
					return
				}
			}
			for inputRef := range drv.InputDerivationOutputs() {
				placeholder := zbstore.UnknownCAOutputPlaceholder(inputRef)
				jref := jsonOutputReference{
					DrvPath:    string(inputRef.DrvPath),
					OutputName: inputRef.OutputName,
				}
				if !yield(placeholder, jref) {
					return
				}
			}
		}),
	}

	data, err := jsonv2.Marshal(j, jsonv2.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("marshal derivation %s: %v", drvPath, err)
	}
	return data, nil
}

type derivationEnvCommand struct {
	evalOptions
	JSONFormat bool   `kong:"name=json,help=Print environments as JSON."`
	TempDir    string `kong:"default=${temp_dir},help=Fill in temporary directory with the given string."`
}

func (c *derivationEnvCommand) Signature() string {
	return `help:"Show the environment of one or more derivations."`
}

func (c *derivationEnvCommand) Validate() error {
	if len(c.Args) != 1 {
		return fmt.Errorf("accepts 1 arg, received %d", len(c.Args))
	}
	return nil
}

func (c *derivationEnvCommand) Run(ctx context.Context, g *globalConfig, stdio *standardStreams, env envLookupFunc) error {
	httpClient, cleanup, err := g.newHTTPClient()
	if err != nil {
		return err
	}
	defer cleanup()
	storeClient := g.openLocalStore(ctx)
	defer storeClient.Close()
	eval, err := c.newEval(g, stdio, env, httpClient, storeClient)
	if err != nil {
		return err
	}
	defer func() {
		if err := eval.Close(); err != nil {
			log.Errorf(ctx, "%v", err)
		}
	}()

	args, err := c.resolveArgs(stdio.workdir)
	if err != nil {
		return err
	}
	var results frontend.OutputMap
	if c.Expression {
		results, err = eval.Expression(ctx, args[0], system.Current())
	} else {
		results, err = eval.URLs(ctx, args, system.Current())
	}
	if err != nil {
		return err
	}
	drvPath, err := xiter.Single(slices.Values(results.DerivationPaths(1)))
	if err != nil {
		return fmt.Errorf("get derivation: %v", err)
	}

	expandResponse := new(zbstorerpc.RealizeResponse)
	err = jsonrpc.Do(ctx, storeClient, zbstorerpc.ExpandMethod, expandResponse, &zbstorerpc.ExpandRequest{
		DrvPath:            drvPath,
		TemporaryDirectory: c.TempDir,
		Reuse:              c.reusePolicy(g),
	})
	if err != nil {
		return err
	}
	build, rawBuild, err := waitForBuild(ctx, storeClient, expandResponse.BuildID, stdio.err)
	if err != nil {
		return err
	}
	if build.Expand == nil {
		return fmt.Errorf("build %s did not provide expand information", expandResponse.BuildID)
	}
	if c.JSONFormat {
		// Dump expand response directly to preserve unknown fields.
		var parsed struct {
			Expand jsontext.Value `json:"expand"`
		}
		if err := jsonv2.Unmarshal(rawBuild, &parsed); err != nil {
			return fmt.Errorf("%s: %v", drvPath, err)
		}
		if err := parsed.Expand.Compact(); err != nil {
			return fmt.Errorf("%s: %v", drvPath, err)
		}
		jsonBytes := append(slices.Clip([]byte(parsed.Expand)), '\n')
		if _, err := stdio.out.Write(jsonBytes); err != nil {
			return err
		}
		return nil
	}

	for k, v := range xmaps.Sorted(build.Expand.Env) {
		if _, err := fmt.Fprintf(stdio.out, "%s=%s\n", k, v); err != nil {
			return err
		}
	}

	return nil
}

func collectStringSlice[S ~string](seq iter.Seq[S]) []string {
	var slice []string
	for s := range seq {
		slice = append(slice, string(s))
	}
	return slice
}
