// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/txtar"
	"rsc.io/script"
	"rsc.io/script/scripttest"
	. "zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/multierror"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/internal/xiter"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log/testlog"
	"zombiezen.com/go/nix"
)

func TestFetch(t *testing.T) {
	t.Parallel()

	testDataDir := filepath.Join("testdata", filepath.FromSlash(t.Name()))
	listing, err := os.ReadDir(testDataDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range listing {
		fileName := entry.Name()
		if strings.HasPrefix(fileName, ".") {
			continue
		}
		testName, isTXTAR := strings.CutSuffix(fileName, ".txt")
		if !isTXTAR {
			continue
		}

		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			dir := zbstore.DefaultDirectory()
			realStoreDir := t.TempDir()

			fallback := new(storetest.Store)
			server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
				TempDir: t.TempDir(),
				Options: Options{
					RealStoreDirectory: realStoreDir,
					Fallback:           fallback,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			data, err := readTestData(dir, t.Name(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := data.writeTo(ctx, server, fallback); err != nil {
				t.Fatal(err)
			}
			runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
				realDirectory: realStoreDir,
				fallback:      fallback,
			})
		})
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	testDataDir := filepath.Join("testdata", filepath.FromSlash(t.Name()))
	listing, err := os.ReadDir(testDataDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range listing {
		fileName := entry.Name()
		if strings.HasPrefix(fileName, ".") {
			continue
		}
		testName, isTXTAR := strings.CutSuffix(fileName, ".txt")
		if !isTXTAR {
			continue
		}

		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			dir := zbstore.DefaultDirectory()
			realStoreDir := t.TempDir()

			server, err := backendtest.NewServer(ctx, t, dir, &backendtest.Options{
				TempDir: t.TempDir(),
				Options: Options{
					RealStoreDirectory: realStoreDir,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			data, err := readTestData(dir, t.Name(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := data.writeTo(ctx, server, nil); err != nil {
				t.Fatal(err)
			}
			runScriptTest(ctx, t, dir, server, data, &scriptTestOptions{
				realDirectory: realStoreDir,
			})
		})
	}
}

type testDataArchive struct {
	filename        string
	comment         []byte
	objects         storetest.BlobSlice
	backendObjects  sets.Set[zbstore.Path]
	fallbackObjects sets.Set[zbstore.Path]
	rewrites        map[string]zbstore.Path
}

// readTestData parses a txtar file.
// If the name does not end with ".txt", the extension is assumed.
// Paths are interpreted relative to the testdata directory.
//
// fileSubstitutions is a map of textual substitutions to make on the txtar objects
// before processing them with [storetest.TxtarObjects].
func readTestData(dir zbstore.Directory, name string, fileSubstitutions map[string]string) (*testDataArchive, error) {
	const ext = ".txt"
	if !strings.HasSuffix(name, ext) {
		name += ext
	}
	filename := filepath.Join("testdata", filepath.FromSlash(name))
	archive, err := txtar.ParseFile(filename)
	if err != nil {
		return nil, err
	}

	backendObjectNames := make(sets.Set[string], len(archive.Files))
	fallbackObjectNames := make(sets.Set[string], len(archive.Files))
	for i := range archive.Files {
		file := &archive.Files[i]
		var labels []string
		labels, file.Name = parseFileNameLabels(file.Name)
		base, _, _ := strings.Cut(file.Name, "/")
		if len(labels) == 0 {
			backendObjectNames.Add(base)
		} else {
			for _, label := range labels {
				switch label {
				case "null":
				case "backend":
					backendObjectNames.Add(base)
				case "fallback":
					fallbackObjectNames.Add(base)
				default:
					return nil, fmt.Errorf("%s: unknown label [%s] on %s", filename, label, file.Name)
				}
			}
		}
	}

	if len(fileSubstitutions) > 0 {
		replacer := newReplacer(maps.All(fileSubstitutions))
		for i := range archive.Files {
			file := &archive.Files[i]
			file.Data = []byte(replacer.Replace(string(file.Data)))
		}
	}

	txtarStore, err := storetest.TxtarObjects(dir, archive.Files)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", filename, err)
	}
	backendObjects := make(sets.Set[zbstore.Path], len(txtarStore.BlobSlice))
	for name := range backendObjectNames {
		backendObjects.Add(txtarStore.Rewrites[name])
	}
	fallbackObjects := make(sets.Set[zbstore.Path], len(txtarStore.BlobSlice))
	for name := range fallbackObjectNames {
		fallbackObjects.Add(txtarStore.Rewrites[name])
	}
	return &testDataArchive{
		filename:        filename,
		comment:         archive.Comment,
		objects:         txtarStore.BlobSlice,
		backendObjects:  backendObjects,
		fallbackObjects: fallbackObjects,
		rewrites:        txtarStore.Rewrites,
	}, nil
}

func (data *testDataArchive) writeTo(ctx context.Context, backend, fallback zbstore.ObjectWriter) error {
	for _, obj := range data.objects {
		if data.backendObjects.Has(obj.StorePath) {
			if err := backend.WriteObject(ctx, obj); err != nil {
				return err
			}
		}
	}

	if data.fallbackObjects.Len() > 0 {
		if fallback == nil {
			return fmt.Errorf("test file contains [fallback] objects, but no fallback provided")
		}
		for _, obj := range data.objects {
			if data.fallbackObjects.Has(obj.StorePath) {
				if err := fallback.WriteObject(ctx, obj); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func parseFileNameLabels(name string) ([]string, string) {
	var labels []string
	for {
		var hasLabel bool
		name, hasLabel = strings.CutPrefix(name, "[")
		if !hasLabel {
			return labels, name
		}
		var label string
		label, name, _ = strings.Cut(name, "]")
		labels = append(labels, label)
		name = strings.TrimLeft(name, " \t")
	}
}

func reverseLookup[K any, V comparable](m iter.Seq2[K, V], want V) (K, bool) {
	for k, v := range m {
		if v == want {
			return k, true
		}
	}
	var zero K
	return zero, false
}

// scriptTestOptions is the set of optional arguments to [runScripTest].
type scriptTestOptions struct {
	// realDirectory is the path to the store's actual directory.
	realDirectory string
	// fallback is the fallback store that the server is configured to read from.
	fallback interface {
		zbstore.ObjectWriter
		realizationFetchWriter
	}
	// initialEnv is a map of any extra environment variables to set in the script to start.
	initialEnv map[string]string
}

// runScriptTest runs a backend script test from a testdata file.
// See testdata/README.md for documentation.
func runScriptTest(ctx context.Context, tb testing.TB, dir zbstore.Directory, server *Server, data *testDataArchive, opts *scriptTestOptions) (env map[string]string) {
	tb.Helper()

	if opts == nil {
		opts = new(scriptTestOptions)
	}

	engine := &script.Engine{
		Cmds: map[string]script.Cmd{
			"env":    script.Env(),
			"echo":   script.Echo(),
			"stdout": script.Stdout(),
			"stderr": script.Stderr(),
			"grep":   script.Grep(),
			"wait":   script.Wait(),
			"stop":   script.Stop(),
			"skip":   scripttest.Skip(),
			"read":   readCommand(),
		},
		Conds: map[string]script.Cond{},
	}
	sc := &storeCommands{
		tb:         tb,
		directory:  dir,
		server:     server,
		allObjects: data.objects,
		rewrites:   data.rewrites,
		fallback:   opts.fallback,
	}
	sc.addTo(engine.Cmds)
	addSystemConds(engine.Conds, system.Current())

	initialEnvSlice := []string{}
	if opts != nil {
		for k, v := range opts.initialEnv {
			initialEnvSlice = append(initialEnvSlice, k+"="+v)
		}
	}
	realDirectory := opts.realDirectory
	if realDirectory == "" {
		realDirectory = string(dir)
	}
	state, err := script.NewState(ctx, realDirectory, initialEnvSlice)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Log(time.Now().UTC().Format(time.RFC3339))
	work, _ := state.LookupEnv("WORK")
	tb.Logf("$WORK=%s", work)
	scripttest.Run(tb, engine, state, data.filename, bytes.NewReader(data.comment))
	env = make(map[string]string)
	for _, kv := range state.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func newReplacer[K, V ~string](rewrites iter.Seq2[K, V]) *strings.Replacer {
	var args []string
	for k, v := range rewrites {
		args = append(args, string(k), string(v))
	}
	return strings.NewReplacer(args...)
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

type realizationFetchWriter interface {
	zbstore.RealizationFetcher

	// WriteRealizations stores [zbstore.RealizationMap] values.
	// If a Writer receives a [zbstore.Realization] identical to one it already has,
	// it should ignore the new realization and it should not return an error.
	WriteRealizations(ctx context.Context, realizations zbstore.RealizationMap) error
}

type storeCommands struct {
	tb         testing.TB
	directory  zbstore.Directory
	server     *Server
	allObjects zbstore.Store
	rewrites   map[string]zbstore.Path
	fallback   realizationFetchWriter
}

func (sc *storeCommands) addTo(cmds map[string]script.Cmd) {
	cmds["only"] = sc.only()
	cmds["realpath"] = sc.realpath()
	cmds["storepath"] = sc.storepath()
	cmds["exists"] = sc.exists()
	cmds["cmpinfo"] = sc.cmpinfo()
	cmds["realize"] = sc.realize()
	cmds["fetch"] = sc.fetch()
	cmds["write-realization"] = sc.writeRealization()
	cmds["delete"] = sc.delete()
}

func (sc *storeCommands) newStoreReplacer() *strings.Replacer {
	return newReplacer(maps.All(sc.rewrites))
}

func (sc *storeCommands) newRealReplacer() *strings.Replacer {
	replacements := make([]string, 0, len(sc.rewrites)*2)
	for fileName, path := range sc.rewrites {
		replacements = append(replacements, fileName, path.Base())
	}
	return strings.NewReplacer(replacements...)
}

func (sc *storeCommands) only() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "verify that the store contains exactly the set of objects named",
			Args:    "[path...]",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			listing, err := os.ReadDir(state.Getwd())
			if err != nil {
				return nil, err
			}
			var ec multierror.Collector
			replacer := sc.newRealReplacer()
			for _, arg := range args {
				rewritten := replacer.Replace(arg)
				i := slices.IndexFunc(listing, func(entry os.DirEntry) bool {
					return entry.Name() == rewritten
				})
				if i == -1 {
					ec.Add(fmt.Errorf("missing %s from store", arg))
				} else {
					listing = slices.Delete(listing, i, i+1)
				}
			}
			for _, entry := range listing {
				ec.Add(fmt.Errorf("unexpected object %s in store", entry.Name()))
			}
			return nil, ec.Error()
		},
	)
}

func (sc *storeCommands) exists() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "check that files exist",
			Args:    "[-readonly] [-exec] file...",
		},
		func(s *script.State, args ...string) (script.WaitFunc, error) {
			var readonly, exec bool
			for ; len(args) > 0 && strings.HasPrefix(args[0], "-"); args = args[1:] {
				if args[0] == "--" {
					args = args[1:]
					break
				}
				switch args[0] {
				case "-readonly":
					readonly = true
				case "-exec":
					exec = true
				default:
					return nil, script.ErrUsage
				}
			}
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			replacer := sc.newRealReplacer()
			for _, arg := range args {
				arg = s.Path(replacer.Replace(arg))
				info, err := os.Stat(arg)
				if err != nil {
					return nil, err
				}
				if readonly && info.Mode()&0o222 != 0 {
					return nil, fmt.Errorf("%s exists but is writable", arg)
				}
				if exec && runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
					return nil, fmt.Errorf("%s exists but is not executable", arg)
				}
			}

			return nil, nil
		})
}

func (sc *storeCommands) storepath() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "writes resolved store paths to stdout, followed by a newline",
			Args:    "path...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			sb := new(strings.Builder)
			replacer := sc.newStoreReplacer()
			for i, arg := range args {
				if i > 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(replacer.Replace(arg))
			}
			sb.WriteString("\n")
			out := sb.String()
			return func(state *script.State) (stdout string, stderr string, err error) {
				return out, "", nil
			}, nil
		},
	)
}

