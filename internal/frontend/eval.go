// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

//go:generate go run ../../cmd/zb-luac --source =(prelude) -o prelude.luac prelude.lua

package frontend

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

const stdlibRegistryKey = "zb.256lights.llc/pkg/internal/frontend stdlib"

//go:embed prelude.luac
var preludeSource []byte

// An HTTPClient is an HTTP client that handles redirects, caching, and authentication.
// Clients are safe for concurrent use by multiple goroutines.
//
// Do sends an HTTP request and returns an HTTP response,
// following policy as configured on the client.
//
// Do returns an error if caused by client policy or failure to speak HTTP.
// On error, any [*http.Response] can be ignored.
// A non-2xx status code doesn't cause an error.
// If the returned error is nil, the [*http.Response] will contain a non-nil Body
// which the user is expected to close.
// Any returned error should be of type [*url.Error].
//
// The [*http.Request] Body, if non-nil, will be closed by the Client, even on errors.
// The Body may be closed asynchronously after Do returns.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options is the set of parameters for [NewEval].
type Options struct {
	// Store is an open JSON-RPC client to the store server.
	Store Store
	// StoreDirectory is the store directory used by the store.
	StoreDirectory zbstore.Directory
	// CacheDBPath is the path to a database file used to speed up store imports.
	// If empty, an in-memory cache will be used.
	CacheDBPath string
	// LookupEnv is called for the Lua os.getenv function.
	// If nil, os.getenv will always return nil.
	LookupEnv func(ctx context.Context, key string) (string, bool)
	// HTTPClient is used for making web requests.
	// If nil, [http.DefaultClient] will be used.
	HTTPClient HTTPClient
	// DownloadBufferCreator is used to create buffers for unbounded downloads.
	// If nil, then in-memory byte slices are used with reasonable limits.
	DownloadBufferCreator bytebuffer.Creator
	// ExportBufferCreator is used to buffer exports
	// to stores that do not implement [zbstore.Importer].
	// If nil, then in-memory byte slices are used with reasonable limits.
	ExportBufferCreator bytebuffer.Creator
}

// Store is the set of store operations that [Eval] needs.
//
// TODO(#44): Embed [zbstore.WritableRandomAccessStore].
type Store interface {
	zbstore.Store
	zbstore.ObjectWriter

	// FetchObjects gets information about existing objects
	// and/or attempts to download objects from another source.
	FetchObjects(ctx context.Context, paths []zbstore.Path) (map[zbstore.Path]*zbstorerpc.ObjectInfo, error)

	// Realize starts a build for the given derivation paths,
	// waits for the build to finish,
	// then returns the results of the build.
	Realize(ctx context.Context, want sets.Set[zbstore.OutputReference]) ([]*zbstorerpc.BuildResult, error)
}

type Eval struct {
	store        Store
	storeDir     zbstore.Directory
	cachePool    *sqlitemigration.Pool
	lookupEnv    func(ctx context.Context, key string) (string, bool)
	httpClient   HTTPClient
	downloadTemp bytebuffer.Creator
	exportTemp   bytebuffer.Creator

	baseImportContext context.Context
	cancelImports     context.CancelFunc
	importGroup       sync.WaitGroup

	zygoteMutex sync.Mutex
	// zygote is a Lua state that populates its registry in [*Eval.initZygote].
	// New states are created by copying the registry into their own tables.
	zygote lua.State

	loadedMutex sync.Mutex
	// loadedState is a Lua state that has a table at the top of all the modules.
	loadedState lua.State
}

