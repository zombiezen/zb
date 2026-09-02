// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/osutil"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func (eval *Eval) pathFunction(ctx context.Context, l *lua.State) (nResults int, err error) {
	var p string
	var pcontext sets.Set[string]
	var name string
	var filterFuncIndex int
	switch l.Type(1) {
	case lua.TypeString:
		p, _ = l.ToString(1)
		pcontext = l.StringContext(1)
	case lua.TypeTable:
		typ, err := l.Field(ctx, 1, "path")
		if err != nil {
			return 0, fmt.Errorf("path: %v", err)
		}
		if typ == lua.TypeNil {
			return 0, lua.NewArgError(l, 1, "missing path")
		}
		p, pcontext, err = lua.ToString(ctx, l, -1)
		if err != nil {
			return 0, fmt.Errorf("path: %v", err)
		}
		l.Pop(1)

		typ, err = l.Field(ctx, 1, "name")
		if err != nil {
			return 0, fmt.Errorf("path: %v", err)
		}
		if typ != lua.TypeNil {
			name, _, _ = lua.ToString(ctx, l, -1)
		}
		l.Pop(1)

		typ, err = l.Field(ctx, 1, "filter")
		if err != nil {
			return 0, fmt.Errorf("path: %v", err)
		}
		if typ != lua.TypeNil {
			filterFuncIndex = l.Top()
		}
	default:
		return 0, lua.NewTypeError(l, 1, "string or table")
	}

	p, err = absSourcePath(l, eval.workDir, eval.storeDir, p, pcontext)
	if err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}
	if name == "" {
		name = filepath.Base(p)
	}

	cache, err := eval.cachePool.Get(ctx)
	if err != nil {
		return 0, err
	}
	defer eval.cachePool.Put(cache)

	var filterFunc func(name string, typ fs.FileMode) (bool, error)
	if filterFuncIndex != 0 {
		filterFunc = func(name string, typ fs.FileMode) (bool, error) {
			defer l.SetTop(l.Top())
			l.PushValue(filterFuncIndex)
			l.PushString(name)
			switch typ.Type() {
			case 0:
				l.PushString("regular")
			case fs.ModeDir:
				l.PushString("directory")
			case fs.ModeSymlink:
				l.PushString("symlink")
			default:
				return false, fmt.Errorf("internal error: unsupported file type %v", typ)
			}
			if err := l.Call(ctx, 2, 1); err != nil {
				return false, err
			}
			return l.ToBoolean(-1), nil
		}
	}
	if err := walkPath(ctx, cache, p, filterFunc); err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}
	defer func() {
		sqlitex.ExecuteScriptFS(cache, sqlFiles(), "walk/drop.sql", nil)
		// TODO(soon): Log error.
	}()

	// If we already imported and it exists in the store, don't do an import.
	if prevStorePath, err := eval.checkStamp(cache, p, name); err != nil {
		log.Debugf(ctx, "%v", err)
	} else {
		if _, err := eval.store.Object(ctx, prevStorePath); err != nil {
			log.Debugf(ctx, "%v", err)
		} else {
			log.Debugf(ctx, "Using existing store path %s", prevStorePath)
			pushStorePath(l, prevStorePath)
			return 1, nil
		}
	}

	exporter, closeExport, err := startExport(ctx, eval.store, eval.exportTemp)
	if err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}
	defer closeExport(false)

	pr, pw := io.Pipe()
	caChan := make(chan nix.ContentAddress)
	go func() {
		defer close(caChan)
		ca, _, _ := zbstore.SourceSHA256ContentAddress(pr, nil)
		caChan <- ca
	}()

	w := nar.NewWriter(io.MultiWriter(pw, exporter))
	err = sqlitex.ExecuteTransientFS(cache, sqlFiles(), "walk/iterate.sql", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			fpath := stmt.GetText("path")
			var subpath string
			if fpath != p {
				var ok bool
				subpath, ok = strings.CutPrefix(fpath, p+string(filepath.Separator))
				if !ok {
					return fmt.Errorf("can't make %s relative to %s", fpath, p)
				}
				subpath = filepath.ToSlash(subpath)
			}

			mode := fs.FileMode(stmt.GetInt64("mode"))

			switch mode.Type() {
			case fs.ModeDir:
				err := w.WriteHeader(&nar.Header{
					Path: subpath,
					Mode: fs.ModeDir | 0o777,
				})
				if err != nil {
					return err
				}
			case fs.ModeSymlink:
				err := w.WriteHeader(&nar.Header{
					Path:       subpath,
					Mode:       fs.ModeSymlink | 0o777,
					LinkTarget: stmt.GetText("link_target"),
				})
				if err != nil {
					return err
				}
			default:
				size := stmt.GetInt64("size")
				err := w.WriteHeader(&nar.Header{
					Path: subpath,
					Mode: mode.Perm(),
					Size: size,
				})
				if err != nil {
					return err
				}
				// TODO(#44): Use store to open file if pathInStore(path, dir).
				f, err := os.Open(fpath)
				if err != nil {
					return err
				}
				defer f.Close()

				n, err := io.Copy(w, f)
				if err != nil {
					return err
				}
				if n != size {
					return fmt.Errorf("%s changed size during import", fpath)
				}
			}

			return nil
		},
	})
	if err != nil {
		pw.CloseWithError(err)
		<-caChan
		return 0, fmt.Errorf("path: %v", err)
	}
	if err := w.Close(); err != nil {
		pw.CloseWithError(err)
		<-caChan
		return 0, fmt.Errorf("path: %v", err)
	}

	pw.Close()
	ca := <-caChan

	storePath, err := zbstore.FixedCAOutputPath(eval.storeDir, name, ca, zbstore.References{})
	if err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}
	err = exporter.Trailer(&zbstore.ExportTrailer{
		StorePath:      storePath,
		ContentAddress: ca,
	})
	if err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}
	if err := closeExport(true); err != nil {
		return 0, fmt.Errorf("path: %v", err)
	}

	err = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(cache)
		if err != nil {
			return err
		}
		defer endFn(&err)
		return updateCache(cache, storePath)
	}()
	if err != nil {
		return 0, fmt.Errorf("path: updating cache: %v", err)
	}

	pushStorePath(l, storePath)
	return 1, nil
}