func (sc *storeCommands) realpath() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "writes filesystem paths to stdout, followed by a newline",
			Args:    "path...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			sb := new(strings.Builder)
			replacer := sc.newRealReplacer()
			for i, arg := range args {
				if i > 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(replacer.Replace(arg))
			}
			sb.WriteString("\n")
			out := sb.String()
			return func(state *script.State) (stdout string, stderr string, err error) {
				return out, "", nil
			}, nil
		},
	)
}

func (sc *storeCommands) cmpinfo() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "verify that info from store matches info from test",
			Args:    "path...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			ctx := state.Context()
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			replacer := sc.newStoreReplacer()
			var ec multierror.Collector
			for _, arg := range args {
				rewritten := replacer.Replace(arg)
				path, subpath, err := sc.directory.ParsePath(rewritten)
				if err != nil {
					ec.Add(err)
					continue
				}
				if subpath != "" {
					ec.Add(fmt.Errorf("cannot use subpath in %s", arg))
					continue
				}
				want, err := sc.allObjects.Object(ctx, path)
				if err != nil {
					ec.Add(err)
					continue
				}
				var info zbstorerpc.InfoResponse
				err = jsonrpc.Do(ctx, sc.server, zbstorerpc.InfoMethod, &info, &zbstorerpc.InfoRequest{
					Path: path,
				})
				if err != nil {
					ec.Add(err)
					continue
				}
				if diff := diffObjectInfo(ctx, want, info.Info); diff != "" {
					ec.Add(fmt.Errorf("%s info (-want +got):\n%s", path, diff))
				}
			}
			return nil, ec.Error()
		},
	)
}