func NewEval(opts *Options) (_ *Eval, err error) {
	eval := &Eval{
		store:        opts.Store,
		storeDir:     opts.StoreDirectory,
		lookupEnv:    opts.LookupEnv,
		httpClient:   opts.HTTPClient,
		downloadTemp: opts.DownloadBufferCreator,
		exportTemp:   opts.ExportBufferCreator,
	}
	if eval.lookupEnv == nil {
		eval.lookupEnv = func(ctx context.Context, key string) (string, bool) {
			return "", false
		}
	}
	if eval.httpClient == nil {
		eval.httpClient = http.DefaultClient
	}
	if eval.downloadTemp == nil {
		eval.downloadTemp = bytebuffer.BufferCreator{}
	}
	if eval.exportTemp == nil {
		eval.exportTemp = bytebuffer.BufferCreator{}
	}

	var schema sqlitemigration.Schema
	for i := 1; ; i++ {
		migration, err := fs.ReadFile(sqlFiles(), fmt.Sprintf("schema/%02d.sql", i))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("zb: new eval: read migrations: %v", err)
		}
		schema.Migrations = append(schema.Migrations, string(migration))
	}
	if opts.CacheDBPath == "" {
		// Because we are limiting the pool size to 1,
		// this acts like a mutex on a single in-memory connection.
		// The SQLite library returns an error if we use ":memory:",
		// so we pick a different string and pass the [sqlite.OpenMemory] flag.
		eval.cachePool = sqlitemigration.NewPool("mycache", schema, sqlitemigration.Options{
			Flags:       sqlite.OpenReadWrite | sqlite.OpenMemory,
			PoolSize:    1,
			PrepareConn: prepareCache,
		})
	} else {
		if err := os.MkdirAll(filepath.Dir(opts.CacheDBPath), 0o777); err != nil {
			return nil, fmt.Errorf("zb: new eval: %v", err)
		}
		eval.cachePool = sqlitemigration.NewPool(opts.CacheDBPath, schema, sqlitemigration.Options{
			Flags:       sqlite.OpenCreate | sqlite.OpenReadWrite,
			PoolSize:    1,
			PrepareConn: prepareCache,
		})
	}

	if err := eval.initZygote(); err != nil {
		return nil, fmt.Errorf("zb: new eval: %v", err)
	}
	if err := eval.initState(&eval.loadedState); err != nil {
		return nil, fmt.Errorf("zb: new eval: %v", err)
	}
	eval.loadedState.CreateTable(0, 0)
	eval.baseImportContext, eval.cancelImports = context.WithCancel(context.Background())
	return eval, nil
}

func (eval *Eval) initZygote() error {
	ctx := context.Background()
	l := &eval.zygote

	if err := registerDerivationMetatable(ctx, l); err != nil {
		return err
	}
	if err := registerLazyMetatable(ctx, l); err != nil {
		return err
	}
	if err := registerModuleMetatable(ctx, l); err != nil {
		return err
	}

	base := lua.NewOpenBase(&lua.BaseOptions{
		Output: io.Discard,
	})
	if err := lua.Require(ctx, l, lua.GName, true, base); err != nil {
		return err
	}

	// Set other built-ins.
	extraBaseFunctions := map[string]lua.Function{
		"await":         awaitFunction,
		"defaultOutput": defaultOutputFunction,
		"derivation":    eval.derivationFunction,
		"import":        eval.importFunction,
		"lazy":          lazyFunction,
		"toFile":        eval.toFileFunction,
		"outputs":       outputsFunction,
		"path":          eval.pathFunction,
		"readFile":      eval.readFileFunction,
		"storePath":     eval.storePathFunction,
	}
	if err := lua.SetPureFunctions(ctx, l, 0, extraBaseFunctions); err != nil {
		return err
	}
	l.PushString(string(eval.storeDir))
	if err := l.SetField(ctx, -2, "storeDir"); err != nil {
		return err
	}

	// Wrap load function.
	if tp := l.RawField(-1, "load"); tp != lua.TypeFunction {
		return fmt.Errorf("load is not a function")
	}
	l.PushPureFunction(1, loadFunction)
	if err := l.RawSetField(-2, "load"); err != nil {
		return err
	}

	// Remove unwanted base functions.
	if err := clearFields(l, "print", "loadfile", "dofile", "collectgarbage"); err != nil {
		return err
	}

	// Pop base library.
	l.Pop(1)

	// Load other standard libraries.
	if err := lua.Require(ctx, l, lua.MathLibraryName, true, lua.NewOpenMath(nil)); err != nil {
		return err
	}
	if err := clearFields(l, "random", "randomseed"); err != nil {
		return err
	}
	l.Pop(1)
	if err := lua.Require(ctx, l, lua.StringLibraryName, true, lua.OpenString); err != nil {
		return err
	}
	if err := clearFields(l, "dump"); err != nil {
		return err
	}
	l.Pop(1)
	if err := lua.Require(ctx, l, lua.TableLibraryName, true, lua.OpenTable); err != nil {
		return err
	}
	l.Pop(1)
	if err := lua.Require(ctx, l, lua.UTF8LibraryName, true, lua.OpenUTF8); err != nil {
		return err
	}
	l.Pop(1)
	if err := lua.Require(ctx, l, lua.OSLibraryName, true, eval.openOS); err != nil {
		return err
	}
	l.Pop(1)

	// Run prelude.
	if err := l.Load(bytes.NewReader(preludeSource), lua.UnknownSource, "b"); err != nil {
		return err
	}
	if err := l.Call(ctx, 0, 0); err != nil {
		return err
	}

	// Copy globals to stdlib key.
	l.RawIndex(lua.RegistryIndex, lua.RegistryIndexGlobals)
	if err := l.RawSetField(lua.RegistryIndex, stdlibRegistryKey); err != nil {
		return err
	}

	// Freeze everything in registry and metatables.
	if err := l.Freeze(lua.RegistryIndex); err != nil {
		return err
	}
	var err error
	forEachSharedMetatableType(l, func() bool {
		if !l.Metatable(-1) {
			l.Pop(1)
			return true
		}
		err = l.Freeze(-1)
		l.Pop(2)
		return err == nil
	})
	if err != nil {
		return err
	}

	return nil
}

