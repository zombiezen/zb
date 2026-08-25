// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

// Package backend provides a [zbstorerpc] implementation backed by local compute resources.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/google/uuid"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/multierror"
	"zb.256lights.llc/pkg/internal/xiter"
	"zb.256lights.llc/pkg/internal/xslices"
	"zb.256lights.llc/pkg/internal/xtime"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
	"zombiezen.com/go/xcontext"
)

// DefaultBuildUsersGroup is the conventional name of the Unix group
// for the users that execute builders on behalf of the daemon.
const DefaultBuildUsersGroup = "zbld"

const heartRate = 5 * time.Second

// Store combines the [zbstore.Store] and [zbstore.RealizationFetcher] interfaces.
type Store interface {
	zbstore.Store
	zbstore.RealizationFetcher
}

// Options is the set of optional parameters to [NewServer].
type Options struct {
	// RealStoreDirectory is where the store objects are located physically on disk.
	// If empty, defaults to the store directory.
	RealStoreDirectory string
	// BuildDirectory is where realizations' working directories will be placed.
	// If empty, defaults to [os.TempDir].
	BuildDirectory string
	// LogDirectory is where builder logs will be stored.
	// If empty, defaults to a directory called "log" in the same directory as the database.
	LogDirectory string
	// ContentAddressBufferCreator is used to create buffers for content addressing analysis.
	// If nil, then in-memory byte slices are used with reasonable limits.
	ContentAddressBufferCreator bytebuffer.Creator

	// Fallback is used to obtain objects and realizations
	// that don't exist in the server's store directory.
	Fallback Store

	// If Upload is not nil, then after a successful builder program run,
	// the server will upload the object and realizations.
	Upload *zbstorehttp.Store

	// DatabasePoolSize is the maximum permitted number of concurrent connections to the database.
	// If less than 1, a reasonable default is used.
	DatabasePoolSize int

	// If AllowKeepFailed is true, then the KeepFailed field in [zbstore.RealizeRequest] will be respected.
	AllowKeepFailed bool

	// If DisableSandbox is true, then builders are always run without the sandbox.
	// Otherwise, sandboxing is used whenever possible.
	DisableSandbox bool
	// SandboxPaths is a map of paths inside the sandbox
	// to paths on the host machine.
	// These paths will be made available to sandboxed builders.
	SandboxPaths map[string]SandboxPath

	// CoresPerBuild is a hint from the user to builders
	// on the number of concurrent jobs to perform.
	// If non-positive, then the number of cores detected on the machine is used.
	CoresPerBuild int

	// BuildUsers is the set of user IDs to use for builds on non-Windows systems.
	// If empty, then builds will use the current process's privileges.
	// [NewServer] will panic if multiple entries have the same user ID.
	BuildUsers []BuildUser

	// BuildContext optionally specifies a function that detaches the context for a build.
	// If BuildContext is nil, the default is [context.Background].
	BuildContext func(parent context.Context, buildID string) context.Context

	// BuildLogRetention is the length of time to retain build logs.
	// If non-positive, then build logs will be not be automatically deleted.
	BuildLogRetention time.Duration

	// Keyring is a set of keys that will be used to sign realizations
	// that this server realizes.
	Keyring *Keyring
}

// A SandboxPath is the set of options for SandboxPaths in [Options].
type SandboxPath struct {
	// Path is the path on the backend's filesystem to make available at the path.
	// If empty, the SandboxPaths key is used.
	Path string
	// If AlwaysPresent is true, then the path will always be made available in the sandbox.
	// The default is to only allow the path to be used if it is declared in __buildSystemDeps.
	AlwaysPresent bool
}

// BuildUser is a descriptor for a Unix user.
type BuildUser struct {
	// UID is the user ID.
	UID int
	// GID is the user's primary group ID.
	GID int
}

func (user BuildUser) String() string {
	return fmt.Sprintf("%d:%d", user.UID, user.GID)
}

// SystemSupportsSandbox reports whether the host operating system supports sandboxing.
func SystemSupportsSandbox() bool {
	return runtime.GOOS == "linux"
}

// CanSandbox reports whether the current execution environment supports sandboxing.
func CanSandbox() bool {
	return SystemSupportsSandbox() && os.Geteuid() == 0
}

// Server is a local store.
// Server implements [jsonrpc.Handler] and is intended to be used with [jsonrpc.Serve].
// A Server is safe to use from multiple goroutines simultaneously.
type Server struct {
	dir             zbstore.Directory
	realDir         string
	buildDir        string
	logDir          string
	caCreateTemp    bytebuffer.Creator
	db              *sqlitemigration.Pool
	allowKeepFailed bool
	buildContext    func(context.Context, string) context.Context
	keyring         *Keyring
	fallback        Store
	upload          *zbstorehttp.Store

	sandbox      bool
	sandboxPaths map[string]SandboxPath

	backgroundContext context.Context
	cancelBackground  context.CancelFunc
	background        sync.WaitGroup

	coresPerBuild int

	writing  mutexMap[zbstore.Path] // store objects being written
	building mutexMap[zbstore.Path] // derivations being built
	users    *userSet

	// activeBuildsMu protects activeBuilds, activeWork, and drained.
	activeBuildsMu sync.Mutex
	// activeBuilds maps build IDs to their cancel functions.
	activeBuilds map[uuid.UUID]context.CancelFunc
	// activeWork counts the active non-build background tasks.
	activeWork int
	// drained is non-nil if a work drain has been requested,
	// and closed if the drain has completed.
	drained chan struct{}

	// launchCheckDone is closed after launchCheckError is set.
	launchCheckDone chan struct{}
	// launchCheckError is the error encountered when checking if there's another server operating on the database.
	// launchCheckError is nil if this server has been confirmed to be the only running server.
	launchCheckError error
}

