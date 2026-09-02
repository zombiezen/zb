// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"os"
)

var interruptSignals = []os.Signal{os.Interrupt}

var drainSignal os.Signal

func ignoreSIGPIPE() {}
