// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"runtime"
	"strconv"

	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/zbstore"
)

// A runnerFunc is a function that can execute a builder.
//
// A runnerFunc should:
//   - Run the builder program
//     with the working directory
//     (and TMPDIR or equivalent environment variables)
//     set to invocation.buildDir.
//     Mapping the location is acceptable,
//     as long as files are physically stored in invocation.buildDir.
//   - Return a [builderFailure] if the builder did not run successfully
//     (e.g. a user build failure).
//     Any other type of error is treated as an internal backend failure.
//   - Create filesystem objects in invocation.realStoreDir
//     for each output path in invocation.outputPaths.
type runnerFunc func(ctx context.Context, invocation *builderInvocation) error

type builderInvocation struct {
	// derivation is the derivation whose builder should be executed.
	// The caller is responsible for expanding any placeholders
	// in the derivation's fields.
	derivation *zbstore.Derivation
	// derivationPath is the path of the derivation whose builder is being executed.
	derivationPath zbstore.Path
	// outputPaths is the map of output name to path this builder is expected to produce.
	outputPaths map[string]zbstore.Path

	// realStoreDir is the directory where the store is located in the local filesystem.
	realStoreDir string
	// buildDir is the temporary directory created for this build.
	buildDir string
	// logWriter is where all builder output should be sent.
	logWriter io.Writer
	// lookup returns the store path for the given derivation output.
	// lookup should return paths for the inputs to the derivation the runner is building
	// at least.
	lookup func(ref zbstore.OutputReference) (zbstore.Path, error)
	// closure calls yield for each store object
	// in the transitive closure of the store object at the given path.
	closure func(path zbstore.Path, yield func(zbstore.Path) bool) error
	// cores is a hint from the user to the builder
	// on the number of concurrent jobs to perform.
	cores int
	// sandboxPaths is a map of paths inside the sandbox
	// to paths on the host machine.
	// For sandboxed runners, these paths will be made available inside the sandbox.
	sandboxPaths map[string]string
}

func (invocation *builderInvocation) hasNetwork() bool {
	return invocation.derivation.Outputs.IsFixed() ||
		invocation.derivation.Env[networkVar] == "1"
}

// runSubprocess runs a builder by running a subprocess.
// It satisfies the [runnerFunc] signature.
func runSubprocess(ctx context.Context, invocation *builderInvocation) error {
	if string(invocation.derivation.Dir) != invocation.realStoreDir {
		return fmt.Errorf("store is unsandboxed and storage directory does not match store (%s)", invocation.derivation.Dir)
	}

	c := exec.CommandContext(ctx, invocation.derivation.Builder, invocation.derivation.Args...)
	setCancelFunc(c)
	env := maps.Clone(invocation.derivation.Env)
	fillBaseEnv(env, invocation.derivation.Dir, invocation.buildDir, invocation.cores)
	for k, v := range xmaps.Sorted(env) {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Dir = invocation.buildDir
	c.Stdout = invocation.logWriter
	c.Stderr = invocation.logWriter

	if err := c.Run(); err != nil {
		return builderFailure{err}
	}

	return nil
}

func fillBaseEnv(m map[string]string, storeDir zbstore.Directory, workDir string, cores int) {
	if runtime.GOOS == "windows" {
		xmaps.SetDefault(m, "HOME", `C:\home-not-set`)
		xmaps.SetDefault(m, "PATH", `C:\path-not-set`)
		xmaps.SetDefault(m, "TEMP", workDir)
		xmaps.SetDefault(m, "TMP", workDir)
		xmaps.SetDefault(m, "ZB_STORE", string(storeDir))
		xmaps.SetDefault(m, "ZB_BUILD_CORES", strconv.Itoa(cores))
		xmaps.SetDefault(m, "ZB_BUILD_TOP", workDir)
	} else {
		xmaps.SetDefault(m, "HOME", "/home-not-set")
		xmaps.SetDefault(m, "PATH", "/path-not-set")
		xmaps.SetDefault(m, "PWD", workDir)
		xmaps.SetDefault(m, "TEMP", workDir)
		xmaps.SetDefault(m, "TEMPDIR", workDir)
		xmaps.SetDefault(m, "TERM", "xterm-256color")
		xmaps.SetDefault(m, "TMP", workDir)
		xmaps.SetDefault(m, "TMPDIR", workDir)
		xmaps.SetDefault(m, "ZB_BUILD_CORES", strconv.Itoa(cores))
		xmaps.SetDefault(m, "ZB_BUILD_TOP", workDir)
		xmaps.SetDefault(m, "ZB_STORE", string(storeDir))
	}
}