// NewServer returns a new [Server] for the given store directory and database path.
// Callers are responsible for calling [Server.Close] on the returned server.
func NewServer(dir zbstore.Directory, dbPath string, opts *Options) *Server {
	if opts == nil {
		opts = new(Options)
	}
	users, err := newUserSet(opts.BuildUsers)
	if err != nil {
		panic(err)
	}
	srv := &Server{
		dir:             dir,
		realDir:         opts.RealStoreDirectory,
		buildDir:        opts.BuildDirectory,
		logDir:          opts.LogDirectory,
		caCreateTemp:    opts.ContentAddressBufferCreator,
		allowKeepFailed: opts.AllowKeepFailed,
		sandbox:         !opts.DisableSandbox && CanSandbox(),
		sandboxPaths:    maps.Clone(opts.SandboxPaths),
		coresPerBuild:   opts.CoresPerBuild,
		users:           users,
		activeBuilds:    make(map[uuid.UUID]context.CancelFunc),
		buildContext:    opts.BuildContext,
		keyring:         opts.Keyring.Clone(),
		fallback:        opts.Fallback,
		upload:          opts.Upload,

		db: sqlitemigration.NewPool(dbPath, loadSchema(), sqlitemigration.Options{
			Flags:       sqlite.OpenCreate | sqlite.OpenReadWrite,
			PrepareConn: prepareConn,
			PoolSize:    opts.DatabasePoolSize,
			OnStartMigrate: func() {
				ctx := context.Background()
				log.Debugf(ctx, "Migrating...")
			},
			OnReady: func() {
				ctx := context.Background()
				log.Debugf(ctx, "Database ready")
			},
			OnError: func(err error) {
				ctx := context.Background()
				log.Errorf(ctx, "Migration: %v", err)
			},
		}),
		launchCheckDone: make(chan struct{}),
	}
	if srv.coresPerBuild <= 0 {
		srv.coresPerBuild = max(1, runtime.NumCPU())
	}
	if srv.realDir == "" {
		srv.realDir = string(srv.dir)
	}
	if srv.buildDir == "" {
		srv.buildDir = os.TempDir()
	}
	if srv.logDir == "" {
		srv.logDir = filepath.Join(filepath.Dir(dbPath), "log")
	}
	if srv.caCreateTemp == nil {
		srv.caCreateTemp = bytebuffer.BufferCreator{}
	}
	if srv.buildContext == nil {
		srv.buildContext = func(_ context.Context, _ string) context.Context {
			return context.Background()
		}
	}
	if srv.fallback == nil {
		srv.fallback = zbstore.Null{}
	}
	srv.backgroundContext, srv.cancelBackground = context.WithCancel(context.Background())

	srv.background.Go(func() {
		srv.optimizeDatabase(srv.backgroundContext)
	})

	srv.background.Go(func() {
		srv.writeHeartbeat(srv.backgroundContext)
	})
	if opts.BuildLogRetention > 0 {
		srv.background.Go(func() {
			srv.gcLogs(srv.backgroundContext, opts.BuildLogRetention)
		})
	}
	return srv
}

// detachFromRPC calls f in its own goroutine
// with a [context.Context] whose values are taken from parent
// and its cancellation from s.backgroundContext.
// detachFromRPC returns an error if it did not start a goroutine
// because the server is currently draining.
func (s *Server) detachFromRPC(parent context.Context, f func(context.Context)) error {
	s.activeBuildsMu.Lock()
	draining := s.drained != nil
	if !draining {
		s.activeWork++
	}
	s.activeBuildsMu.Unlock()

	if draining {
		return errors.New("server shutting down; not starting new work")
	}

	s.background.Go(func() {
		defer func() {
			s.activeBuildsMu.Lock()
			s.activeWork--
			s.lockedUpdateDrained()
			s.activeBuildsMu.Unlock()
		}()

		f(splitContext{
			cancel: s.backgroundContext,
			values: parent,
		})
	})
	return nil
}

// detachFromBuild calls f in its own goroutine
// with a [context.Context] whose values are taken from parent
// and its cancellation from s.backgroundContext.
//
// detachFromBuild does not check for draining,
// since detachFromBuild will only be called during a build.
func (s *Server) detachFromBuild(parent context.Context, f func(context.Context)) {
	s.activeBuildsMu.Lock()
	s.activeWork++
	s.activeBuildsMu.Unlock()

	s.background.Go(func() {
		defer func() {
			s.activeBuildsMu.Lock()
			s.activeWork--
			s.lockedUpdateDrained()
			s.activeBuildsMu.Unlock()
		}()

		f(splitContext{
			cancel: s.backgroundContext,
			values: parent,
		})
	})
}

// lockedUpdateDrained closes s.drained if all work has completed.
// The caller must be holding onto s.activeBuildsMu.
func (s *Server) lockedUpdateDrained() {
	if s.drained != nil && s.activeWork == 0 && len(s.activeBuilds) == 0 {
		close(s.drained)
	}
}

// Drain prevents the server from starting more builds
// and blocks until all builds are complete
// or [context.Context.Done] is closed,
// whichever comes first.
func (s *Server) Drain(ctx context.Context) error {
	s.activeBuildsMu.Lock()
	drained := s.drained
	if drained == nil {
		drained = make(chan struct{})
		s.drained = drained
		s.lockedUpdateDrained()
	}
	s.activeBuildsMu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("drain server: %w", ctx.Err())
	}
}

// Close stops any ongoing builds
// and releases any resources associated with the server.
// The [*Server] cannot be used after calling Close.
func (s *Server) Close() error {
	s.activeBuildsMu.Lock()
	if s.drained == nil {
		s.drained = make(chan struct{})
		s.lockedUpdateDrained()
	}
	s.activeBuildsMu.Unlock()

	s.cancelBackground()
	s.background.Wait()

	return s.db.Close()
}