func clearFields(l *lua.State, fieldNames ...string) error {
	for _, k := range fieldNames {
		l.PushNil()
		if err := l.RawSetField(-2, k); err != nil {
			return err
		}
	}
	return nil
}

func (eval *Eval) openOS(ctx context.Context, l *lua.State) (int, error) {
	lua.NewPureLib(l, map[string]lua.Function{
		"getenv": func(ctx context.Context, l *lua.State) (int, error) {
			key, err := lua.CheckString(l, 1)
			if err != nil {
				return 0, err
			}
			if val, ok := eval.lookupEnv(ctx, key); ok {
				l.PushString(val)
			} else {
				l.PushNil()
			}
			return 1, nil
		},
	})
	return 1, nil
}

func (eval *Eval) storePathFunction(ctx context.Context, l *lua.State) (int, error) {
	rawPath, err := lua.CheckString(l, 1)
	if err != nil {
		return 0, err
	}
	path, _, err := eval.storeDir.ParsePath(rawPath)
	if err != nil {
		return 0, fmt.Errorf("%sstorePath: path %s is not under %s",
			lua.Where(l, 1), lualex.Quote(rawPath), lualex.Quote(string(eval.storeDir)))
	}
	objects, err := eval.store.FetchObjects(ctx, []zbstore.Path{path})
	if err != nil {
		return 0, fmt.Errorf("%sstorePath: %v", lua.Where(l, 1), err)
	}
	if objects[path] == nil {
		return 0, fmt.Errorf("%sstorePath: %s does not exist", lua.Where(l, 1), path)
	}
	l.PushStringContext(rawPath, sets.New(contextValue{path: path}.String()))
	return 1, nil
}

func (eval *Eval) newState() (*lua.State, error) {
	l := new(lua.State)
	if err := eval.initState(l); err != nil {
		return nil, err
	}
	return l, nil
}

