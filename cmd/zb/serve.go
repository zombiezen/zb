// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/osutil"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/ui"
	"zb.256lights.llc/pkg/internal/xnet"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/bass/runhttp"
	"zombiezen.com/go/log"
	"zombiezen.com/go/log/zstdlog"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/xcontext"
)

const contentAddressTempFilePattern = "zb-ca-*"

type serveCommand struct {
	storeDatabaseFlags `kong:"embed"`

	BuildDir          string            `kong:"name=build-root,default=${temp_dir},help=Store build artifacts in this directory."`
	BuildUsersGroup   string            `kong:"default=${build_users_group},placeholder=${default_build_users_group},help=Run builds as users in the Unix group with the given name."`
	LogDirectory      string            `kong:"default=${default_log_dir},help=Store logs in this directory."`
	KeyFiles          []string          `kong:"name=signing-key,sep=none,placeholder=file,help=Key files for signing realizations (can be passed multiple times)"`
	Sandbox           bool              `kong:"negatable,default=${supports_sandbox},help=Run builders in a restricted environment."`
	SandboxPaths      sandboxPathsFlags `kong:"embed"`
	AllowKeepFailed   bool              `kong:"negatable,default=true,help=Allow user to skip cleanup of failed builds."`
	CoresPerBuild     int               `kong:"default=${num_cpu},help=Hint to builders for number of concurrent jobs to run"`
	BuildLogRetention time.Duration     `kong:"default=168h,help=Delete finished build logs after this duration. (Default: ${default})"`
	SystemdSocket     bool              `kong:"help=Use systemd socket activation"`

	WebListenAddress   string `kong:"name=ui,placeholder=[host]:port,help=Serve HTTP for web UI at the given address."`
	AllowRemoteWeb     bool   `kong:"name=allow-remote-ui,help=Accept non-localhost connections for web UI."`
	TemplatesDirectory string `kong:"name=dev-templates,hidden,placeholder=dir,help=Directory to use for templates"`
	StaticDirectory    string `kong:"name=dev-static,hidden,placeholder=dir,help=Directory to use for static assets"`
}

func (c *serveCommand) Signature() string {
	return `help:"Run a build server."`
}

