// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"
	"cloud.google.com/go/storage"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/tailscale/hujson"
	"google.golang.org/api/option"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/althttp"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/httpcache"
	"zb.256lights.llc/pkg/internal/xurl"
	"zb.256lights.llc/pkg/internal/zbstorehttp"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

// globalConfig is the set of configuration settings and persistent command-line flags.
// More details at https://main--zb-docs.netlify.app/configuration
type globalConfig struct {
	Debug             bool                            `json:"debug" kong:"help=Show debugging output."`
	Directory         zbstore.Directory               `json:"storeDirectory" kong:"name=store,default=${default_store_dir},help=Store directory"`
	StoreSocket       string                          `json:"storeSocket" kong:"default=${default_store_socket},help=Server socket"`
	NetrcPath         string                          `json:"netrcFile,omitempty" kong:"name=netrc-file,default=${netrc},help=Use HTTP credentials from the given path."`
	CacheDB           string                          `json:"cacheDB" kong:"name=cache,default=${cache_db},help=Cache database"`
	HTTPCacheDB       string                          `json:"httpCache" kong:"name=http-cache,default=${http_cache},help=Cache HTTP responses in the given file."`
	AllowEnv          stringAllowList                 `json:"allowEnvironment" kong:"-"`
	TrustedPublicKeys []*zbstore.RealizationPublicKey `json:"trustedPublicKeys" kong:"-"`
	Server            serverConfig                    `json:"server,omitzero" kong:"-"`
}

// defaultGlobalConfig returns a [globalConfig] populated with values
// based on OS and generic environment variables (e.g. $HOME, $XDG_CACHE_HOME, etc.).
func defaultGlobalConfig(env envLookupFunc) *globalConfig {
	g := &globalConfig{
		Directory:   zbstore.DefaultDirectory(),
		StoreSocket: filepath.Join(varDir(), "zb", "server.sock"),
	}
	if home, err := env.userHomeDir(); err == nil {
		g.NetrcPath = filepath.Join(home, ".netrc")
	}
	if cd, err := env.userCacheDir(); err == nil {
		g.CacheDB = filepath.Join(cd, "zb", "cache.db")
		g.HTTPCacheDB = filepath.Join(cd, "zb", "http-cache.db")
	}
	return g
}

func (g *globalConfig) clone() *globalConfig {
	if g == nil {
		return nil
	}
	g = new(*g)
	g.TrustedPublicKeys = slices.Clone(g.TrustedPublicKeys)
	if g.Server.Download != nil {
		g.Server.Download = new(*g.Server.Download)
	}
	if g.Server.Upload != nil {
		g.Server.Upload = new(*g.Server.Upload)
	}
	g.Server.KeyFiles = slices.Clone(g.Server.KeyFiles)
	return g
}

// mergeEnvironment copies environment variable values to [globalConfig] fields.
func (g *globalConfig) mergeEnvironment(env envLookupFunc) error {
	if dir := env.get("ZB_STORE_DIR"); dir != "" {
		zbDir, err := zbstore.CleanDirectory(dir)
		if err != nil {
			return err
		}
		g.Directory = zbDir
	}

	if path := env.get("ZB_STORE_SOCKET"); path != "" {
		g.StoreSocket = path
	}

	if path := env.get("NETRC"); path != "" {
		g.NetrcPath = path
	}

	return nil
}

// mergeFiles parses each path as JSON With Commas and Comments
// and merges each into g.
// Thus, later files in the paths sequence take precedence over earlier files.
func (g *globalConfig) mergeFiles(paths iter.Seq[string]) error {
	for path := range paths {
		huJSONData, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		jsonData, err := hujson.Standardize(huJSONData)
		if err != nil {
			return fmt.Errorf("read %s: %v", path, err)
		}
		prev := g.clone()
		if err := jsonv2.Unmarshal(jsonData, g, jsonv2.RejectUnknownMembers(false)); err != nil {
			return fmt.Errorf("read %s: %v", path, err)
		}
		if err := g.resolveRelativePaths(filepath.Dir(path), prev); err != nil {
			return err
		}
	}

	return nil
}