func (eval *Eval) initState(l *lua.State) error {
	eval.zygoteMutex.Lock()
	eval.zygote.PushValue(lua.RegistryIndex)
	forEachSharedMetatableType(&eval.zygote, func() bool {
		if !eval.zygote.Metatable(-1) {
			eval.zygote.PushNil()
		}
		eval.zygote.Remove(-2)
		return true
	})
	err := l.XMove(&eval.zygote, eval.zygote.Top())
	eval.zygoteMutex.Unlock()
	if err != nil {
		return err
	}

	// Copy over metatables.
	const tableIndex = 1
	const firstMetatableIndex = 2
	forEachSharedMetatableType(l, func() bool {
		l.Rotate(firstMetatableIndex, -1) // Move next metatable to the top.
		err = l.SetMetatable(-2)
		l.Pop(1) // Pop off dummy value.
		return err == nil
	})
	if err != nil {
		return err
	}

	// Copy over entries into registry.
	l.PushNil()
	for l.Next(tableIndex) {
		if l.IsInteger(-2) {
			if i, _ := l.ToInteger(-2); i == lua.RegistryIndexGlobals {
				l.Pop(1)
				continue
			}
		}
		l.PushValue(-2)
		l.Insert(-2)

		if err := l.RawSet(lua.RegistryIndex); err != nil {
			return err
		}
	}
	l.Pop(1) // Pop the zygote registry.

	// Set up globals metatable.
	// Any unknown names will be looked up in the standard library registry key.
	// We don't set it to the standard library table directly
	// because if this value gets moved to a different state,
	// we want to respect the state's registry key.
	l.RawIndex(lua.RegistryIndex, lua.RegistryIndexGlobals)
	lua.NewPureLib(l, map[string]lua.Function{
		"__metatable": nil,
		"__index": func(ctx context.Context, l *lua.State) (int, error) {
			if l.Type(2) == lua.TypeString {
				if s, _ := l.ToString(2); s == lua.GName {
					l.SetTop(1)
					return 1, nil
				}
			}
			if tp, err := l.Field(ctx, lua.RegistryIndex, stdlibRegistryKey); err != nil {
				return 0, err
			} else if tp == lua.TypeNil {
				// If the state does not have a standard library in the registry (bug?),
				// then return nothing.
				return 1, nil
			}
			l.Insert(2)
			l.SetTop(3)
			if _, err := l.Table(ctx, 2); err != nil {
				return 0, err
			}
			return 1, nil
		},
	})
	if err := l.SetMetatable(-2); err != nil {
		return err
	}
	l.Pop(1)

	return nil
}

func prepareCache(conn *sqlite.Conn) error {
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode=wal;", nil); err != nil {
		return fmt.Errorf("enable write-ahead logging: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA synchronous=normal;", nil); err != nil {
		return fmt.Errorf("enable write-ahead logging: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=on;", nil); err != nil {
		return fmt.Errorf("enable foreign keys: %v", err)
	}

	err := conn.CreateFunction("store_path_name", &sqlite.FunctionImpl{
		NArgs: 1,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			if args[0].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			p, err := zbstore.ParsePath(args[0].Text())
			if err != nil {
				// A non-store path has no name. Return null.
				return sqlite.Value{}, nil
			}
			return sqlite.TextValue(p.Name()), nil
		},
		Deterministic: true,
		AllowIndirect: true,
	})
	if err != nil {
		return err
	}

	if err := conn.SetCollation("PATH", collatePath); err != nil {
		return err
	}

	return nil
}

func (eval *Eval) Close() error {
	eval.cancelImports()
	eval.importGroup.Wait()
	return eval.cachePool.Close()
}

// Expression evaluates a single Lua expression
// and returns the result converted to an [OutputMap].
func (eval *Eval) Expression(ctx context.Context, expr string, sys system.System) (OutputMap, error) {
	l, err := eval.newState()
	if err != nil {
		return OutputMap{}, err
	}
	defer l.Close()

	l.PushPureFunction(0, messageHandler)
	messageHandlerIndex := l.Top()

	result := OutputMap{
		groups: []map[string]*Output{nil},
	}
	l.PushClosure(0, func(ctx context.Context, l *lua.State) (int, error) {
		l.SetTop(1)
		if err := l.Call(ctx, 0, 1); err != nil {
			return 0, err
		}
		l.PushString(SystemTriple(sys))
		var err error
		result.groups[0], err = objectOutputs(ctx, l, -2)
		if err != nil {
			return 0, err
		}
		return 0, nil
	})
	if err := loadExpression(l, expr); err != nil {
		return OutputMap{}, err
	}
	if err := l.PCall(ctx, 1, 0, messageHandlerIndex); err != nil {
		return OutputMap{}, err
	}
	return result, nil
}

