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
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/tailscale/hujson"
	"zb.256lights.llc/pkg/internal/althttp"
	"zb.256lights.llc/pkg/internal/backend"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/httpcache"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/netrc"
	"zb.256lights.llc/pkg/internal/remotestore"
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
func defaultGlobalConfig() *globalConfig {
	g := &globalConfig{
		Directory:   zbstore.DefaultDirectory(),
		StoreSocket: filepath.Join(defaultVarDir(), "server.sock"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		g.NetrcPath = filepath.Join(home, ".netrc")
	}
	if cd := cacheDir(); cd != "" {
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
	if g.Server.Download != nil {
		g.Server.Download = new(*g.Server.Download)
	}
	return g
}

// mergeEnvironment copies environment variable values to [globalConfig] fields.
func (g *globalConfig) mergeEnvironment() error {
	if dir := os.Getenv("ZB_STORE_DIR"); dir != "" {
		zbDir, err := zbstore.CleanDirectory(dir)
		if err != nil {
			return err
		}
		g.Directory = zbDir
	}

	if path := os.Getenv("ZB_STORE_SOCKET"); path != "" {
		g.StoreSocket = path
	}

	if path := os.Getenv("NETRC"); path != "" {
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
		g.resolveRelativePaths(filepath.Dir(path), prev)
	}

	return nil
}

func (g *globalConfig) resolveRelativePaths(dir string, prev *globalConfig) {
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			return filepath.Join(dir, path)
		}
		return path
	}
	dirToURL := func() *url.URL {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil
		}
		baseURL := fileurl.FromPath(absDir)
		if !strings.HasSuffix(baseURL.Path, "/") {
			baseURL.Path += "/"
		}
		return baseURL
	}

	if prev == nil || g.StoreSocket != prev.StoreSocket {
		g.StoreSocket = resolve(g.StoreSocket)
	}
	if prev == nil || g.CacheDB != prev.CacheDB {
		g.CacheDB = resolve(g.CacheDB)
	}
	if prev == nil || g.HTTPCacheDB != prev.HTTPCacheDB {
		g.HTTPCacheDB = resolve(g.HTTPCacheDB)
	}
	if prev == nil || g.NetrcPath != prev.NetrcPath {
		g.NetrcPath = resolve(g.NetrcPath)
	}
	if prev == nil || !g.Server.Download.Equal(prev.Server.Download) {
		if baseURL := dirToURL(); baseURL != nil {
			g.Server.Download = g.Server.Download.resolve(baseURL)
		}
	}
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