// JSONRPC implements the [jsonrpc.Handler] interface
// and serves the [zbstorerpc] API.
func (s *Server) JSONRPC(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	if err := s.LaunchCheck(ctx); err != nil {
		return nil, err
	}

	return jsonrpc.ServeMux{
		zbstorerpc.ExistsMethod:         jsonrpc.HandlerFunc(s.exists),
		zbstorerpc.InfoMethod:           jsonrpc.HandlerFunc(s.info),
		zbstorerpc.ExportMethod:         jsonrpc.HandlerFunc(s.export),
		zbstorerpc.ExpandMethod:         jsonrpc.HandlerFunc(s.expand),
		zbstorerpc.RealizeMethod:        jsonrpc.HandlerFunc(s.realize),
		zbstorerpc.GetBuildMethod:       jsonrpc.HandlerFunc(s.getBuild),
		zbstorerpc.GetBuildResultMethod: jsonrpc.HandlerFunc(s.getBuildResult),
		zbstorerpc.CancelBuildMethod:    jsonrpc.HandlerFunc(s.cancelBuild),
		zbstorerpc.ReadLogMethod:        jsonrpc.HandlerFunc(s.readLog),
		zbstorerpc.FetchMethod:          jsonrpc.HandlerFunc(s.fetch),

		zbstorerpc.NopMethod: jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
			return &jsonrpc.Response{
				Result: jsontext.Value("null"),
			}, nil
		}),
	}.JSONRPC(ctx, req)
}

func (s *Server) realPath(path zbstore.Path) string {
	return filepath.Join(s.realDir, path.Base())
}

func (s *Server) exists(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.ExistsRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	p, sub, err := s.dir.ParsePath(args.Path)
	if err != nil {
		log.Debugf(ctx, "Queried invalid path %s", args.Path)
		return &jsonrpc.Response{
			Result: jsontext.Value("false"),
		}, nil
	}
	unlock, err := s.writing.lock(ctx, p)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(s.realPath(p), filepath.FromSlash(sub))); err != nil {
		log.Debugf(ctx, "%s does not exist (%v)", args.Path, err)
		return &jsonrpc.Response{
			Result: jsontext.Value("false"),
		}, nil
	}
	log.Debugf(ctx, "%s exists", args.Path)
	return &jsonrpc.Response{
		Result: jsontext.Value("true"),
	}, nil
}

func (s *Server) info(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.InfoRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	if args.Path.Dir() != s.dir {
		return marshalResponse(&zbstorerpc.InfoResponse{})
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	log.Debugf(ctx, "Looking up path info for %s...", args.Path)
	info, err := pathInfo(conn, args.Path)
	if err != nil {
		if errors.Is(err, zbstore.ErrNotFound) {
			return marshalResponse(&zbstorerpc.InfoResponse{})
		}
		return nil, err
	}
	return marshalResponse(&zbstorerpc.InfoResponse{
		Info: zbstorerpc.NewObjectInfo(info),
	})
}

func (s *Server) getBuild(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.GetBuildRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	buildID, ok := parseBuildID(args.BuildID)
	if !ok {
		return marshalResponse(&zbstorerpc.Build{
			ID:      args.BuildID,
			Status:  zbstorerpc.BuildUnknown,
			Results: []*zbstorerpc.BuildResult{},
		})
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	// Read active status before consulting database.
	// We write to the database before clearing the active status.
	s.activeBuildsMu.Lock()
	_, isActive := s.activeBuilds[buildID]
	s.activeBuildsMu.Unlock()

	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, fmt.Errorf("get build %v: %v", buildID, err)
	}
	defer rollback()

	resp := &zbstorerpc.Build{
		ID:      args.BuildID,
		Status:  zbstorerpc.BuildUnknown,
		Results: []*zbstorerpc.BuildResult{},
	}
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/find.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id": buildID.String(),
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			switch typ := stmt.ColumnType(stmt.ColumnIndex("ended_at")); typ {
			case sqlite.TypeNull:
				if !isActive {
					// If we don't have an end time and we're not running a build with this ID,
					// assume it was orphaned from a previous run.
					return nil
				}
				resp.Status = zbstorerpc.BuildActive
			case sqlite.TypeInteger:
				resp.Status = zbstorerpc.BuildStatus(stmt.GetText("status"))
				resp.EndedAt = zbstorerpc.NonNull(time.UnixMilli(stmt.GetInt64("ended_at")).UTC())
			default:
				return fmt.Errorf("type(ended_at) = %v", typ)
			}

			resp.StartedAt = time.UnixMilli(stmt.GetInt64("started_at"))

			if stmt.GetBool("has_expand") {
				resp.Expand = &zbstorerpc.ExpandResult{
					Builder: stmt.GetText("expand_builder"),
				}
				if s := stmt.GetText("expand_args"); s == "" {
					resp.Expand.Args = []string{}
				} else if err := unmarshalJSONString(s, &resp.Expand.Args); err != nil {
					return fmt.Errorf("expand.args: %v", err)
				}
				if s := stmt.GetText("expand_env"); s == "" {
					resp.Expand.Env = map[string]string{}
				} else if err := unmarshalJSONString(s, &resp.Expand.Env); err != nil {
					return fmt.Errorf("expand.env: %v", err)
				}
			}

			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get build %v: %v", buildID, err)
	}
	if resp.Status == zbstorerpc.BuildUnknown {
		return marshalResponse(resp)
	}

	resp.Results, err = findBuildResults(resp.Results, conn, s.logDir, buildID, "")
	if err != nil {
		return nil, err
	}

	return marshalResponse(resp)
}

