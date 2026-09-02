// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix/nar"
)

type narCommand struct {
	Pack packNARCommand `kong:"cmd"`
}

func (*narCommand) Signature() string {
	return `help:"Operate on NAR files."`
}

type packNARCommand struct {
	InputPath      string `kong:"arg,name=path,required,help=Filesystem object to serialize."`
	OutputPath     string `kong:"name=output,short=o,placeholder=file,help=Write NAR to file. (Defaults to stdout.)"`
	SelfReferences bool   `kong:"help=Rewrite any self-references and print path to stdout. (Must use with --output.)"`
}

func (c *packNARCommand) Signature() string {
	return `help:"Serialize a filesystem object to NAR format."`
}

func (c *packNARCommand) Validate() error {
	if c.SelfReferences && (c.OutputPath == "" || c.OutputPath == "-") {
		return errors.New("--self-references given without --output")
	}
	return nil
}

func (c *packNARCommand) Run(ctx context.Context, stdio *standardStreams) error {
	outputFile, err := stdio.openOutputFile(c.OutputPath)
	if err != nil {
		return err
	}
	if c.SelfReferences {
		var finalStorePath zbstore.Path
		finalStorePath, err = runPackNARSelfRefs(ctx, outputFile.(io.ReadWriteSeeker), stdio.abs(c.InputPath))
		if _, err := fmt.Fprintln(stdio.out, finalStorePath); err != nil {
			return err
		}
	} else {
		err = nar.DumpPath(outputFile, stdio.abs(c.InputPath))
	}
	err = errors.Join(err, outputFile.Close())
	return err
}

func runPackNARSelfRefs(ctx context.Context, dst io.ReadWriteSeeker, path string) (zbstore.Path, error) {
	type caResult struct {
		ca       zbstore.ContentAddress
		analysis *zbstore.SelfReferenceAnalysis
		err      error
	}

	// Interpret path as a store path, using the parent directory as the store directory.
	// This ensures that the path includes a valid digest.
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	storePath, err := zbstore.ParsePath(path)
	if err != nil {
		return "", err
	}

	// Serialize as NAR, noting any occurrence of the digest as it is written.
	originalDigest := storePath.Digest()
	pr, pw := io.Pipe()
	c := make(chan caResult)
	go func() {
		var result caResult
		result.ca, result.analysis, result.err = zbstore.SourceSHA256ContentAddress(pr, &zbstore.ContentAddressOptions{
			Digest:     originalDigest,
			CreateTemp: bytebuffer.TempFileCreator{Pattern: contentAddressTempFilePattern},
			Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
		})
		c <- result
	}()
	err = nar.DumpPath(io.MultiWriter(pw, dst), path)
	pw.CloseWithError(err)
	result := <-c // Always wait on goroutine.
	if err != nil {
		return "", err
	}
	if result.err != nil {
		return "", err
	}

	// Compute the final store path.
	finalStorePath, err := zbstore.FixedCAOutputPath(storePath.Dir(), storePath.Name(), result.ca, zbstore.References{
		Self: result.analysis.HasSelfReferences(),
	})
	if err != nil {
		return "", err
	}

	// If the digest differs, rewrite each occurrence.
	if finalDigest := finalStorePath.Digest(); finalDigest != originalDigest {
		if err := zbstore.Rewrite(dst, 0, finalDigest, result.analysis.Rewrites); err != nil {
			return "", err
		}
	}
	return finalStorePath, nil
}