func (sc *storeCommands) realize() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "realize one or more derivations in the store",
			Args:    "[--clean] drvPath...",
			Async:   true,
		},
		sc.runRealize,
	)
}

func (sc *storeCommands) runRealize(state *script.State, args ...string) (script.WaitFunc, error) {
	ctx := state.Context()

	clean := false
	for ; len(args) > 0 && strings.HasPrefix(args[0], "-"); args = args[1:] {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		switch args[0] {
		case "--clean":
			clean = true
		default:
			return nil, script.ErrUsage
		}
	}
	if len(args) == 0 {
		return nil, script.ErrUsage
	}
	drvPaths := make([]zbstore.Path, 0, len(args))
	replacer := sc.newStoreReplacer()
	for _, arg := range args {
		rewritten := replacer.Replace(arg)
		drvPath, subpath, err := sc.directory.ParsePath(rewritten)
		if err != nil {
			return nil, err
		}
		if subpath != "" {
			return nil, fmt.Errorf("cannot use subpath in %s", arg)
		}
		drvPaths = append(drvPaths, drvPath)
	}

	realizeResponse := new(zbstorerpc.RealizeResponse)
	err := jsonrpc.Do(ctx, sc.server, zbstorerpc.RealizeMethod, realizeResponse, &zbstorerpc.RealizeRequest{
		DrvPaths: drvPaths,
		Reuse:    &zbstorerpc.ReusePolicy{All: !clean},
	})
	if err != nil {
		return nil, err
	}
	if realizeResponse.BuildID == "" {
		return nil, fmt.Errorf("no build ID returned")
	}

	return func(state *script.State) (stdout string, stderr string, err error) {
		got, err := backendtest.WaitForBuild(ctx, sc.server, realizeResponse.BuildID)
		if err != nil {
			return "", "", err
		}
		if buildJSON, err := jsonv2.Marshal(got); err != nil {
			sc.tb.Error("marshal build:", err)
		} else {
			state.Setenv("build", string(buildJSON))
		}
		if !got.EndedAt.Valid {
			sc.tb.Error("build.endedAt = null")
		}

		logArchive := &txtar.Archive{
			Files: make([]txtar.File, 0, len(got.Results)),
		}
		for _, result := range got.Results {
			var logFile txtar.File
			var err error
			logFile.Data, err = backendtest.ReadLog(ctx, sc.server, realizeResponse.BuildID, result.DrvPath)
			if err != nil {
				state.Logf("%v", err)
				continue
			}
			var hasRewrite bool
			logFile.Name, hasRewrite = reverseLookup(maps.All(sc.rewrites), result.DrvPath)
			if !hasRewrite {
				logFile.Name = result.DrvPath.Base()
			}
			logArchive.Files = append(logArchive.Files, logFile)
		}

		if got.Status == zbstorerpc.BuildSuccess {
			if len(drvPaths) == 1 {
				if result, err := got.ResultForPath(drvPaths[0]); err != nil {
					state.Logf("%v", err)
				} else {
					for _, output := range result.Outputs {
						if output.Path.Valid {
							state.Setenv(output.Name, string(output.Path.X))
						}
					}
				}
			}
			err = nil
		} else {
			err = fmt.Errorf("build %s failed with status %q", realizeResponse.BuildID, got.Status)
		}
		return string(txtar.Format(logArchive)), "", err
	}, nil
}