func (s *Server) getBuildResult(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.GetBuildResultRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	buildID, ok := parseBuildID(args.BuildID)
	if !ok || args.DrvPath == "" {
		return marshalResponse(&zbstorerpc.BuildResult{
			DrvPath: args.DrvPath,
			Status:  zbstorerpc.BuildUnknown,
			Outputs: []*zbstorerpc.RealizeOutput{},
		})
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	results, err := findBuildResults(nil, conn, s.logDir, buildID, args.DrvPath)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return marshalResponse(&zbstorerpc.BuildResult{
			DrvPath: args.DrvPath,
			Status:  zbstorerpc.BuildUnknown,
			Outputs: []*zbstorerpc.RealizeOutput{},
		})
	}
	if len(results) > 1 {
		return nil, fmt.Errorf("internal error: multiple build results found for %s in build %v", args.DrvPath, buildID)
	}
	return marshalResponse(results[0])
}

func (s *Server) cancelBuild(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.CancelBuildNotification
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	buildID, ok := parseBuildID(args.BuildID)
	if !ok {
		return nil, nil
	}
	s.activeBuildsMu.Lock()
	cancel := s.activeBuilds[buildID]
	s.activeBuildsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil, nil
}

func (s *Server) readLog(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.ReadLogRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	newNotFoundError := func() error {
		// TODO(someday): Use 404-like error code.
		return fmt.Errorf("could not find log for %q in build ID %q", args.DrvPath, args.BuildID)
	}
	if args.DrvPath == "" {
		return nil, newNotFoundError()
	}
	if args.RangeStart < 0 {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("log range start must be non-negative"))
	}
	if args.RangeEnd.Valid && args.RangeEnd.X <= args.RangeStart {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("log range end must be greater than range start"))
	}
	buildID, ok := parseBuildID(args.BuildID)
	if !ok {
		return nil, newNotFoundError()
	}

	f, openError := os.Open(builderLogPath(s.logDir, buildID, args.DrvPath))
	if errors.Is(openError, os.ErrNotExist) {
		conn, err := s.db.Get(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch build result for %s in build %v: %v", args.DrvPath, buildID, err)
		}
		defer s.db.Put(conn)
		results, err := findBuildResults(nil, conn, "", buildID, args.DrvPath)
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, newNotFoundError()
		}
		// Treat like a zero-length log.
		return marshalResponse(&zbstorerpc.ReadLogResponse{EOF: true})
	}
	if openError != nil {
		return nil, openError
	}
	defer f.Close()

	const maxRead = 64 * 1024
	end := args.RangeStart + maxRead
	if args.RangeEnd.Valid {
		end = min(end, args.RangeEnd.X)
	}
	buf := make([]byte, end-args.RangeStart)

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, err)
	}
	if args.RangeStart+int64(len(buf)) < size {
		// Special case: if the requested range is within what's already written,
		// we can skip acquiring a database connection.
		// We only need the database connection to check whether the builder is finished.
		if _, err := f.Seek(args.RangeStart, io.SeekStart); err != nil {
			return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, err)
		}
		n, err := io.ReadFull(f, buf)
		if n == 0 {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, err)
		}
		resp := &zbstorerpc.ReadLogResponse{EOF: err == io.EOF}
		resp.SetPayload(buf[:n])
		return marshalResponse(resp)
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	var results []*zbstorerpc.BuildResult
	for {
		var err error
		results, err = findBuildResults(results[:0], conn, "", buildID, args.DrvPath)
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, newNotFoundError()
		}

		// Read log size after reading builder status.
		// If the status is finished, then the log should be at its final size.
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, err)
		}
		// At least one byte should be available.
		if args.RangeStart < size {
			if _, err := f.Seek(args.RangeStart, io.SeekStart); err != nil {
				return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, err)
			}
			n := 0
			var readError error
			for n < len(buf) && readError == nil {
				var nn int
				nn, readError = f.Read(buf[n:])
				n += nn
			}
			if n == 0 {
				if readError == io.EOF {
					readError = io.ErrUnexpectedEOF
				}
				return nil, fmt.Errorf("read log for %s in build %v: %v", args.DrvPath, buildID, readError)
			}
			resp := &zbstorerpc.ReadLogResponse{
				EOF: readError == io.EOF && results[0].Status.IsFinished(),
			}
			resp.SetPayload(buf[:n])
			return marshalResponse(resp)
		}

		if results[0].Status.IsFinished() {
			if args.RangeStart > size {
				return nil, fmt.Errorf("read log for %s in build %v: start byte %d out of range",
					args.DrvPath, buildID, args.RangeStart)
			}
			return marshalResponse(&zbstorerpc.ReadLogResponse{EOF: true})
		}

		// Wait for more.
		if err := xtime.Sleep(ctx, builderLogInterval); err != nil {
			return nil, fmt.Errorf("read log for %s in build %v: %w", args.DrvPath, buildID, err)
		}
	}
}

// RecentBuildIDs returns the most recent builds started or finished.
// It returns at most limit values.
func (s *Server) RecentBuildIDs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent builds: %v", err)
	}
	defer s.db.Put(conn)

	result := make([]string, 0, limit)
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/recent.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":n": limit,
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result = append(result, stmt.GetText("id"))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list recent builds: %v", err)
	}
	return result, nil
}

