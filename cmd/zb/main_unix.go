// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

//go:build unix

package main

import (
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

var interruptSignals = []os.Signal{
	unix.SIGTERM,
	unix.SIGINT,
}

var drainSignal os.Signal = unix.SIGUSR2

func ignoreSIGPIPE() {
	signal.Ignore(unix.SIGPIPE)
}