func (eval *Eval) readFileFunction(ctx context.Context, l *lua.State) (int, error) {
	path, err := lua.CheckString(l, 1)
	if err != nil {
		return 0, err
	}
	pcontext := l.StringContext(1)

	absPath, err := absSourcePathWithDeps(ctx, l, eval, path, pcontext)
	if err != nil {
		return 0, fmt.Errorf("readFile: %v", err)
	}

	content, err := osutil.ReadFileString(absPath)
	if err != nil {
		return 0, fmt.Errorf("readFile: reading file: %v", err)
	}

	l.PushString(content)
	return 1, nil
}

func (eval *Eval) toFileFunction(ctx context.Context, l *lua.State) (int, error) {
	name, err := lua.CheckString(l, 1)
	if err != nil {
		return 0, err
	}
	s, err := lua.CheckString(l, 2)
	if err != nil {
		return 0, err
	}

	possibleRefs := new(sets.Sorted[zbstore.Path])
	for dep := range l.StringContext(2) {
		c, err := parseContextString(dep)
		if err != nil {
			return 0, fmt.Errorf("internal error: %v", err)
		}
		if c.path == "" {
			return 0, fmt.Errorf("toFile %q: cannot depend on derivation outputs", name)
		}
		possibleRefs.Add(c.path)
	}
	blob, err := zbstore.NewTextBlob(eval.storeDir, name, []byte(s), possibleRefs)
	if err != nil {
		return 0, fmt.Errorf("toFile %q: %v", name, err)
	}

	if err := eval.store.WriteObject(ctx, blob); err != nil {
		return 0, fmt.Errorf("toFile %q: %v", name, err)
	}

	pushStorePath(l, blob.StorePath)
	return 1, nil
}

// absSourcePath takes a source path passed as an argument from Lua to Go
// and resolves it relative to the calling function.
func absSourcePath(l *lua.State, workDir string, dir zbstore.Directory, path string, context sets.Set[string]) (string, error) {
	// Lua guarantees that a call to a native function will never be a tail call,
	// so we can always get information about the immediate caller.
	debugInfo := l.Info(1)
	if debugInfo == nil {
		return "", fmt.Errorf("resolve path: no caller information available")
	}
	source, hasSource := debugInfo.Source.Filename()
	sourceInStore := hasSource && pathInStore(source, dir)

	if filepath.IsAbs(path) {
		if sourceInStore && !pathInStore(path, dir) {
			return "", fmt.Errorf("resolve path: cannot refer to paths outside store")
		}
		return path, nil
	}
	if strings.HasPrefix(path, "/") {
		// We may be specifying a store path via a placeholder, possibly with a trailing child path.
		// Such placeholders have a leading slash, followed by a long hash digest.
		// On non-POSIX systems, such a path might not be recognized as an absolute path.
		// We treat this as an absolute path while preserving the placeholder.
		for dep := range context {
			c, err := parseContextString(dep)
			if err != nil {
				return "", fmt.Errorf("resolve path: internal error: %v", err)
			}
			if c.outputReference.IsZero() {
				continue
			}
			placeholder := zbstore.UnknownCAOutputPlaceholder(c.outputReference)
			slashTail, match := strings.CutPrefix(path, placeholder)
			if !match {
				continue
			}
			tail := filepath.FromSlash(slashTail)
			if tail == slashTail {
				// We can just return the original string.
				// Reduces allocations.
				return path, nil
			}
			return placeholder + filepath.FromSlash(tail), nil
		}
	}

	if !hasSource {
		// Not loaded from a file. Use working directory.
		//
		// TODO(someday): This is intended for --expr evaluation,
		// but would take place for any chunk loaded with the "load" built-in.
		// Perhaps an allow-list of sources?
		path := filepath.Join(workDir, filepath.FromSlash(path))
		if sourceInStore && !pathInStore(path, dir) {
			return "", fmt.Errorf("resolve path: cannot refer to paths outside %s", dir)
		}
		return path, nil
	}

	path = filepath.Join(filepath.Dir(source), filepath.FromSlash(path))
	if sourceInStore && !pathInStore(path, dir) {
		return "", fmt.Errorf("resolve path: cannot refer to paths outside %s", dir)
	}
	return path, nil
}