func (g *globalConfig) resolveRelativePaths(dir string, prev *globalConfig) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	baseURL := baseDirectoryURL(dir)

	if prev == nil || g.StoreSocket != prev.StoreSocket {
		g.StoreSocket = resolvePath(dir, g.StoreSocket)
	}
	if prev == nil || g.CacheDB != prev.CacheDB {
		g.CacheDB = resolvePath(dir, g.CacheDB)
	}
	if prev == nil || g.HTTPCacheDB != prev.HTTPCacheDB {
		g.HTTPCacheDB = resolvePath(dir, g.HTTPCacheDB)
	}
	if prev == nil || g.NetrcPath != prev.NetrcPath {
		g.NetrcPath = resolvePath(dir, g.NetrcPath)
	}
	if prev == nil || !g.Server.Download.Equal(prev.Server.Download) {
		g.Server.Download = g.Server.Download.resolve(baseURL)
	}
	if prev == nil || !g.Server.Upload.Equal(prev.Server.Upload) {
		g.Server.Upload = g.Server.Upload.resolve(baseURL)
	}
	if prev == nil || !slices.Equal(g.Server.KeyFiles, prev.Server.KeyFiles) {
		// If the previous slice is a prefix of the current slice,
		// then only resolve the newly added paths.
		toResolve := g.Server.KeyFiles
		var prevKeyFiles []string
		if prev != nil {
			prevKeyFiles = prev.Server.KeyFiles
		}
		if len(g.Server.KeyFiles) > len(prevKeyFiles) &&
			slices.Equal(g.Server.KeyFiles[:len(prevKeyFiles)], prevKeyFiles) {
			toResolve = g.Server.KeyFiles[len(prevKeyFiles):]
		}

		for i, path := range toResolve {
			toResolve[i] = resolvePath(dir, path)
		}
	}
	return nil
}

// UnmarshalJSONFrom unmarshals the configuration object from the JSON decoder,
// merging any fields in the JSON object with existing values.
func (g *globalConfig) UnmarshalJSONFrom(in *jsontext.Decoder) error {
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
		case "debug":
			if err := jsonv2.UnmarshalDecode(in, &g.Debug); err != nil {
				return fmt.Errorf("unmarshal config.debug: %w", err)
			}
		case "storeDirectory":
			if err := jsonv2.UnmarshalDecode(in, &g.Directory); err != nil {
				return fmt.Errorf("unmarshal config.storeDirectory: %w", err)
			}
		case "storeSocket":
			if err := jsonv2.UnmarshalDecode(in, &g.StoreSocket); err != nil {
				return fmt.Errorf("unmarshal config.storeSocket: %w", err)
			}
		case "cacheDB":
			if err := jsonv2.UnmarshalDecode(in, &g.CacheDB); err != nil {
				return fmt.Errorf("unmarshal config.cacheDB: %w", err)
			}
		case "httpCache":
			if err := jsonv2.UnmarshalDecode(in, &g.HTTPCacheDB); err != nil {
				return fmt.Errorf("unmarshal config.httpCache: %w", err)
			}
		case "allowEnvironment":
			if err := jsonv2.UnmarshalDecode(in, &g.AllowEnv); err != nil {
				return fmt.Errorf("unmarshal config.allowEnvironment: %w", err)
			}
		case "trustedPublicKeys":
			// Use any unused capacity at end of the slice.
			newKeys := g.TrustedPublicKeys[len(g.TrustedPublicKeys):]

			if err := jsonv2.UnmarshalDecode(in, &newKeys); err != nil {
				return fmt.Errorf("unmarshal config.trustedPublicKeys: %w", err)
			}
			g.TrustedPublicKeys = append(g.TrustedPublicKeys, newKeys...)
		case "netrcFile":
			if err := jsonv2.UnmarshalDecode(in, &g.NetrcPath); err != nil {
				return fmt.Errorf("unmarshal config.netrcFile: %w", err)
			}
		case "server":
			if err := jsonv2.UnmarshalDecode(in, &g.Server); err != nil {
				return fmt.Errorf("unmarshal config.server: %w", err)
			}
		default:
			if reject, _ := jsonv2.GetOption(in.Options(), jsonv2.RejectUnknownMembers); reject {
				return fmt.Errorf("unmarshal config: unknown field %q", k)
			}
			if err := in.SkipValue(); err != nil {
				return fmt.Errorf("unmarshal config.server: %w", err)
			}
		}
	}
}

// Validate checks the configuration for any missing or semantically incorrect settings.
// Validate should be called after the configuration is complete,
// because partial configurations may not pass validation.
func (g *globalConfig) Validate() error {
	if !filepath.IsAbs(string(g.Directory)) {
		// The directory must be in the format of the local OS.
		return fmt.Errorf("store directory %q is not absolute", g.Directory)
	}
	if g.StoreSocket == "" {
		return fmt.Errorf("ZB_STORE_SOCKET not set")
	}
	if g.CacheDB == "" || g.HTTPCacheDB == "" {
		return fmt.Errorf("cache directory not set")
	}

	return nil
}