func (c *serveCommand) Run(ctx context.Context, g *globalConfig, drain drainSignalChan) error {
	if !g.Directory.IsNative() {
		return fmt.Errorf("%s cannot be used on this system", g.Directory)
	}
	if c.Sandbox && !backend.CanSandbox() {
		if !backend.SystemSupportsSandbox() {
			return fmt.Errorf("sandboxing requested but not supported on %v", system.Current())
		}
		return fmt.Errorf("sandboxing requested but unable to use (are you running with admin privileges?)")
	}
	allKeyFiles := make([]string, 0, len(g.Server.KeyFiles)+len(c.KeyFiles))
	allKeyFiles = append(allKeyFiles, g.Server.KeyFiles...)
	allKeyFiles = append(allKeyFiles, c.KeyFiles...)
	keyring, err := readKeyringFromFiles(allKeyFiles)
	if err != nil {
		return err
	}
	storeDirGroupID, buildUsers, err := buildUsersForGroup(ctx, c.BuildUsersGroup)
	if err != nil {
		return err
	}
	if err := ensureStoreDirectory(string(g.Directory), storeDirGroupID); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.StoreSocket), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.DBPath), 0o755); err != nil {
		return err
	}
	// TODO(someday): Properly set permissions on the created database.

	provideHTTPClient, cleanup := singletonProvider(g.newHTTPClient)
	defer cleanup()

	fallbackStore, err := g.Server.Download.toStore(func() (zbstorehttp.Client, error) {
		return provideHTTPClient()
	})
	if err != nil {
		return err
	}
	uploadStore, err := g.Server.Upload.toStore(func() (zbstorehttp.Client, error) {
		return provideHTTPClient()
	})
	if err != nil {
		return err
	}
	var uploadHTTPStore *zbstorehttp.Store
	switch uploadStore := uploadStore.(type) {
	case zbstore.Null:
	case *zbstorehttp.Store:
		uploadHTTPStore = uploadStore
	default:
		return fmt.Errorf("unsupported type %q for upload store", g.Server.Upload.storeType)
	}

	webHandler := new(webServer)
	if c.TemplatesDirectory != "" {
		root, err := os.OpenRoot(c.TemplatesDirectory)
		if err != nil {
			return err
		}
		webHandler.templateFiles = root.FS()
	} else {
		webHandler.templateFiles = ui.TemplateFiles()
	}
	if c.StaticDirectory != "" {
		root, err := os.OpenRoot(c.StaticDirectory)
		if err != nil {
			return err
		}
		webHandler.staticAssets = root.FS()
	} else {
		webHandler.staticAssets = ui.StaticAssets()
	}

	loggedShuttingDown := make(chan struct{})
	stopShutdownLog := context.AfterFunc(ctx, func() {
		defer close(loggedShuttingDown)
		log.Infof(ctx, "Shutting down (signal received)...")
	})
	defer func() {
		if !stopShutdownLog() {
			<-loggedShuttingDown
		}
	}()

	grp, grpCtx := errgroup.WithContext(ctx)
	backendServer := backend.NewServer(g.Directory, c.DBPath, &backend.Options{
		BuildDirectory:              c.BuildDir,
		LogDirectory:                c.LogDirectory,
		ContentAddressBufferCreator: bytebuffer.TempFileCreator{Pattern: contentAddressTempFilePattern},
		SandboxPaths:                c.SandboxPaths.toMap(),
		DisableSandbox:              !c.Sandbox,
		BuildUsers:                  buildUsers,
		AllowKeepFailed:             c.AllowKeepFailed,
		CoresPerBuild:               c.CoresPerBuild,
		BuildLogRetention:           c.BuildLogRetention,
		Keyring:                     keyring,
		Fallback:                    fallbackStore,
		Writer:                      uploadHTTPStore,
	})
	defer func() {
		if err := backendServer.Close(); err != nil {
			log.Errorf(ctx, "%v", err)
		}
	}()
	webHandler.backend = backendServer

	grp.Go(func() error {
		return c.listenRPC(grpCtx, backendServer, g)
	})

	drainFinished := errors.New("drain finished")
	if drain != nil {
		grp.Go(func() error {
			select {
			case <-drain:
				log.Infof(ctx, "Draining (signal received)...")
				err := backendServer.Drain(grpCtx)
				// Once the drain is complete,
				// we want to interrupt the listen goroutines by returning a non-nil error.
				return cmp.Or(err, drainFinished)
			case <-grpCtx.Done():
				return nil
			}
		})
	}

	if c.WebListenAddress != "" {
		grp.Go(func() error {
			httpServer := &http.Server{
				Addr:    c.WebListenAddress,
				Handler: webHandler,
				BaseContext: func(l net.Listener) context.Context {
					return grpCtx
				},
				ErrorLog: zstdlog.New(log.Default(), &zstdlog.Options{
					Context: grpCtx,
					Level:   log.Error,
				}),

				ReadTimeout:       60 * time.Second,
				ReadHeaderTimeout: 30 * time.Second,
				WriteTimeout:      60 * time.Second,
			}
			if !c.AllowRemoteWeb {
				httpServer.Handler = localOnlyMiddleware{httpServer.Handler}
			}

			err := runhttp.Serve(grpCtx, httpServer, &runhttp.Options{
				OnStartup: func(ctx context.Context, addr net.Addr) {
					addrString := addr.String()
					if parsed, err := xnet.HostPortToIP(addrString, netip.Addr{}); err != nil {
						log.Debugf(ctx, "Invalid listen address: %v", err)
					} else {
						port := strconv.Itoa(int(parsed.Port()))
						if host := parsed.Addr(); host.IsLoopback() || host.IsUnspecified() {
							addrString = net.JoinHostPort("localhost", port)
						}
					}
					log.Infof(ctx, "Listening for HTTP on http://%s", addrString)
				},
			})
			return cmp.Or(err, net.ErrClosed)
		})
	}

	waitError := grp.Wait()
	if errors.Is(waitError, net.ErrClosed) || errors.Is(waitError, drainFinished) {
		waitError = nil
	}
	return waitError
}