// Register adds the info for a store object that is already present in the store directory
// to the store's database.
// If the store path is already in the database
// and the information matches what is already there,
// then Register is a no-op and returns nil.
//
// If info.NARSize and/or info.NARHash are omitted, then Register will compute them
// before storing the info in the store's database.
func (s *Server) Register(ctx context.Context, info *zbstore.ObjectInfo) error {
	storeRefs := zbstore.MakeReferences(info.StorePath, &info.References)
	if err := zbstore.ValidateContentAddress(info.ContentAddress, storeRefs); err != nil {
		return fmt.Errorf("register %s: %v", info.StorePath, err)
	}

	unlock, err := s.writing.lock(ctx, info.StorePath)
	if err != nil {
		return fmt.Errorf("register %s: %v", info.StorePath, err)
	}
	realPath := s.realPath(info.StorePath)
	_, statError := os.Lstat(realPath)
	unlock()
	if statError != nil {
		return fmt.Errorf("register %s: %v", info.StorePath, statError)
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return err
	}
	defer s.db.Put(conn)

	existingInfo, err := pathInfo(conn, info.StorePath)
	if err == nil {
		if info.NARHash.IsZero() {
			existingInfo.NARHash = nix.Hash{}
		}
		if !info.HasNARSize() {
			existingInfo.NARSize = -1
		}
		if !existingInfo.Equal(info) {
			return fmt.Errorf("register %s: does not match existing data", info.StorePath)
		}
		return nil
	}
	if !errors.Is(err, zbstore.ErrNotFound) {
		return fmt.Errorf("register %s: %v", info.StorePath, err)
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	var hasher *nix.Hasher
	if info.NARHash.IsZero() {
		hasher = nix.NewHasher(nix.SHA256)
	}
	go func() {
		defer close(done)
		var dumpWriter io.Writer = pw
		if hasher != nil {
			dumpWriter = io.MultiWriter(hasher, pw)
		}
		err := nar.DumpPath(dumpWriter, realPath)
		pw.CloseWithError(err)
	}()
	obj := &pipeObject{info: *info, narContent: pr}
	info.NARSize, err = zbstore.VerifyObject(ctx, io.Discard, obj, &zbstore.ContentAddressOptions{
		CreateTemp: s.caCreateTemp,
		Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
	})
	pr.Close()
	<-done
	if err != nil {
		return fmt.Errorf("register %s: %v", info.StorePath, err)
	}
	if hasher != nil {
		info.NARHash = hasher.SumHash()
	}

	if err := insertObject(ctx, conn, info); err != nil {
		return fmt.Errorf("register %s: %v", info.StorePath, err)
	}
	return nil
}

// Delete deletes the set of store paths.
// Delete will return an error if any of the named paths do not exist
// or there are store objects beyond those named that refer to the named store objects.
func (s *Server) Delete(ctx context.Context, paths sets.Set[zbstore.Path]) error {
	return s.delete(ctx, paths, false)
}

// DeleteIncludingReferences is the same as [*Server.Delete],
// but if the store objects have other store objects referring to them,
// then they will be deleted as well.
func (s *Server) DeleteIncludingReferences(ctx context.Context, paths sets.Set[zbstore.Path]) error {
	return s.delete(ctx, paths, true)
}

func (s *Server) delete(ctx context.Context, paths sets.Set[zbstore.Path], recursive bool) (err error) {
	if paths.Len() == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			if path, singleError := xiter.Single(paths.All()); singleError == nil {
				err = fmt.Errorf("delete %s: %v", path, err)
			} else {
				err = fmt.Errorf("delete store paths: %v", err)
			}
		}
	}()

	for path := range paths.All() {
		if path.Dir() != s.dir {
			return fmt.Errorf("%s not in %s", path, s.dir)
		}
	}

	var allPaths []zbstore.Path
	var unlocks []func()
	defer func() {
		for _, u := range unlocks {
			u()
		}
		unlocks = nil
	}()
	err = func() (err error) {
		conn, err := s.db.Get(ctx)
		if err != nil {
			return err
		}
		defer s.db.Put(conn)

		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)
		if err := sqlitex.ExecuteScriptFS(conn, sqlFiles(), "delete/create_target_table.sql", nil); err != nil {
			return err
		}

		insertStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "delete/insert_target.sql")
		if err != nil {
			return err
		}
		defer insertStmt.Finalize()
		for path := range paths.All() {
			if exists, err := objectExists(conn, path); err != nil {
				return err
			} else if !exists {
				return fmt.Errorf("%s: %w", path, zbstore.ErrNotFound)
			}

			insertStmt.SetText(":path", string(path))
			if _, err := insertStmt.Step(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			if err := insertStmt.Reset(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
		}

		reverseDeps := make(sets.Set[zbstore.Path])
		err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "delete/reverse_deps.sql", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				path, err := zbstore.ParsePath(stmt.GetText("path"))
				if err != nil {
					return err
				}
				reverseDeps.Add(path)
				return nil
			},
		})
		if err != nil {
			return err
		}

		if reverseDeps.Len() > 0 {
			log.Debugf(ctx, "Found more dependencies to delete: %v", reverseDeps)
			if !recursive {
				return fmt.Errorf("store objects have references")
			}
		}
		if err := sqlitex.ExecuteScriptFS(conn, sqlFiles(), "delete/drop_target_table.sql", nil); err != nil {
			return err
		}

		// Reverse topological sort.
		allPaths = make([]zbstore.Path, 0, paths.Len()+reverseDeps.Len())
		allPaths = slices.AppendSeq(allPaths, xiter.Chain(paths.All(), reverseDeps.All()))
		references := make(map[zbstore.Path]sets.Sorted[zbstore.Path], len(allPaths))
		for _, path := range allPaths {
			var err error
			references[path], err = listReferences(conn, path)
			if err != nil {
				return err
			}
		}
		err = sortByReferences(
			allPaths,
			func(p zbstore.Path) zbstore.Path { return p },
			func(p zbstore.Path) sets.Sorted[zbstore.Path] { return references[p] },
			true,
		)
		if err != nil {
			return err
		}
		slices.Reverse(allPaths)

		// Delete the files in reverse dependency order.
		deleteStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "delete/delete.sql")
		if err != nil {
			return err
		}
		defer deleteStmt.Finalize()
		deleteSelfRefStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "delete/delete_self_ref.sql")
		if err != nil {
			return err
		}
		defer deleteSelfRefStmt.Finalize()
		for _, path := range allPaths {
			deleteSelfRefStmt.SetText(":path", string(path))
			if _, err := deleteSelfRefStmt.Step(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			if err := deleteSelfRefStmt.Reset(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}

			deleteStmt.SetText(":path", string(path))
			if _, err := deleteStmt.Step(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			if err := deleteStmt.Reset(); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
		}

		// Acquire write locks on the paths we're about to delete before committing the transaction.
		unlocks = make([]func(), 0, len(allPaths))
		for _, path := range allPaths {
			unlock, err := s.writing.lock(ctx, path)
			if err != nil {
				return err
			}
			unlocks = append(unlocks, unlock)
		}

		// We end the transaction before removing the files
		// to avoid blocking other work
		// but also because we don't want a partial failure in removing files
		// to abort the transaction.
		return nil
	}()
	if err != nil {
		return err
	}

	ok := true
	for _, path := range allPaths {
		log.Debugf(ctx, "Deleting store object %s...", path)
		if err := os.RemoveAll(s.realPath(path)); err != nil {
			log.Errorf(ctx, "Failed to delete %s: %v", path, err)
			ok = false
		}
	}
	if !ok {
		return fmt.Errorf("one or more store paths could not be deleted")
	}
	return nil
}