func (g *globalConfig) reusePolicy() *zbstorerpc.ReusePolicy {
	if len(g.TrustedPublicKeys) == 0 {
		return &zbstorerpc.ReusePolicy{All: true}
	}
	return &zbstorerpc.ReusePolicy{PublicKeys: g.TrustedPublicKeys}
}

func (g *globalConfig) newHTTPClient() (_ *httpClient, cleanup func(), err error) {
	if err := os.MkdirAll(filepath.Dir(g.HTTPCacheDB), 0o777); err != nil {
		return nil, nil, err
	}
	var netrcData []byte
	if g.NetrcPath == "" {
		var err error
		netrcData, err = os.ReadFile(g.NetrcPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
	}

	baseTransport := &http.Transport{
		// Settings copied from [http.DefaultTransport].
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	baseTransport.RegisterProtocol(althttp.GCSScheme, g.newGCSTransport(baseTransport))
	cache := httpcache.Open(g.HTTPCacheDB, baseTransport, &httpcache.Options{
		MaxResponseSize:         4 << 20, // 4 MiB
		RequestCoalescingCutoff: 5 * time.Second,
		ErrorReporter: httpcache.ErrorReporterFunc(func(ctx context.Context, info *httpcache.RequestInfo, err error) {
			if info != nil {
				log.Warnf(ctx, "HTTP cache failure on %s %v: %v", info.Method, info.URL.Redacted(), err)
			} else {
				log.Warnf(ctx, "HTTP cache error: %v", err)
			}
		}),
	})
	cleanup = func() {
		if err := cache.Close(); err != nil {
			log.Warnf(context.Background(), "%v", err)
		}
		baseTransport.CloseIdleConnections()
	}
	return &httpClient{
		Transport: cache,
		Netrc:     netrcData,
	}, cleanup, nil
}

func (g *globalConfig) newGCSTransport(baseRoundTripper http.RoundTripper) http.RoundTripper {
	adc, detectADCError := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{storage.ScopeReadWrite},
		Client: &http.Client{Transport: baseRoundTripper},
	})
	authHTTPClient, err := httptransport.NewClient(&httptransport.Options{
		BaseRoundTripper:      baseRoundTripper,
		Credentials:           adc,
		DisableAuthentication: detectADCError != nil,
	})
	if err != nil {
		return stubRoundTripper{err}
	}
	gcsClient, err := storage.NewClient(
		context.Background(),
		option.WithHTTPClient(authHTTPClient),
		// TODO(https://github.com/googleapis/google-cloud-go/issues/7786): Remove when fixed.
		storage.WithXMLReads(),
	)
	if err != nil {
		return stubRoundTripper{err}
	}
	return &althttp.GCSTransport{Client: gcsClient}
}

// fileSplitTransport is an [http.RoundTripper]
// that sends "file://" URLs directly to a [fileurl.Transport].
// This allows "file://" URLs to bypass caching
// and other middleware unnecessary for local file access.
type fileSplitTransport struct {
	file     fileurl.Transport
	fallback http.RoundTripper
}

func (t *fileSplitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == fileurl.Scheme {
		var transport fileurl.Transport
		if t != nil {
			transport = t.file
		}
		return transport.RoundTrip(req)
	}
	if t == nil || t.fallback == nil {
		req.Body.Close()
		return nil, http.ErrSkipAltProtocol
	}
	return t.fallback.RoundTrip(req)
}

func (g *globalConfig) openLocalStore(ctx context.Context) *zbstorerpc.Client {
	return zbstorerpc.NewClient(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", g.StoreSocket)
	})
}

type storeConfigType string

const (
	storeConfigNullType storeConfigType = "null"
	storeConfigHTTPType storeConfigType = "http"
)

type storeConfig struct {
	storeType  storeConfigType
	properties jsontext.Value
}

func (sc *storeConfig) isNull() bool {
	return sc == nil || sc.storeType == storeConfigNullType
}

func (sc *storeConfig) Equal(other *storeConfig) bool {
	if sc.isNull() {
		return other.isNull()
	}
	if other.isNull() {
		return false
	}
	if sc.storeType != other.storeType {
		return false
	}
	if bytes.Equal(sc.properties, other.properties) {
		return true
	}
	p1 := sc.properties.Clone()
	if err := p1.Canonicalize(); err != nil {
		return false
	}
	p2 := other.properties.Clone()
	if err := p2.Canonicalize(); err != nil {
		return false
	}
	return bytes.Equal(p1, p2)
}