func (sc *storeCommands) writeRealization() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "write a realization to the fallback store",
			Args:    "drvPath!outputName path",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			ctx := state.Context()
			if len(args) != 2 {
				return nil, script.ErrUsage
			}
			if sc.fallback == nil {
				return nil, fmt.Errorf("fallback store not set")
			}
			replacer := sc.newStoreReplacer()
			ref, err := zbstore.ParseOutputReference(replacer.Replace(args[0]))
			if err != nil {
				return nil, err
			}
			outputPath, err := zbstore.ParsePath(replacer.Replace(args[1]))
			if err != nil {
				return nil, err
			}
			drvHash, derivers, err := hashDerivationFromFetcher(ctx, sc.allObjects, sc.fallback, ref.DrvPath)
			if err != nil {
				return nil, fmt.Errorf("write realization %v → %s: %v", ref, outputPath, err)
			}
			realizationRef := zbstore.RealizationOutputReference{DerivationHash: drvHash, OutputName: ref.OutputName}
			realization := &zbstore.Realization{OutputPath: outputPath}
			if outputObject, err := sc.allObjects.Object(ctx, outputPath); err != nil && !errors.Is(err, zbstore.ErrNotFound) {
				return nil, fmt.Errorf("write realization %v → %s: %v", ref, outputPath, err)
			} else if err == nil {
				for ref := range outputObject.Info().References.Values() {
					if d := derivers[ref]; len(d) == 0 {
						realization.ReferenceClasses = append(realization.ReferenceClasses, &zbstore.ReferenceClass{Path: ref})
					} else {
						for _, realizationRef := range d {
							realization.ReferenceClasses = append(realization.ReferenceClasses, &zbstore.ReferenceClass{Path: ref, Realization: realizationRef})
						}
					}
				}
			}
			err = sc.fallback.WriteRealizations(ctx, zbstore.RealizationMap{
				DerivationHash: drvHash,
				Realizations: map[string][]*zbstore.Realization{
					ref.OutputName: []*zbstore.Realization{realization},
				},
			})
			if err != nil {
				return nil, err
			}
			state.Logf("Wrote realization %v → %s to fallback", realizationRef, outputPath)
			return nil, nil
		},
	)
}