// absSourcePathWithDeps takes a source path passed as an argument from Lua to Go
// and resolves it relative to the calling function, taking into account
// any dependencies the string may have.
func absSourcePathWithDeps(ctx context.Context, l *lua.State, eval *Eval, filename string, filenameContext sets.Set[string]) (path string, err error) {
	// TODO(someday): If we have dependencies and we're using a non-local store,
	// export the store object and read it.
	toRealize := make(sets.Set[zbstore.OutputReference])
	placeholders := make(map[string]zbstore.OutputReference)
	for dep := range filenameContext {
		c, err := parseContextString(dep)
		if err != nil {
			return "", fmt.Errorf("internal error: %w", err)
		}
		if c.outputReference.IsZero() {
			continue
		}
		placeholder := zbstore.UnknownCAOutputPlaceholder(c.outputReference)
		if !strings.Contains(filename, placeholder) {
			continue
		}
		toRealize.Add(c.outputReference)
		placeholders[placeholder] = c.outputReference
	}
	if toRealize.Len() > 0 {
		results, err := eval.store.Realize(ctx, toRealize)
		if err != nil {
			return "", err
		}

		var rewrites []string
		for placeholder, outputReference := range placeholders {
			outputPath, err := zbstorerpc.FindRealizeOutput(slices.Values(results), outputReference)
			if err != nil {
				return "", err
			}
			if !outputPath.Valid || outputPath.X == "" {
				return "", fmt.Errorf("realize %v: build failed", outputReference)
			}
			rewrites = append(rewrites, placeholder, string(outputPath.X))
		}
		filename = strings.NewReplacer(rewrites...).Replace(filename)
	}

	return absSourcePath(l, eval.workDir, eval.storeDir, filename, filenameContext)
}

func pathInStore(path string, dir zbstore.Directory) bool {
	return strings.HasPrefix(path, string(dir)) &&
		(len(path) <= len(dir) || path[len(dir)] == byte(filepath.Separator))
}

// checkStamp returns the store path of a previous import,
// if the cache still matches the metadata of the files on disk.
// path must be a cleaned, absolute path.
// name is the intended name of the store object.
// [walkPath] must be called before calling checkStamp.
func (eval *Eval) checkStamp(cache *sqlite.Conn, path, name string) (_ zbstore.Path, err error) {
	var found zbstore.Path
	err = sqlitex.ExecuteTransientFS(cache, sqlFiles(), "find.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":name": name,
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			p, err := zbstore.ParsePath(stmt.GetText("path"))
			if err != nil || p.Dir() != eval.storeDir {
				// Skip.
				return nil
			}
			if found != "" {
				return fmt.Errorf("multiple store paths found for %s (hash collision): %s and %s", path, found, p)
			}
			found = p
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("check stamp for %s: find match: %v", path, err)
	}
	if found == "" {
		return "", fmt.Errorf("check stamp for %s: no match", path)
	}
	return found, nil
}