func (g *globalConfig) newHTTPClient() (*http.Client, io.Closer, error) {
	baseTransport := &http.Transport{
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
	if awsDefaultConfig, err := awsconfig.LoadDefaultConfig(context.Background()); err != nil {
		baseTransport.RegisterProtocol(althttp.S3Scheme, &stubRoundTripper{
			message: err.Error(),
		})
	} else {
		baseTransport.RegisterProtocol(althttp.S3Scheme, &althttp.S3Transport{
			Client: s3.NewFromConfig(awsDefaultConfig),
		})
	}

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
	transport := &fileSplitTransport{fallback: cache}
	if g.NetrcPath == "" {
		return &http.Client{Transport: transport}, cache, nil
	}
	netrcData, err := os.ReadFile(g.NetrcPath)
	if errors.Is(err, os.ErrNotExist) {
		return &http.Client{Transport: transport}, cache, nil
	}
	if err != nil {
		cache.Close()
		return nil, nil, fmt.Errorf("open netrc file: %v", err)
	}
	return &http.Client{
		Transport: &netrc.Transport{
			Netrc:        netrcData,
			RoundTripper: cache,
		},
	}, cache, nil
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

func (g *globalConfig) storeClient(opts *zbstorerpc.CodecOptions) *jsonrpc.Client {
	return jsonrpc.NewClient(func(ctx context.Context) (jsonrpc.ClientCodec, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", g.StoreSocket)
		if err != nil {
			return nil, err
		}
		return zbstorerpc.NewCodec(conn, opts), nil
	})
}

func (g *globalConfig) storeDeps() (_ *storeDeps, cleanup func()) {
	var state struct {
		client       *http.Client
		clientCloser io.Closer
	}

	deps := &storeDeps{
		httpClientProvider: func() (*http.Client, error) {
			if state.client == nil {
				var err error
				state.client, state.clientCloser, err = g.newHTTPClient()
				if err != nil {
					return state.client, err
				}
			}
			return state.client, nil
		},
	}
	cleanup = func() {
		if state.client != nil {
			state.client.CloseIdleConnections()
		}
		if state.clientCloser != nil {
			if err := state.clientCloser.Close(); err != nil {
				log.Warnf(context.Background(), "%v", err)
			}
		}
	}
	return deps, cleanup
}

type storeDeps struct {
	httpClientProvider func() (*http.Client, error)
}

type storeConfig struct {
	Type       string         `json:"type"`
	Properties jsontext.Value `json:",inline"`
}

func (sc *storeConfig) isNull() bool {
	return sc == nil || sc.Type == "null"
}

func (sc *storeConfig) Equal(other *storeConfig) bool {
	if sc.isNull() {
		return other.isNull()
	}
	if other.isNull() {
		return false
	}
	if sc.Type != other.Type {
		return false
	}
	if bytes.Equal(sc.Properties, other.Properties) {
		return true
	}
	p1 := sc.Properties.Clone()
	if err := p1.Canonicalize(); err != nil {
		return false
	}
	p2 := other.Properties.Clone()
	if err := p2.Canonicalize(); err != nil {
		return false
	}
	return bytes.Equal(p1, p2)
}

func (sc *storeConfig) toStore(deps *storeDeps) (backend.Store, error) {
	if sc == nil {
		return zbstore.Null{}, nil
	}
	switch sc.Type {
	case "null":
		return zbstore.Null{}, nil
	case "http":
		var props storeConfigHTTPProperties
		if err := jsonv2.Unmarshal(sc.Properties, &props); err != nil {
			return nil, fmt.Errorf("unmarshal http store configuration: %v", err)
		}
		client, err := deps.httpClientProvider()
		if err != nil {
			return nil, err
		}
		store := &remotestore.HTTPStore{
			HTTPClient: client,
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
		return nil, fmt.Errorf("unmarshal store configuration: unknown type %q", sc.Type)
	}
}

// resolve returns a copy of sc with any relative URLs resolved relative to base,
// or returns sc if does not contain relative URLs.
func (sc *storeConfig) resolve(base *url.URL) *storeConfig {
	if sc == nil {
		return nil
	}
	switch sc.Type {
	case "http":
		var props storeConfigHTTPProperties
		if err := jsonv2.Unmarshal(sc.Properties, &props); err != nil {
			return sc
		}
		u, err := url.Parse(props.URL)
		if err != nil || u.IsAbs() {
			return sc
		}
		props.URL = base.ResolveReference(u).String()
		newProps, err := jsonv2.Marshal(props)
		if err != nil {
			return sc
		}
		return &storeConfig{
			Type:       sc.Type,
			Properties: newProps,
		}
	default:
		return sc
	}
}

// storeConfigHTTPProperties is the set of properties in [storeConfig] for the "http" type.
type storeConfigHTTPProperties struct {
	URL string `json:"url"`
}

type stubRoundTripper struct {
	message string
}

func (stub *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	message := stub.message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return &http.Response{
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		StatusCode:    http.StatusInternalServerError,
		Status:        http.StatusText(http.StatusInternalServerError),
		ContentLength: int64(len(message)),
		Header: http.Header{
			"Content-Type":           {"text/plain; charset=utf-8"},
			"X-Content-Type-Options": {"nosniff"},
			"Content-Length":         {strconv.Itoa(len(message))},
			"Date":                   {time.Now().UTC().Format(http.TimeFormat)},
		},
		Body: io.NopCloser(strings.NewReader(message)),
	}, nil
}

// defaultVarDir returns "/opt/zb/var/zb" on Unix-like systems or `C:\zb\var\zb` on Windows systems.
func defaultVarDir() string {
	return filepath.Join(filepath.Dir(string(zbstore.DefaultDirectory())), "var", "zb")
}