func (s *Server) fetch(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var args zbstorerpc.FetchRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	if len(args.Paths) == 0 {
		return marshalResponse(&zbstorerpc.FetchResponse{})
	}

	// Since the copy can take a while, detach the copy operation from this RPC.
	// We'll block on it for as long as we can,
	// and the caller can retry the RPC and it will eventually not block.
	copyDone := make(chan struct{})
	err := s.detachFromRPC(ctx, func(ctx context.Context) {
		defer close(copyDone)
		conn, err := s.db.Get(ctx)
		if err != nil {
			return
		}
		defer s.db.Put(conn)
		err = s.copyFromFallback(ctx, conn, func(yield func(pathAndEquivalenceClass) bool) {
			for _, path := range args.Paths {
				pe := pathAndEquivalenceClass{path: path}
				if !yield(pe) {
					return
				}
			}
		})
		if err != nil {
			log.Infof(ctx, "After fetch: %v", err)
		}
	})
	if err != nil {
		return nil, err
	}

	select {
	case <-copyDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)
	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, err
	}
	defer rollback()

	resp := &zbstorerpc.FetchResponse{
		Found: make(map[zbstore.Path]*zbstorerpc.ObjectInfo, len(args.Paths)),
	}
	for _, path := range args.Paths {
		info, err := pathInfo(conn, path)
		if err == nil {
			resp.Found[path] = zbstorerpc.NewObjectInfo(info)
		} else if !errors.Is(err, zbstore.ErrNotFound) {
			return nil, err
		}
	}
	return marshalResponse(resp)
}

// copyFromFallback imports any store objects identified by paths
// that are not present in the store directory
// from the fallback store.
// The [equivalenceClass] values are used in error messages.
// If any of the store objects could not be downloaded, then copyFromFallback will return an error.
// If copyFromFallback returns an error, it will always be a [copyFromFallbackError].
func (s *Server) copyFromFallback(ctx context.Context, conn *sqlite.Conn, paths iter.Seq[pathAndEquivalenceClass]) (err error) {
	defer func() {
		if err != nil {
			err = copyFromFallbackError{err}
		}
	}()

	storePathsToDownload := make(map[zbstore.Path]sets.Set[equivalenceClass])
	exists := make(sets.Set[zbstore.Path])
	for pe := range paths {
		if exists.Has(pe.path) {
			continue
		}
		if eqClassSet, ok := storePathsToDownload[pe.path]; ok {
			if !pe.equivalenceClass.isZero() {
				if eqClassSet == nil {
					eqClassSet = make(sets.Set[equivalenceClass])
					storePathsToDownload[pe.path] = eqClassSet
				}
				eqClassSet.Add(pe.equivalenceClass)
			}
			continue
		}

		log.Debugf(ctx, "Waiting for lock on %s (output of %v)...", pe.path, pe.equivalenceClass)
		unlockInput, err := s.writing.lock(ctx, pe.path)
		if err != nil {
			return err
		}
		_, err = os.Lstat(s.realPath(pe.path))
		unlockInput()
		log.Debugf(ctx, "%s exists=%t (output of %v)", pe.path, err == nil, pe.equivalenceClass)
		if err == nil {
			exists.Add(pe.path)
		} else if pe.equivalenceClass.isZero() {
			if _, ok := storePathsToDownload[pe.path]; !ok {
				storePathsToDownload[pe.path] = nil
			}
		} else {
			addToMultiMap(storePathsToDownload, pe.path, pe.equivalenceClass)
		}
	}

	if len(storePathsToDownload) == 0 {
		return nil
	}

	pr, pw := io.Pipe()
	exportFinished := make(chan error)
	go func() {
		err := zbstore.Export(ctx, s.fallback, pw, sets.Collect(maps.Keys(storePathsToDownload)), nil)
		pw.CloseWithError(err)
		exportFinished <- err
	}()

	recv := s.newNARReceiver(ctx, s.caCreateTemp, &singleConnectionGetter{
		conn: conn,
	})
	pathRecorder := &pathRecorderReceiver{receiver: recv}
	receiveError := zbstore.ReceiveExport(pathRecorder, pr)
	recv.Cleanup(ctx)
	pr.CloseWithError(receiveError)
	exportError := <-exportFinished
	if exportError != nil {
		return fmt.Errorf("failed to copy from fallback store: %v", exportError)
	}
	if receiveError != nil {
		return fmt.Errorf("failed to copy from fallback store: %v", receiveError)
	}

	var ec multierror.Collector
	for path, eqClassesForPath := range storePathsToDownload {
		if !pathRecorder.paths.Has(path) {
			var err error
			if eqClassesForPath.Len() > 0 {
				err = fmt.Errorf("fallback store did not export %s (output of %v)", path, eqClassesForPath)
			} else {
				err = fmt.Errorf("fallback store did not export %s", path)
			}
			ec.Add(err)
			continue
		}

		log.Debugf(ctx, "Waiting for lock on %s (%v)...", path, eqClassesForPath)
		unlockInput, err := s.writing.lock(ctx, path)
		if err != nil {
			return err
		}
		_, err = os.Lstat(s.realPath(path))
		unlockInput()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if eqClassesForPath.Len() > 0 {
					err = fmt.Errorf("failed to import %s (output of %v) from fallback store", path, eqClassesForPath)
				} else {
					err = fmt.Errorf("failed to import %s from fallback store", path)
				}
			}
			ec.Add(err)
		}
	}

	return ec.Error()
}

