// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	slashpath "path"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/internal/fileurl"
	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/xhttp"
	"zb.256lights.llc/pkg/internal/xio"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// URLs imports the Lua file for each URL,
// and uses the fragment from each URL (see [parseFragment])
// to determine the Lua value to convert to outputs.
// Output groups are 1:1 with the urls slice.
func (eval *Eval) URLs(ctx context.Context, urls []string, sys system.System) (OutputMap, error) {
	if len(urls) == 0 {
		return OutputMap{}, nil
	}

	// Parse URLs first before doing any expensive operations.
	parsedURLs := make([]*url.URL, len(urls))
	for i, s := range urls {
		u, err := ParseURL(s)
		if err != nil {
			return OutputMap{}, err
		}
		archiveEntry, _, err := parseFragment(u.Fragment)
		if err != nil {
			return OutputMap{}, fmt.Errorf("%s: %v", s, err)
		}
		if u.Scheme == "" || u.Scheme == fileurl.Scheme {
			if _, err := URLToPath(u); err != nil {
				return OutputMap{}, err
			}
			if archiveEntry != "" {
				return OutputMap{}, fmt.Errorf("%s: archive path not valid for local file", s)
			}
		}
		parsedURLs[i] = u
	}

	// Download and import any URLs.
	grp, grpCtx := errgroup.WithContext(ctx)
	grp.SetLimit(2)
	var mu sync.Mutex
	importedStorePaths := make(map[string]zbstore.Path, len(parsedURLs))
	for _, u := range parsedURLs {
		if u.Scheme == "" || u.Scheme == fileurl.Scheme {
			continue
		}
		key := stripFragment(u).String()
		mu.Lock()
		if _, ok := importedStorePaths[key]; ok {
			mu.Unlock()
			continue
		}
		importedStorePaths[key] = ""
		mu.Unlock()

		grp.Go(func() error {
			path, err := eval.importURL(grpCtx, u)
			if err != nil {
				return err
			}

			mu.Lock()
			importedStorePaths[key] = path
			mu.Unlock()
			return nil
		})
	}
	if err := grp.Wait(); err != nil {
		return OutputMap{}, err
	}

	// Start imports. These will run concurrently.
	l, err := eval.newState()
	if err != nil {
		return OutputMap{}, err
	}
	defer l.Close()
	l.CreateTable(len(parsedURLs), 0)
	tableStackIndex := l.Top()
	if _, err := l.Global(ctx, "import"); err != nil {
		return OutputMap{}, fmt.Errorf("internal error: _G.import: %v", err)
	}
	importStackIndex := l.Top()
	if _, err := l.Global(ctx, "extract"); err != nil {
		return OutputMap{}, fmt.Errorf("internal error: _G.extract: %v", err)
	}
	extractStackIndex := l.Top()
	for i, u := range parsedURLs {
		l.PushValue(importStackIndex)
		if u.Scheme == "" || u.Scheme == fileurl.Scheme {
			path, err := URLToPath(u)
			if err != nil {
				// Should have already been verified above.
				return OutputMap{}, fmt.Errorf("internal error: %v", err)
			}
			l.PushString(path)
		} else {
			storePath := importedStorePaths[stripFragment(u).String()]
			l.PushStringContext(string(storePath), sets.New(contextValue{path: storePath}.String()))
			if archiveFile, _, _ := parseFragment(u.Fragment); archiveFile != "" {
				// Call extract{src=storePath}.
				l.CreateTable(0, 1)
				l.Insert(-2)
				if err := l.RawSetField(-2, "src"); err != nil {
					return OutputMap{}, fmt.Errorf("internal error: {src=%s}: %v", lualex.Quote(string(storePath)), err)
				}
				l.PushValue(extractStackIndex)
				l.Insert(-2)
				if err := l.PCall(ctx, 1, 1, 0); err != nil {
					return OutputMap{}, fmt.Errorf("extract{src=%s}: %v", lualex.Quote(string(storePath)), err)
				}
				l.PushString("/" + archiveFile)
				if err := l.Concat(ctx, 2); err != nil {
					return OutputMap{}, fmt.Errorf("internal error: concat extract{...}..%s: %v",
						lualex.Quote(archiveFile), err)
				}
			}
		}
		if err := l.PCall(ctx, 1, 1, 0); err != nil {
			return OutputMap{}, err
		}
		l.RawSetIndex(tableStackIndex, int64(i+1))
	}

	// Perform lookups on each import.
	l.PushClosure(0, messageHandler)
	messageHandlerIndex := l.Top()

	var result OutputMap
	sysString := SystemTriple(sys)
	l.PushClosure(0, func(ctx context.Context, l *lua.State) (int, error) {
		l.SetTop(2)
		fieldPath, _ := l.ToString(2)
		l.Pop(1)
		if fieldPath != "" {
			if err := searchKeyPath(ctx, l, fieldPath); err != nil {
				return 0, err
			}
			l.Remove(-2)
		}
		l.PushString(sysString)
		urlOutputs, err := objectOutputs(ctx, l, 1)
		if err != nil {
			return 0, err
		}
		result.groups = append(result.groups, urlOutputs)
		return 0, nil
	})

	for i, u := range parsedURLs {
		l.PushValue(-1)
		l.RawIndex(tableStackIndex, int64(i+1))
		_, fieldPath, _ := parseFragment(u.Fragment)
		l.PushString(fieldPath)
		if err := l.PCall(ctx, 2, 0, messageHandlerIndex); err != nil {
			return OutputMap{}, fmt.Errorf("%s: %v", u.Redacted(), err)
		}
	}
	return result, nil
}