func (c *serveCommand) listenRPC(ctx context.Context, server *backend.Server, g *globalConfig) error {
	if err := server.LaunchCheck(ctx); err != nil {
		return err
	}

	var l net.Listener
	if runtime.GOOS == "linux" && c.SystemdSocket {
		listeners, err := activation.Listeners()
		if err != nil {
			return err
		}
		if len(listeners) != 1 {
			return fmt.Errorf("systemd passed in %d sockets (want 1)", len(listeners))
		}
		l = listeners[0]
		log.Infof(ctx, "Listening on systemd-provided socket")
	} else {
		if err := os.Remove(g.StoreSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err == nil {
			log.Infof(ctx, "Cleaned up existing socket at %s", g.StoreSocket)
		}

		var err error
		l, err = listenUnix(g.StoreSocket)
		if err != nil {
			return err
		}
		defer func() {
			if err := os.Remove(g.StoreSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warnf(ctx, "Failed to clean up socket: %v", err)
			}
		}()
		log.Infof(ctx, "Listening on %s", g.StoreSocket)
	}

	grp, ctx := errgroup.WithContext(ctx)
	closer := xcontext.CloseWhenDone(ctx, l)
	defer closer.Close()
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
					// Don't close the connection if an object cannot be written.
					ObjectWriter: ignoreErrorsObjectWriter{server},
					HashType:     nix.SHA256,
					BufferCreator: bytebuffer.TempFileCreator{
						Pattern: "zb-serve-receive-*.nar",
					},
				}
				err := zbstorerpc.Serve(ctx, conn, serverImporter)
				// [zbstorerpc.Serve] always returns an error.
				// A client disconnect should not cause the server to shut down,
				// so we always return nil.
				if err != nil {
					log.Debugf(ctx, "Disconnecting from %v: %v", conn.RemoteAddr(), err)
				}
				return nil
			})
		}
	})

	return grp.Wait()
}

type ignoreErrorsObjectWriter struct {
	objectWriter zbstore.ObjectWriter
}

func (w ignoreErrorsObjectWriter) WriteObject(ctx context.Context, object zbstore.Object) error {
	if err := w.objectWriter.WriteObject(ctx, object); err != nil {
		log.Warnf(ctx, "%v", err)
	}
	return nil
}

func ensureStoreDirectory(path string, gid int) error {
	if err := os.MkdirAll(filepath.Dir(string(path)), 0o755); err != nil {
		return err
	}
	const mode os.FileMode = 0o775 | os.ModeSticky
	if err := os.Mkdir(path, mode); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = nil
		}
		return err
	}
	// Run an extra chmod to bypass umask.
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if gid == -1 || gid == os.Getegid() {
		return nil
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return err
	}
	return nil
}

func buildUsersForGroup(ctx context.Context, name string) (gid int, buildUsers []backend.BuildUser, err error) {
	if name == "" {
		return -1, nil, nil
	}
	if runtime.GOOS == "windows" {
		return -1, nil, fmt.Errorf("cannot set --build-users-group on Windows")
	}
	g, userNames, err := osutil.LookupGroup(ctx, name)
	if err != nil {
		return -1, nil, err
	}
	gid, err = strconv.Atoi(g.Gid)
	if err != nil {
		return -1, nil, fmt.Errorf("build users group id: %v", err)
	}
	log.Debugf(ctx, "Using build group %s (gid=%d), users=%v", name, gid, userNames)
	for _, userName := range userNames {
		u, err := user.Lookup(userName)
		if err != nil {
			return gid, nil, fmt.Errorf("build users group: %v", err)
		}
		var buildUser backend.BuildUser
		buildUser.UID, err = strconv.Atoi(u.Uid)
		if err != nil {
			return gid, nil, fmt.Errorf("build users group: user %s: user id: %v", userName, err)
		}
		buildUser.GID, err = strconv.Atoi(u.Gid)
		if err != nil {
			return gid, nil, fmt.Errorf("build users group: user %s: group id: %v", userName, err)
		}
		buildUsers = append(buildUsers, buildUser)
	}
	return gid, buildUsers, nil
}

