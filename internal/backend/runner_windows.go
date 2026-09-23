// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"os/exec"
)

func setCancelFunc(c *exec.Cmd) {
	// Default behavior of exec.CommandContext is fine, no-op.
}