func (sc *storeConfig) MarshalJSONTo(enc *jsontext.Encoder) error {
	if sc == nil {
		return enc.WriteToken(jsontext.Null)
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("type")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(string(sc.storeType))); err != nil {
		return err
	}
	if len(sc.properties) == 0 {
		return enc.WriteToken(jsontext.EndObject)
	}

	dec := jsontext.NewDecoder(bytes.NewBuffer(sc.properties))
	if first, err := dec.ReadToken(); err != nil {
		return fmt.Errorf("marshal store configuration: unmarshal properties: %v", err)
	} else if first.Kind() != '{' {
		return fmt.Errorf("marshal store configuration: unmarshal properties: not an object")
	}
	for {
		keyToken, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("marshal store configuration: unmarshal properties: %v", err)
		}
		if keyToken.Kind() == '"' && keyToken.String() == "type" {
			return fmt.Errorf("marshal store configuration: unmarshal properties: duplicate \"type\" key")
		}
		if err := enc.WriteToken(keyToken); err != nil {
			return err
		}
		if keyToken.Kind() == '}' {
			return nil
		}

		value, err := dec.ReadValue()
		if err != nil {
			return fmt.Errorf("marshal store configuration: unmarshal %+q property: %v", keyToken, err)
		}
		if err := enc.WriteValue(value); err != nil {
			return fmt.Errorf("marshal store configuration: %w", err)
		}
	}
}

func (sc *storeConfig) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return fmt.Errorf("unmarshal store configuration: %w", err)
		}
		*sc = storeConfig{storeType: storeConfigNullType}
		return nil
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("unmarshal store configuration: %w", err)
		}
		return sc.UnmarshalText([]byte(tok.String()))
	}

	var parsed struct {
		Type       storeConfigType `json:"type"`
		Properties jsontext.Value  `json:",inline"`
	}
	if err := jsonv2.UnmarshalDecode(dec, &parsed); err != nil {
		return fmt.Errorf("unmarshal store configuration: %w", err)
	}
	switch parsed.Type {
	case "":
		return fmt.Errorf("unmarshal store configuration: type not set")
	case storeConfigNullType, storeConfigHTTPType:
	default:
		return fmt.Errorf("unmarshal store configuration: unknown type %+q", parsed.Type)
	}
	*sc = storeConfig{
		storeType:  parsed.Type,
		properties: parsed.Properties,
	}
	return nil
}

func (sc *storeConfig) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*sc = storeConfig{storeType: storeConfigNullType}
		return nil
	}
	if _, err := url.Parse(string(text)); err != nil {
		return fmt.Errorf("unmarshal store configuration string: %v", err)
	}
	props, err := jsonv2.Marshal(storeConfigHTTPProperties{
		URL: string(text),
	})
	if err != nil {
		return fmt.Errorf("unmarshal store configuration string: convert properties: %v", err)
	}
	*sc = storeConfig{
		storeType:  storeConfigHTTPType,
		properties: props,
	}
	return nil
}

type storeWriter interface {
	backend.Store
	backend.Writer
}