type serverConfig struct {
	Download *storeConfig `json:"download"`
	Upload   *storeConfig `json:"upload"`
	KeyFiles []string     `json:"signingKeyFiles"`
}

// UnmarshalJSONFrom unmarshals the server configuration object from the JSON decoder,
// merging any fields in the JSON object with existing values.
func (sc *serverConfig) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	tok, err := in.ReadToken()
	if err != nil {
		return err
	}
	if got := tok.Kind(); got != '{' {
		return fmt.Errorf("config must be an object not a %v", got)
	}

	for {
		keyToken, err := in.ReadToken()
		if err != nil {
			return err
		}
		switch kind := keyToken.Kind(); kind {
		case '}':
			return nil
		case '"':
			// Keep going.
		default:
			return fmt.Errorf("unexpected non-string key (%v) in object", kind)
		}

		switch k := keyToken.String(); k {
		case "download":
			if err := jsonv2.UnmarshalDecode(in, &sc.Download); err != nil {
				return fmt.Errorf("unmarshal config.server.download: %w", err)
			}
		case "upload":
			if err := jsonv2.UnmarshalDecode(in, &sc.Upload); err != nil {
				return fmt.Errorf("unmarshal config.server.upload: %w", err)
			}
		case "signingKeyFiles":
			newKeyFiles := sc.KeyFiles[len(sc.KeyFiles):]

			if err := jsonv2.UnmarshalDecode(in, &newKeyFiles); err != nil {
				return fmt.Errorf("unmarshal config.server.signingKeyFiles: %w", err)
			}
			sc.KeyFiles = append(sc.KeyFiles, newKeyFiles...)
		default:
			if reject, _ := jsonv2.GetOption(in.Options(), jsonv2.RejectUnknownMembers); reject {
				return fmt.Errorf("unmarshal config.server: unknown field %q", k)
			}
			if err := in.SkipValue(); err != nil {
				return fmt.Errorf("unmarshal config.server: %w", err)
			}
		}
	}
}

type sandboxPathsFlags struct {
	SandboxPaths       map[string]string `kong:"name=sandbox-path,type=pathmap,placeholder=path,help=Paths to allow in sandbox (can be passed multiple times)"`
	ImplicitSystemDeps sets.Set[string]  `kong:"name=implicit-system-dep,placeholder=path,help=Paths to always mount in sandbox (can be passed multiple times)"`
}

func (flags *sandboxPathsFlags) toMap() map[string]backend.SandboxPath {
	result := make(map[string]backend.SandboxPath)
	for mappedPath, hostPath := range flags.SandboxPaths {
		result[mappedPath] = backend.SandboxPath{Path: hostPath}
	}
	for path := range flags.ImplicitSystemDeps {
		opts := result[path]
		opts.AlwaysPresent = true
		result[path] = opts
	}
	return result
}

func listenUnix(path string) (*net.UnixListener, error) {
	laddr := &net.UnixAddr{
		Net:  "unix",
		Name: path,
	}
	l, err := net.ListenUnix(laddr.Net, laddr)
	if err != nil {
		return nil, err
	}

	// TODO(soon): Restrict to a group.
	if err := os.Chmod(path, 0o777); err != nil {
		l.Close()
		return nil, err
	}

	return l, nil
}