func (sc *storeCommands) fetch() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "fetch one or more store objects from fallback",
			Args:    "path...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			ctx := state.Context()
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			paths := make([]zbstore.Path, 0, len(args))
			replacer := sc.newStoreReplacer()
			for _, arg := range args {
				rewritten := replacer.Replace(arg)
				path, subpath, err := sc.directory.ParsePath(rewritten)
				if err != nil {
					return nil, err
				}
				if subpath != "" {
					return nil, fmt.Errorf("cannot use subpath in %s", arg)
				}
				paths = append(paths, path)
			}

			response := new(zbstorerpc.FetchResponse)
			err := jsonrpc.Do(ctx, sc.server, zbstorerpc.FetchMethod, response, &zbstorerpc.FetchRequest{
				Paths: paths,
			})
			if err != nil {
				return nil, err
			}
			for path, got := range response.Found {
				if !slices.Contains(paths, path) {
					sc.tb.Errorf("fetch response contains unrequested path %s", path)
					continue
				}
				want, err := sc.allObjects.Object(ctx, path)
				if err != nil {
					if errors.Is(err, zbstore.ErrNotFound) {
						sc.tb.Errorf("fetch response contains unknown object %s", path)
					}
					return nil, err
				}
				if diff := diffObjectInfo(ctx, want, got); diff != "" {
					sc.tb.Errorf("%s info (-want +got):\n%s", path, diff)
				}
			}

			var unreceivedPaths []zbstore.Path
			for _, path := range paths {
				if response.Found[path] == nil {
					unreceivedPaths = append(unreceivedPaths, path)
				}
			}
			if len(unreceivedPaths) > 0 {
				sb := new(strings.Builder)
				sb.WriteString("fetch did not retrieve ")
				for i, path := range unreceivedPaths {
					if i > 0 {
						sb.WriteString(", ")
					}
					sb.WriteString(string(path))
				}
				return nil, errors.New(sb.String())
			}

			return nil, nil
		},
	)
}