// walkPath creates a temporary table on the connection called "curr"
// and inserts the paths and their stamps into the table.
// walkPath only operates on the TEMP schema.
func walkPath(ctx context.Context, conn *sqlite.Conn, path string, filter func(name string, typ fs.FileMode) (bool, error)) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("walk %s: %v", path, err)
		}
	}()

	rootInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}

	defer sqlitex.Save(conn)(&err)

	err = sqlitex.ExecuteScriptFS(conn, sqlFiles(), "walk/create.sql", nil)
	if err != nil {
		return fmt.Errorf("create temp table: %v", err)
	}
	insertStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "walk/insert.sql")
	if err != nil {
		return err
	}
	defer insertStmt.Finalize()
	stampAndInsert := func(path string, info fs.FileInfo) error {
		entryStamp, err := stamp(path, info)
		if err != nil {
			return err
		}

		insertStmt.SetText(":path", path)
		insertStmt.SetInt64(":mode", int64(info.Mode()))
		if info.Mode().IsRegular() {
			insertStmt.SetInt64(":size", info.Size())
		} else {
			insertStmt.SetInt64(":size", -1)
		}
		insertStmt.SetText(":stamp", entryStamp)
		log.Debugf(ctx, "walk %s stamp=%s", path, entryStamp)
		_, insertError := insertStmt.Step()
		insertStmt.ClearBindings()
		resetError := insertStmt.Reset()
		if insertError != nil {
			return insertError
		}
		if resetError != nil {
			return resetError
		}
		return nil
	}

	if rootInfo.IsDir() {
		rootPath := path
		err = filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			isDescendant := len(path) > len(rootPath)
			if !strings.HasPrefix(path, rootPath) ||
				isDescendant && path[len(rootPath)] != filepath.Separator {
				return fmt.Errorf("internal error: %s is not prefixed by %s", path, rootPath)
			}
			if isDescendant && filter != nil {
				pathArg := filepath.ToSlash(path[len(rootPath)+1:])
				entryType := entry.Type()
				keep, err := filter(pathArg, entryType)
				if err != nil {
					return fmt.Errorf("filter %s: %v", path, err)
				}
				if !keep {
					if entryType.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
			}

			info, err := entry.Info()
			if err != nil {
				return err
			}
			return stampAndInsert(path, info)
		})
		if err != nil {
			return err
		}
	} else {
		if err := stampAndInsert(path, rootInfo); err != nil {
			return err
		}
	}

	return nil
}

func updateCache(conn *sqlite.Conn, storePath zbstore.Path) (err error) {
	defer sqlitex.Save(conn)(&err)

	err = sqlitex.ExecuteScriptFS(conn, sqlFiles(), "invalidate.sql", nil)
	if err != nil {
		return fmt.Errorf("update cache for %s: %v", storePath, err)
	}

	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "upsert_store_object.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":path": storePath,
		},
	})
	if err != nil {
		return fmt.Errorf("update cache for %s: %v", storePath, err)
	}

	var mappingID int64
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "new_mapping.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":path": storePath,
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			mappingID = stmt.GetInt64("mapping_id")
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("update cache for %s: %v", storePath, err)
	}

	err = sqlitex.ExecuteScriptFS(conn, sqlFiles(), "copy_walk.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":mapping_id": mappingID,
		},
	})
	if err != nil {
		return fmt.Errorf("update cache for %s: %v", storePath, err)
	}

	return nil
}

// collatePath compares two operating-system-native path strings,
// returning -1 if a < b,
// returning 1 if a > b,
// or returning 0 if a == b.
func collatePath(a, b string) int {
	a = filepath.Clean(a)
	b = filepath.Clean(b)

	for i := 0; i < len(a) && i < len(b); i++ {
		switch aSep, bSep := os.IsPathSeparator(a[i]), os.IsPathSeparator(b[i]); {
		case aSep && !bSep:
			return -1
		case !aSep && bSep:
			return 1
		case !aSep && !bSep && a[i] != b[i]:
			if a[i] < b[i] {
				return -1
			} else {
				return 1
			}
		}
	}

	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

func startExport(ctx context.Context, store zbstore.ObjectWriter, createTemp bytebuffer.Creator) (exporter *zbstore.ExportWriter, closeFunc func(ok bool) error, err error) {
	pr, pw := io.Pipe()
	done := make(chan error)
	go func() {
		err := (&zbstore.BufferedImporter{
			ObjectWriter:  store,
			BufferCreator: createTemp,
		}).StoreImport(ctx, pr)
		pr.Close()
		done <- err
		close(done)
	}()

	exporter = zbstore.NewExportWriter(pw)
	var once sync.Once
	closeFunc = func(ok bool) error {
		var errs [2]error
		errs[0] = errors.New("already closed")

		once.Do(func() {
			if ok {
				errs[0] = exporter.Close()
				pw.CloseWithError(errs[0])
			} else {
				errs[0] = nil
				pw.CloseWithError(errors.New("export interrupted"))
			}
			errs[1] = <-done
		})

		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}
	return exporter, closeFunc, nil
}

func stamp(path string, info fs.FileInfo) (string, error) {
	if info.Mode().Type() == os.ModeSymlink {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return "link:" + target, nil
	}
	return stampFileInfo(info), nil
}

func stampFileInfo(info fs.FileInfo) string {
	if info.IsDir() {
		// Directories change too much; detect only existence.
		return "dir"
	}
	modTime := info.ModTime()
	uid, gid := owner(info)
	return fmt.Sprintf("%d.%06d-%d-%d-%d-%d-%d",
		modTime.Unix(),
		modTime.UTC().Nanosecond()/1000,
		info.Size(),
		inode(info),
		info.Mode(),
		uid,
		gid,
	)
}