func loadFile(l *lua.State, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("load file: %w", err)
	}
	// TODO(#44): Use store to open file if pathInStore(path, dir).
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("load file: %w", err)
	}
	defer f.Close()

	if err := l.Load(bufio.NewReader(f), lua.FilenameSource(path), "t"); err != nil {
		return fmt.Errorf("load file %s: %w", path, err)
	}
	return nil
}

func loadExpression(l *lua.State, expr string) error {
	if err := l.Load(strings.NewReader("return "+expr+";"), lua.LiteralSource(expr), "t"); err == nil {
		return nil
	}
	if err := l.Load(strings.NewReader(expr), lua.LiteralSource(expr), "t"); err != nil {
		return err
	}
	return nil
}

func ipairs(ctx context.Context, l *lua.State, idx int, f func(i int64) error) error {
	idx = l.AbsIndex(idx)
	top := l.Top()
	defer l.SetTop(top)
	for i := int64(1); ; i++ {
		typ, err := l.Index(ctx, idx, i)
		if err != nil {
			return fmt.Errorf("#%d: %w", i, err)
		}
		if typ == lua.TypeNil {
			return nil
		}
		err = f(i)
		l.SetTop(top)
		if err != nil {
			return err
		}
	}
}

// loadFunction is a wrapper around the load builtin function
// that forces the mode to be "t".
func loadFunction(ctx context.Context, l *lua.State) (int, error) {
	const maxLoadArgs = 4
	const modeArg = 3

	l.SetTop(maxLoadArgs)
	switch l.Type(modeArg) {
	case lua.TypeNil:
		l.PushString("t")
		if err := l.Replace(modeArg); err != nil {
			return 0, err
		}
	case lua.TypeString:
		if s, _ := l.ToString(modeArg); s != "t" {
			l.PushNil()
			l.PushString(fmt.Sprintf("load only supports text chunks (got %q)", s))
			return 2, nil
		}
	default:
		l.PushNil()
		l.PushString("only permitted mode for load is 't'")
		return 2, nil
	}
	l.PushValue(lua.UpvalueIndex(1))
	l.Insert(1)
	if err := l.Call(ctx, maxLoadArgs, lua.MultipleReturns); err != nil {
		return 0, err
	}
	return l.Top(), nil
}

func messageHandler(ctx context.Context, l *lua.State) (int, error) {
	msg, ok := l.ToString(1)
	sctx := l.StringContext(1)
	if !ok {
		hasMeta, err := lua.CallMeta(ctx, l, 1, "__tostring")
		if err != nil {
			return 0, err
		}
		if hasMeta && l.Type(-1) == lua.TypeString {
			return 1, nil
		}
		msg = fmt.Sprintf("(error object is a %v value)", l.Type(1))
	}

	l.PushStringContext(lua.Traceback(l, msg, 1), sctx)
	return 1, nil
}

// forEachSharedMetatableType pushes nil, false, 0, "", and a no-op function
// onto l's stack in sequence
// and calls f after each push.
// If f returns false, forEachSharedMetatableType skips pushing the remaining values.
// This can be used to do something for each Lua type that shares [metatables]
// among all values of the type.
//
// [metatables]: https://www.lua.org/manual/5.4/manual.html#2.4
func forEachSharedMetatableType(l *lua.State, f func() bool) {
	l.PushNil()
	if !f() {
		return
	}
	l.PushBoolean(false)
	if !f() {
		return
	}
	l.PushNumber(0)
	if !f() {
		return
	}
	l.PushString("")
	if !f() {
		return
	}
	l.PushClosure(0, func(ctx context.Context, l *lua.State) (int, error) {
		return 0, nil
	})
	f()
}

//go:embed cache_sql
var rawSqlFiles embed.FS

func sqlFiles() fs.FS {
	fsys, err := fs.Sub(rawSqlFiles, "cache_sql")
	if err != nil {
		panic(err)
	}
	return fsys
}

func stripFragment(u *url.URL) *url.URL {
	if u.Fragment == "" && u.RawFragment == "" {
		return u
	}
	u2 := new(url.URL)
	*u2 = *u
	u2.Fragment = ""
	u2.RawFragment = ""
	return u2
}