func (sc *storeCommands) delete() script.Cmd {
	return script.Command(
		script.CmdUsage{
			Summary: "delete one or more store objects",
			Args:    "[-r] path...",
		},
		func(state *script.State, args ...string) (script.WaitFunc, error) {
			ctx := state.Context()
			recursive := false
			for ; len(args) > 0 && strings.HasPrefix(args[0], "-"); args = args[1:] {
				if args[0] == "--" {
					args = args[1:]
					break
				}
				switch args[0] {
				case "-r":
					recursive = true
				default:
					return nil, script.ErrUsage
				}
			}
			if len(args) == 0 {
				return nil, script.ErrUsage
			}
			paths := make(sets.Set[zbstore.Path], len(args))
			replacer := sc.newStoreReplacer()
			for _, arg := range args {
				rewritten := replacer.Replace(arg)
				path, subpath, err := sc.directory.ParsePath(rewritten)
				if err != nil {
					return nil, err
				}
				if subpath != "" {
					return nil, fmt.Errorf("cannot use subpath in %s", arg)
				}
				paths.Add(path)
			}
			f := sc.server.Delete
			if recursive {
				f = sc.server.DeleteIncludingReferences
			}
			if err := f(ctx, paths); err != nil {
				return nil, err
			}
			return nil, nil
		},
	)
}

// hashDerivationFromFetcher hashes the derivation at the given path
// by reading realizations from a [zbstore.RealizationFetcher].
// If the fetcher does not return exactly one realization for each transitive derivation,
// then hashDerivationFromFetcher returns an error.
func hashDerivationFromFetcher(ctx context.Context, drvStore zbstore.Store, fetcher zbstore.RealizationFetcher, drvPath zbstore.Path) (drvHash nix.Hash, derivers map[zbstore.Path][]zbstore.RealizationOutputReference, err error) {
	drvHashes := make(map[zbstore.Path]nix.Hash)
	derivers = make(map[zbstore.Path][]zbstore.RealizationOutputReference)
	var f func(zbstore.OutputReference) (zbstore.Path, error)
	f = func(ref zbstore.OutputReference) (zbstore.Path, error) {
		drvHash := drvHashes[ref.DrvPath]
		if drvHash.IsZero() {
			drvObject, err := drvStore.Object(ctx, ref.DrvPath)
			if err != nil {
				return "", fmt.Errorf("realization for %v: %v", ref, err)
			}
			drv, err := zbstore.ParseDerivationObject(ctx, drvObject)
			if err != nil {
				return "", fmt.Errorf("realization for %v: %v", ref, err)
			}
			drvHash, err = drv.SHA256RealizationHash(f)
			if err != nil {
				return "", fmt.Errorf("realization for %v: %v", ref, err)
			}
			drvHashes[ref.DrvPath] = drvHash
		}

		realizations, err := fetcher.FetchRealizations(ctx, drvHash)
		if err != nil {
			return "", fmt.Errorf("realization for %v: %v", ref, err)
		}
		r, err := xiter.Single(slices.Values(realizations.Realizations[ref.OutputName]))
		if err != nil {
			return "", fmt.Errorf("realization for %v: %v", ref, err)
		}
		derivers[r.OutputPath] = append(derivers[r.OutputPath], zbstore.RealizationOutputReference{
			DerivationHash: drvHash,
			OutputName:     ref.OutputName,
		})
		return r.OutputPath, nil
	}

	drvObject, err := drvStore.Object(ctx, drvPath)
	if err != nil {
		return nix.Hash{}, nil, err
	}
	drv, err := zbstore.ParseDerivationObject(ctx, drvObject)
	if err != nil {
		return nix.Hash{}, nil, err
	}
	drvHash, err = drv.SHA256RealizationHash(f)
	if err != nil {
		return nix.Hash{}, nil, err
	}
	return drvHash, derivers, nil
}

// diffObjectInfo compares an object with its [*zbstorerpc.ObjectInfo].
// It returns an empty string if and only if the information is equivalent.
func diffObjectInfo(ctx context.Context, want zbstore.Object, got *zbstorerpc.ObjectInfo) string {
	var ht nix.HashType
	if got != nil {
		ht = got.NARHash.Type()
	}
	if ht == 0 {
		ht = nix.SHA256
	}
	h := nix.NewHasher(ht)
	if err := want.WriteNAR(ctx, h); err != nil {
		return "WriteNAR error: " + err.Error()
	}
	wantInfo := zbstorerpc.NewObjectInfo(want.Info())
	wantInfo.NARHash = h.SumHash()
	return cmp.Diff(wantInfo, got)
}

func TestMain(m *testing.M) {
	testlog.Main(nil)
	os.Exit(m.Run())
}