func (eval *Eval) importURL(ctx context.Context, u *url.URL) (zbstore.Path, error) {
	u = stripFragment(u)
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
		Header: http.Header{
			"Accept": {"text/plain, application/gzip, application/x-bzip2, application/zip, application/x-tar;q=0.9"},
		},
	}
	resp, err := eval.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("download %v: %v", u, err)
	}
	respCloser := xio.CloseOnce(resp.Body)
	defer respCloser.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %v: %v", u, xhttp.ErrorFromResponse(resp))
	}

	// If the server provides a Content-Length header,
	// we can stream the download directly to the store.
	name := inferDownloadName(slashpath.Base(u.Path))
	if resp.ContentLength >= 0 {
		path, err := eval.importFlatFile(ctx, name, resp.ContentLength, resp.Body)
		if err != nil {
			return "", fmt.Errorf("download %v: %v", u, err)
		}
		return path, nil
	}

	// Otherwise, download to a temporary file and then ingest.
	f, err := eval.downloadTemp.CreateBuffer(-1)
	if err != nil {
		return "", fmt.Errorf("download %v: %v", u, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Debugf(ctx, "Closing temp file: %v", err)
		}
	}()
	size, err := io.Copy(f, resp.Body)
	respCloser.Close()
	if err != nil {
		return "", fmt.Errorf("download %v: %v", u, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("download %v: %v", u, err)
	}
	path, err := eval.importFlatFile(ctx, name, size, f)
	if err != nil {
		return "", fmt.Errorf("download %v: %v", u, err)
	}
	return path, nil
}

func (eval *Eval) importFlatFile(ctx context.Context, name string, size int64, f io.Reader) (zbstore.Path, error) {
	exporter, closeExport, err := startExport(ctx, eval.store, eval.exportTemp)
	if err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	defer closeExport(false)
	nw := nar.NewWriter(exporter)
	if err := nw.WriteHeader(&nar.Header{Size: size}); err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	h := nix.NewHasher(nix.SHA256)
	if _, err := io.CopyN(io.MultiWriter(h, nw), f, size); err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	if err := nw.Close(); err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	ca := nix.FlatFileContentAddress(h.SumHash())
	path, err := zbstore.FixedCAOutputPath(eval.storeDir, name, ca, zbstore.References{})
	if err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	err = exporter.Trailer(&zbstore.ExportTrailer{
		StorePath:      path,
		ContentAddress: ca,
	})
	if err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	if err := closeExport(true); err != nil {
		return "", fmt.Errorf("import %s: %v", name, err)
	}
	return path, nil
}