func (sc *storeConfig) toStore(provideHTTPClient func() (zbstorehttp.Client, error)) (storeWriter, error) {
	if sc == nil {
		return zbstore.Null{}, nil
	}
	switch sc.storeType {
	case storeConfigNullType:
		return zbstore.Null{}, nil
	case storeConfigHTTPType:
		var props storeConfigHTTPProperties
		if err := jsonv2.Unmarshal(sc.properties, &props); err != nil {
			return nil, fmt.Errorf("unmarshal http store configuration: %v", err)
		}
		client, err := provideHTTPClient()
		if err != nil {
			return nil, err
		}
		store := &zbstorehttp.Store{
			HTTPClient: client,
			CreateTemp: bytebuffer.TempFileCreator{Pattern: contentAddressTempFilePattern},
		}
		store.URL, err = url.Parse(props.URL)
		if err != nil {
			return nil, fmt.Errorf("unmarshal http store configuration: url: %v", err)
		}
		if !store.URL.IsAbs() {
			return nil, fmt.Errorf("unmarshal http store configuration: url: %s is not absolute", store.URL.Redacted())
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unmarshal store configuration: unknown type %q", sc.storeType)
	}
}

// resolve returns a copy of sc with any relative URLs resolved relative to base,
// or returns sc if does not contain relative URLs.
func (sc *storeConfig) resolve(base *url.URL) *storeConfig {
	if sc == nil {
		return nil
	}
	switch sc.storeType {
	case storeConfigHTTPType:
		var props storeConfigHTTPProperties
		if err := jsonv2.Unmarshal(sc.properties, &props); err != nil {
			return sc
		}
		u, err := url.Parse(props.URL)
		if err != nil || u.IsAbs() {
			return sc
		}
		props.URL = xurl.ResolveReference(base, u).String()
		newProps, err := jsonv2.Marshal(props)
		if err != nil {
			return sc
		}
		return &storeConfig{
			storeType:  sc.storeType,
			properties: newProps,
		}
	default:
		sc = new(*sc)
		sc.properties = bytes.Clone(sc.properties)
		return sc
	}
}

// storeConfigHTTPProperties is the set of properties in [storeConfig] for [storeConfigHTTPType].
type storeConfigHTTPProperties struct {
	URL string `json:"url"`
}

type envLookupFunc func(key string) (string, bool)

func (f envLookupFunc) lookup(key string) (string, bool) {
	if f == nil {
		return "", false
	}
	return f(key)
}

func (f envLookupFunc) get(key string) string {
	value, _ := f.lookup(key)
	return value
}

func (f envLookupFunc) tempDir() string {
	if runtime.GOOS == "windows" {
		for _, key := range [...]string{"TMP", "TEMP", "USERPROFILE", "WINDIR"} {
			if dir := f.get(key); dir != "" {
				return dir
			}
		}
		return `C:\WINDOWS`
	}
	if dir := f.get("TMPDIR"); dir != "" {
		return dir
	}
	return "/tmp"
}

// userConfigDirs returns a sequence of configuration directory paths
// in increasing order of preference
// (i.e. later entries should override earlier entries).
func (f envLookupFunc) userConfigDirs() iter.Seq[string] {
	if runtime.GOOS == "windows" {
		return func(yield func(string) bool) {
			if dir := f.get("AppData"); dir != "" {
				yield(dir)
			}
		}
	}
	return func(yield func(string) bool) {
		var dirs []string
		if dirsVar := f.get("XDG_CONFIG_DIRS"); dirsVar != "" {
			dirs = filepath.SplitList(dirsVar)
		} else {
			dirs = []string{"/etc/xdg"}
		}
		for _, dir := range slices.Backward(dirs) {
			if !yield(dir) {
				return
			}
		}
		dir := f.get("XDG_CONFIG_HOME")
		if dir == "" {
			home := f.get("HOME")
			if home == "" {
				return
			}
			dir = filepath.Join(home, ".config")
		}
		yield(dir)
	}
}

func (f envLookupFunc) userCacheDir() (string, error) {
	if runtime.GOOS == "windows" {
		dir := f.get("LocalAppData")
		if dir == "" {
			return "", errors.New("%LocalAppData% is not defined")
		}
		return dir, nil
	}
	dir := f.get("XDG_CACHE_HOME")
	if dir == "" {
		home := f.get("HOME")
		if home == "" {
			return "", errors.New("neither $XDG_CACHE_HOME nor $HOME are defined")
		}
		dir = filepath.Join(home, ".cache")
	}
	return dir, nil
}

func (f envLookupFunc) userHomeDir() (string, error) {
	env, enverr := "HOME", "$HOME"
	switch runtime.GOOS {
	case "windows":
		env, enverr = "USERPROFILE", "%userprofile%"
	case "plan9":
		env, enverr = "home", "$home"
	}
	v := f.get(env)
	if v == "" {
		return "", errors.New(enverr + " is not defined")
	}
	return v, nil
}

// varDir returns "/opt/zb/var" on Unix-like systems or `C:\zb\var` on Windows systems.
func varDir() string {
	return filepath.Join(filepath.Dir(string(zbstore.DefaultDirectory())), "var")
}

// singletonProvider returns a function that calls f at most once
// and a cleanup function that calls any cleanup function returned from f.
func singletonProvider[T any](f func() (T, func(), error)) (provider func() (T, error), cleanup func()) {
	var state struct {
		init    sync.Once
		x       T
		cleanup func()
		err     error
	}
	provider = func() (T, error) {
		state.init.Do(func() {
			state.x, state.cleanup, state.err = f()
		})
		return state.x, state.err
	}
	cleanup = func() {
		state.init.Do(func() {})
		if state.cleanup != nil {
			state.cleanup()
		}
	}
	return provider, cleanup
}