// uploadClosure uploads the store objects in the batch
// and all transitive dependencies of those store objects.
// uploadClosure will attempt to upload as many objects as possible
// without uploading any objects
// where their referenced objects have not been confirmed to be uploaded.
func (s *Server) uploadClosure(ctx context.Context, batch iter.Seq[*zbstore.ObjectInfo]) {
	if s.upload == nil {
		return
	}

	objectMap := s.makeObjectInfoMap(ctx, batch, func(err error) {
		log.Warnf(ctx, "%v", err)
	})
	stack := make([]*zbstore.ObjectInfo, 0, len(objectMap))
	for _, obj := range objectMap {
		if obj.References.Len() == 0 {
			stack = append(stack, obj)
		}
	}

	done := make(map[zbstore.Path]bool)
	for len(stack) > 0 {
		curr := xslices.Last(stack)
		stack = xslices.Pop(stack, 1)
		if _, visited := done[curr.StorePath]; visited {
			continue
		}
		err := s.uploadObject(ctx, curr)
		done[curr.StorePath] = err == nil
		if err != nil {
			log.Warnf(ctx, "%v", err)
			continue
		}
		for _, other := range objectMap {
			if other.StorePath == curr.StorePath || !other.References.Has(curr.StorePath) {
				continue
			}
			if xiter.All(other.References.Values(), func(p zbstore.Path) bool { return done[p] }) {
				stack = append(stack, other)
			}
		}
	}
}

// uploadObject uploads a single object to the s.upload store, retrying as necessary.
func (s *Server) uploadObject(ctx context.Context, obj *zbstore.ObjectInfo) error {
	if s.upload == nil {
		return nil
	}

	log.Infof(ctx, "Uploading %s to store at %s...", obj.StorePath, s.upload.URL.Redacted())
	log.Debugf(ctx, "Waiting for lock on %s...", obj.StorePath)
	unlock, err := s.writing.lock(ctx, obj.StorePath)
	if err != nil {
		return fmt.Errorf("upload %s: %w", obj.StorePath, err)
	}
	realPath := s.realPath(obj.StorePath)
	_, statError := os.Lstat(realPath)
	unlock()
	if statError != nil {
		return fmt.Errorf("upload %s: %v", obj.StorePath, err)
	}

	for t := xtime.NewBackoffTimer(uploadBackoffTable[:], uploadBackoffJitter); ; {
		var dumpGroup sync.WaitGroup
		err := s.upload.PutObject(ctx, &zbstorehttp.PutObjectRequest{
			StorePath:      obj.StorePath,
			References:     obj.References,
			ContentAddress: obj.ContentAddress,
			NARSize:        obj.NARSize,
			GetNAR: func() (io.ReadCloser, error) {
				pr, pw := io.Pipe()
				dumpGroup.Go(func() {
					err := nar.DumpPath(pw, realPath)
					pw.CloseWithError(err)
				})
				return pr, nil
			},
		})
		dumpGroup.Wait()

		if err == nil {
			log.Infof(ctx, "Uploaded %s", obj.StorePath)
			return nil
		}
		if zbstorehttp.IsPermanentError(err) {
			return err
		}
		log.Warnf(ctx, "%v", err)
		if err := t.Sleep(ctx); err != nil {
			return fmt.Errorf("upload %s: %w", obj.StorePath, err)
		}
	}
}

// makeObjectInfoMap builds a map of the transitive closure of [*zbstore.ObjectInfo] values
// for the objects sequence.
func (s *Server) makeObjectInfoMap(ctx context.Context, objects iter.Seq[*zbstore.ObjectInfo], onError func(error)) map[zbstore.Path]*zbstore.ObjectInfo {
	result := make(map[zbstore.Path]*zbstore.ObjectInfo)
	for obj := range objects {
		result[obj.StorePath] = obj
	}
	var stack []zbstore.Path
	for _, obj := range result {
		stack = slices.Grow(stack, obj.References.Len())
		stack = slices.AppendSeq(stack, obj.References.Values())
	}

	if len(stack) > 0 {
		conn, err := s.db.Get(ctx)
		if err != nil {
			onError(fmt.Errorf("gather object information: %w", err))
			return result
		}
		defer s.db.Put(conn)

		for len(stack) > 0 {
			curr := xslices.Last(stack)
			stack = xslices.Pop(stack, 1)
			if _, ok := result[curr]; ok {
				continue
			}
			result[curr], err = pathInfo(conn, curr)
			if err != nil {
				if errors.Is(err, zbstore.ErrNotFound) {
					log.Debugf(ctx, "%v", err)
				} else {
					onError(err)
				}
			}
		}

		for p, obj := range result {
			if obj == nil {
				delete(result, p)
			}
		}
	}

	return result
}