// searchKeyPath pushes the value at the slash-separated field path onto the stack.
func searchKeyPath(ctx context.Context, l *lua.State, fieldPath string) error {
	if !l.CheckStack(3) {
		return errors.New("internal error: lua stack overflow")
	}
	l.PushClosure(0, followKeyPath)
	l.PushValue(-2)
	l.PushString(fieldPath)
	if err := l.Call(ctx, 2, 1); err != nil {
		return err
	}
	if l.Type(-1) == lua.TypeNil {
		log.Debugf(ctx, "%s not found", fieldPath)
	} else {
		log.Debugf(ctx, "Found non-nil at %s", fieldPath)
	}
	return nil
}

// followKeyPath is a [lua.Function] that accesses a slash-separated field path
// and returns the value.
// If a nil is encountered along the way, nil is returned.
// The first argument to followKeyPath is the root object
// and the second argument is the string containing the slash-separated field path.
func followKeyPath(ctx context.Context, l *lua.State) (int, error) {
	fieldPath, err := lua.CheckString(l, 2)
	if err != nil {
		return 0, err
	}
	l.SetTop(1)

	lastType := l.Type(-1)
	for k := range splitKeyPath(fieldPath) {
		if lastType == lua.TypeNil {
			return 1, nil
		}
		var err error
		lastType, err = l.Field(ctx, -1, k)
		if err != nil {
			return 0, err
		}
		l.Remove(-2)
	}
	return 1, nil
}

// ParseURL parses a URL, but permits some amount of sloppiness for Windows paths.
func ParseURL(s string) (*url.URL, error) {
	return fileurl.Parse(s)
}

// URLToPath returns the filesystem path represented by u.
// URLToPath returns an error if u is not a "file:" URL or a path.
func URLToPath(u *url.URL) (string, error) {
	return fileurl.ToPath(u)
}

// parseFragment parses an unescaped fragment string (excluding the "#")
// and splits it at the last colon (":") into an archive member path
// and a slash-separated key path.
// parseFragment returns an error if the keyPath has leading, trailing, or repeated dots.
func parseFragment(s string) (archivePath, keyPath string, err error) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		archivePath = s[:i]
		keyPath = s[i+1:]
	} else {
		keyPath = s
	}

	if keyPath == "" {
		return archivePath, keyPath, nil
	}
	for k := range splitKeyPath(keyPath) {
		if k == "" {
			return archivePath, keyPath, fmt.Errorf("invalid fragment %v: empty key in %s", &url.URL{Fragment: s}, keyPath)
		}
	}
	return archivePath, keyPath, nil
}

// splitKeyPath splits a slash-separated path into its components.
// A run of slashes is treated the same as a single slash.
// An empty string yields no elements.
func splitKeyPath(s string) iter.Seq[string] {
	if s == "" {
		return func(yield func(string) bool) {}
	}
	return func(yield func(string) bool) {
		for i := 0; ; {
			n := strings.IndexByte(s[i:], '/')
			if n < 0 {
				yield(s[i:])
				return
			}

			if !yield(s[i : i+n]) {
				return
			}
			i = i + n
			for i < len(s) && s[i] == '/' {
				i++
			}
		}
	}
}

// inferDownloadName converts baseName into a store-object-appropriate name.
func inferDownloadName(baseName string) string {
	hasNameChars := false
	hasSpecialChars := false
	for _, c := range baseName {
		if c <= 0x7f && isNameChar(byte(c)) {
			hasNameChars = true
		} else {
			hasSpecialChars = true
		}
	}
	if !hasNameChars {
		return "x"
	}
	if !hasSpecialChars {
		return baseName
	}

	sb := new(strings.Builder)
	sb.Grow(len(baseName))
	for _, c := range baseName {
		if c <= 0x7f && isNameChar(byte(c)) {
			hasNameChars = true
			sb.WriteByte(byte(c))
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func isNameChar(c byte) bool {
	return 'a' <= c && c <= 'z' ||
		'A' <= c && c <= 'Z' ||
		'0' <= c && c <= '9' ||
		c == '+' || c == '-' || c == '.' || c == '_' || c == '='
}
