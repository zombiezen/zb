// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/txtar"
	"rsc.io/script"
	"rsc.io/script/scripttest"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/log/testlog"
	"zombiezen.com/go/nix"
)

func TestEndToEnd(t *testing.T) {
	testDataRoot := "testdata"
	const ext = ".txt"
	var testNames []string
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		testName, ok := strings.CutPrefix(path, testDataRoot+string(filepath.Separator))
		if !ok {
			return nil
		}
		testName, ok = strings.CutSuffix(testName, ext)
		if !ok {
			return nil
		}
		if !d.IsDir() && !strings.HasPrefix(filepath.Base(path), ".") {
			testNames = append(testNames, filepath.ToSlash(testName))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, testName := range testNames {
		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			filename := filepath.FromSlash(testDataRoot + "/" + testName + ext)
			archive, err := txtar.ParseFile(filename)
			if err != nil {
				t.Fatal(err)
			}

			workDir := t.TempDir()
			for _, file := range archive.Files {
				inputFilename := filepath.Join(workDir, filepath.FromSlash(file.Name))
				if err := os.MkdirAll(filepath.Dir(inputFilename), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(inputFilename, file.Data, 0o666); err != nil {
					t.Fatal(err)
				}
			}

			storeDir := backendtest.NewStoreDirectory(t)
			tempDir := t.TempDir()
			var initialEnv []string
			if runtime.GOOS == "windows" {
				initialEnv = append(initialEnv,
					"USERPROFILE="+workDir,
					"APPDATA="+filepath.Join(workDir, "AppData", "Roaming"),
					"LOCALAPPDATA="+filepath.Join(workDir, "AppData", "Local"),
					"TMP="+tempDir,
				)
			} else {
				initialEnv = append(initialEnv,
					"HOME="+workDir,
					"TMPDIR="+tempDir,
					// Prevent default of /etc/xdg.
					"XDG_CONFIG_DIRS="+t.TempDir(),
				)
			}
			storeSocketPath := makeSocketPath(t)
			initialEnv = append(initialEnv,
				"PATH="+os.Getenv("PATH"),
				"ZB_STORE_DIR="+string(storeDir),
				"ZB_STORE_SOCKET="+storeSocketPath,
			)

			serveCtx := context.WithValue(ctx, loggerContextKey{}, &log.LevelFilter{
				Min:    log.Warn,
				Output: testlog.Logger{},
			})
			server, err := backendtest.NewServer(serveCtx, t, storeDir, &backendtest.Options{
				TempDir: tempDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			startServerForTest(serveCtx, t, storeSocketPath, server)

			engine := &script.Engine{
				Cmds: scripttest.DefaultCmds(),
				Conds: map[string]script.Cond{
					"exec":    scripttest.CachedExec(),
					"short":   script.BoolCondition("testing.Short()", testing.Short()),
					"verbose": script.BoolCondition("testing.Verbose()", testing.Verbose()),
				},
			}
			addSystemConds(engine.Conds, system.Current())
			engine.Cmds["read"] = readCommand()
			engine.Cmds["zb"] = script.Command(
				script.CmdUsage{
					Summary: "runs zb",
					Args:    "[arg...]",
					Async:   true,
				},
				func(state *script.State, args ...string) (script.WaitFunc, error) {
					c := &zbCommand{
						stdin:     bytebuffer.Null{},
						workdir:   state.Getwd(),
						lookupEnv: state.LookupEnv,
					}
					k, err := c.newKong()
					if err != nil {
						return nil, err
					}
					return func(state *script.State) (stdout string, stderr string, err error) {
						stdoutBuffer := new(strings.Builder)
						k.Stdout = stdoutBuffer
						stderrBuffer := new(strings.Builder)
						stderrWriter := &syncWriter{w: stderrBuffer}
						k.Stderr = stderrWriter

						done := make(chan error)
						k.Exit = func(code int) {
							if code == 0 {
								done <- nil
							} else {
								done <- fmt.Errorf("exit status %d", code)
							}
							runtime.Goexit()
						}
						go func() {
							ctx := state.Context()
							kc, err := k.Parse(args)
							ctx = context.WithValue(ctx, loggerContextKey{}, newLogger(stderrWriter, c.Config.Debug))
							switch {
							case bool(c.VersionFlag):
								err = c.Version.Run(ctx, k)
							case err == nil:
								kc.BindTo(ctx, (*context.Context)(nil))
								err = kc.Run()
							}

							if err != nil {
								log.Errorf(ctx, "%v", err)
								done <- errors.New("exit status 1")
							} else {
								done <- nil
							}
						}()

						err = <-done
						return stdoutBuffer.String(), stderrBuffer.String(), err
					}, nil
				},
			)

			state, err := script.NewState(ctx, workDir, initialEnv)
			if err != nil {
				t.Fatal(err)
			}
			scripttest.Run(t, engine, state, filename, bytes.NewReader(archive.Comment))
		})
	}
}

func addSystemConds(dst map[string]script.Cond, sys system.System) {
	dst["x86_64"] = script.BoolCondition("architecture is 64-bit Intel", sys.Arch.IsX86() && sys.Arch.Is64Bit())
	dst["aarch64"] = script.BoolCondition("architecture is 64-bit ARM", sys.Arch.IsARM() && sys.Arch.Is64Bit())
	dst["linux"] = script.BoolCondition("operating system is Linux", sys.OS.IsLinux())
	dst["macos"] = script.BoolCondition("operating system is macOS", sys.OS.IsMacOS())
	dst["windows"] = script.BoolCondition("operating system is Windows", sys.OS.IsWindows())
}

func readCommand() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "read one line from the stdout buffer and assign to names",
			Args:    "name...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			firstLine, _, _ := strings.Cut(state.Stdout(), "\n")
			for _, name := range args[:len(args)-1] {
				firstLine = strings.TrimLeft(firstLine, " \t")
				end := strings.IndexAny(firstLine, " \t")
				if end < 0 {
					end = len(firstLine)
				}
				state.Setenv(name, firstLine[:end])
			}
			state.Setenv(args[len(args)-1], firstLine)
			return nil, nil
		},
	)
}

