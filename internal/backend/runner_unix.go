// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

//go:build unix

package backend

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

func setCancelFunc(c *exec.Cmd) {
	c.Cancel = func() error {
		return c.Process.Signal(unix.SIGTERM)
	}
}