// uploadRealizations uploads a [zbstore.RealizationMap] to the s.upload store, retrying as necessary.
func (s *Server) uploadRealizations(ctx context.Context, realizations zbstore.RealizationMap) {
	if s.upload == nil || realizations.IsEmpty() {
		return
	}

	for t := xtime.NewBackoffTimer(uploadBackoffTable[:], uploadBackoffJitter); ; {
		err := s.upload.PutRealizations(ctx, realizations)
		if err == nil {
			log.Infof(ctx, "Uploaded realizations for %v", realizations.DerivationHash)
			return
		}
		log.Warnf(ctx, "%v", err)
		if zbstorehttp.IsPermanentError(err) {
			return
		}

		if err := t.Sleep(ctx); err != nil {
			return
		}
	}
}

func (s *Server) gcLogs(ctx context.Context, window time.Duration) {
	ticker := time.NewTicker(min(5*time.Minute, window))
	defer ticker.Stop()

	t := time.Now()
	for {
		conn, err := s.db.Get(ctx)
		if err != nil {
			// Likely means context was canceled.
			log.Debugf(ctx, "Exiting build log GC due to: %v", err)
			return
		}
		cutoff := t.Add(-window)
		s.activeBuildsMu.Lock()
		activeBuilds := slices.Collect(maps.Keys(s.activeBuilds))
		s.activeBuildsMu.Unlock()
		log.Debugf(ctx, "Cleaning up build logs older than %v...", cutoff.UTC())
		n, err := deleteOldBuilds(ctx, conn, cutoff, &deleteOldBuildOptions{
			logDir: s.logDir,
			keep:   slices.Values(activeBuilds),
		})
		if err != nil {
			log.Warnf(ctx, "Failed to clean up build logs: %v", err)
		} else if n > 0 {
			log.Infof(ctx, "Deleted %d build logs older than %v", n, cutoff.Truncate(time.Millisecond).UTC())
		} else {
			log.Debugf(ctx, "No build logs to clean up.")
		}
		// Attempt to reclaim disk space.
		if err := sqlitex.ExecuteTransient(conn, "PRAGMA incremental_vacuum(128);", nil); err != nil {
			log.Warnf(ctx, "Incremental vacuum failed: %v", err)
		}
		s.db.Put(conn)

		select {
		case t = <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// LaunchCheck waits until the [Server] has verified that it is the only one operating on its database
// or the ctx.Done() channel is closed,
// whichever comes first.
func (s *Server) LaunchCheck(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.launchCheckDone:
		return s.launchCheckError
	}
}

func (s *Server) writeHeartbeat(ctx context.Context) {
	err := func() (err error) {
		conn, err := s.db.Get(ctx)
		if err != nil {
			return err
		}
		defer s.db.Put(conn)

		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)

		lastHeartbeat, err := lastHeartbeat(conn)
		if err != nil {
			return err
		}
		if time.Since(lastHeartbeat) < 2*heartRate {
			return fmt.Errorf("another heartbeat detected")
		}

		if err := updateHeartbeat(conn); err != nil {
			// Failing to write the first heartbeat shouldn't fail the launch check.
			log.Errorf(ctx, "%v", err)
		}
		return nil
	}()

	if err != nil {
		s.launchCheckError = fmt.Errorf("check for other running servers: %v", err)
		close(s.launchCheckDone)
		return
	}
	close(s.launchCheckDone)

	ticker := time.NewTicker(heartRate)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			err := func() (err error) {
				bgCtx, cancel := xcontext.KeepAlive(ctx, 30*time.Second)
				defer cancel()

				conn, err := s.db.Get(bgCtx)
				if err != nil {
					return err
				}
				defer s.db.Put(conn)

				return clearServerState(conn)
			}()
			if err != nil {
				log.Warnf(ctx, "Failed to clear server heartbeat: %v", err)
			} else {
				log.Debugf(ctx, "Cleared server heartbeat.")
			}
			return
		}

		err = func() (err error) {
			conn, err := s.db.Get(ctx)
			if err != nil {
				return err
			}
			defer s.db.Put(conn)

			endFn, err := sqlitex.ImmediateTransaction(conn)
			if err != nil {
				return err
			}
			defer endFn(&err)

			return updateHeartbeat(conn)
		}()
		if err != nil {
			log.Warnf(ctx, "Failed to update server heartbeat: %v", err)
		} else {
			log.Debugf(ctx, "Updated server heartbeat")
		}
	}
}

func (s *Server) optimizeDatabase(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		conn, err := s.db.Get(ctx)
		if err != nil {
			// Likely means context was canceled.
			log.Debugf(ctx, "Exiting background optimization due to: %v", err)
			return
		}
		if err := sqlitex.ExecuteTransient(conn, "PRAGMA optimize;", nil); err != nil {
			log.Warnf(ctx, "Incremental vacuum failed: %v", err)
		}
		s.db.Put(conn)
	}
}

func parseBuildID(id string) (_ uuid.UUID, ok bool) {
	u, err := uuid.Parse(id)
	if err != nil || id != u.String() {
		return uuid.UUID{}, false
	}
	return u, true
}

func marshalResponse(data any, opts ...jsonv2.Options) (*jsonrpc.Response, error) {
	jsonData, err := jsonv2.Marshal(data, opts...)
	if err != nil {
		return nil, jsonrpc.Error(jsonrpc.InternalError, err)
	}
	return &jsonrpc.Response{Result: jsonData}, nil
}

// copyFromFallbackError is an error returned by [*Server.copyFromFallback].
type copyFromFallbackError struct {
	err error
}

func isCopyFromFallbackError(err error) bool {
	_, ok := errors.AsType[copyFromFallbackError](err)
	return ok
}

func (e copyFromFallbackError) Error() string { return e.err.Error() }
func (e copyFromFallbackError) Unwrap() error { return e.err }

const uploadBackoffJitter = 0.25

var uploadBackoffTable = [...]time.Duration{
	1 * time.Second,
	1 * time.Second,
	1 * time.Second,
	5 * time.Second,
	5 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}