func startServerForTest(ctx context.Context, tb testing.TB, storeSocket string, server *backend.Server) {
	tb.Helper()

	l, err := net.Listen("unix", storeSocket)
	if err != nil {
		tb.Fatal(err)
	}
	grp, ctx := errgroup.WithContext(ctx)
	tb.Cleanup(func() {
		if err := l.Close(); err != nil {
			tb.Error("listener.Close:", err)
		}
		grp.Wait()
	})

	grp.Go(func() error {
		for {
			conn, err := l.Accept()
			if err != nil {
				return err
			}

			grp.Go(func() error {
				var serverImporter struct {
					*backend.Server
					zbstorerpc.Importer
				}
				serverImporter.Server = server
				serverImporter.Importer = &zbstore.BufferedImporter{
					ObjectWriter: server,
					HashType:     nix.SHA256,
				}
				zbstorerpc.Serve(ctx, conn, serverImporter)
				return nil
			})
		}
	})
}

// makeSocketPath creates a path for a socket.
//
// It intentionally does not call [*testing.T.TempDir]
// because Unix domain socket path names have very short limits.
// See [unix(7)] for Linux or [unix(4)] for FreeBSD/Darwin.
//
// [unix(7)]: https://manpages.debian.org/trixie/manpages/unix.7.en.html
// [unix(4)]: https://man.freebsd.org/cgi/man.cgi?query=unix&apropos=0&sektion=4&manpath=FreeBSD+15.1-RELEASE+and+Ports.quarterly&format=html
func makeSocketPath(tb testing.TB) string {
	dir, err := os.MkdirTemp("", "*")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			tb.Error("Clean up socket:", err)
		}
	})
	return filepath.Join(dir, "sock")
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

type loggerContextKey struct{}

type mainTestLogger struct {
}

func (l mainTestLogger) Log(ctx context.Context, e log.Entry) {
	minLogLevel := log.Info
	if testing.Verbose() {
		minLogLevel = log.Debug
	}
	var logger log.Logger = &log.LevelFilter{
		Min:    minLogLevel,
		Output: testlog.Logger{},
	}
	if v := ctx.Value(loggerContextKey{}); v != nil {
		logger = v.(log.Logger)
	}
	logger.Log(ctx, e)
}

func (l mainTestLogger) LogEnabled(e log.Entry) bool {
	return true
}

func TestMain(m *testing.M) {
	log.SetDefault(mainTestLogger{})
	os.Exit(m.Run())
}
