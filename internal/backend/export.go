// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	jsonv2 "github.com/go-json-experiment/json"
	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix/nar"
)

func (s *Server) export(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	args := new(zbstorerpc.ExportRequest)
	if err := jsonv2.Unmarshal(req.Params, args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}

	// Conceptually, same as a [zbstore.Copy],
	// but we only want to return an error if [*Server.StoreExport] returns an error.
	// [zbstore.Copy] also checks for existence, which isn't an error for an export.
	pr, pw := io.Pipe()
	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		err := s.StoreExport(ctx, pw, sets.Collect(slices.Values(args.Paths)), &zbstore.ExportOptions{
			ExcludeReferences: args.ExcludeReferences,
		})
		pw.CloseWithError(err)
		return err
	})
	grp.Go(func() error {
		err := zbstorerpc.ServeExport(ctx, pr)
		pr.CloseWithError(err)
		// Closing the pipe will cause the write to fail,
		// and we want that error to come through.
		return nil
	})
	return nil, grp.Wait()
}

type dbStore struct {
	dir     zbstore.Directory
	realDir string
	db      connectionGetter
}

func (s *Server) dbStore() *dbStore {
	return &dbStore{
		dir:     s.dir,
		realDir: s.realDir,
		db:      s.db,
	}
}

func (store *dbStore) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	objects, err := store.ObjectBatch(ctx, sets.New(path))
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", path, err)
	}
	i := slices.IndexFunc(objects, func(object zbstore.Object) bool {
		return object.Info().StorePath == path
	})
	if i == -1 {
		return nil, fmt.Errorf("get %s: %w", path, zbstore.ErrNotFound)
	}
	return objects[i], nil
}

func (store *dbStore) ObjectBatch(ctx context.Context, paths sets.Set[zbstore.Path]) ([]zbstore.Object, error) {
	n := 0
	for path := range paths.All() {
		if path.Dir() == store.dir {
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}

	conn, err := store.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer store.db.Put(conn)

	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, err
	}
	defer rollback()

	result := make([]zbstore.Object, 0, n)
	for path := range paths.All() {
		if path.Dir() != store.dir {
			continue
		}
		info, err := pathInfo(conn, path)
		switch {
		case err == nil:
			result = append(result, &filesystemObject{
				path: filepath.Join(store.realDir, path.Base()),
				info: *info,
			})
		case !errors.Is(err, zbstore.ErrNotFound):
			return nil, err
		}
	}
	return result, nil
}

// closure returns the store objects that are transitively referenced by the given paths.
// The list is in topological order,
// so each store object in the list will only reference itself
// or store objects that come before it in the list.
func (store *dbStore) closure(ctx context.Context, paths sets.Set[zbstore.Path]) ([]zbstore.Object, error) {
	hasPathsInDir := false
	for path := range paths.All() {
		if path.Dir() == store.dir {
			hasPathsInDir = true
			break
		}
	}
	if !hasPathsInDir {
		return nil, nil
	}

	conn, err := store.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer store.db.Put(conn)

	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, err
	}
	defer rollback()

	var result []zbstore.Object
	hasPath := func(s []zbstore.Object, path zbstore.Path) bool {
		return slices.ContainsFunc(s, func(obj zbstore.Object) bool {
			return obj.Info().StorePath == path
		})
	}
	for path := range paths {
		if path.Dir() != store.dir {
			continue
		}

		var infoError error
		err := closurePaths(conn, pathAndEquivalenceClass{path: path}, func(pe pathAndEquivalenceClass) bool {
			if hasPath(result, pe.path) {
				return true
			}
			var info *zbstore.ObjectInfo
			info, infoError = pathInfo(conn, pe.path)
			if infoError != nil {
				// Even a "not found" error here is bad,
				// because it means we have an inconsistent store.
				return false
			}
			result = append(result, &filesystemObject{
				path: filepath.Join(store.realDir, pe.path.Base()),
				info: *info,
			})
			return true
		})
		if infoError != nil {
			return nil, infoError
		}
		if err != nil && !errors.Is(err, zbstore.ErrNotFound) {
			return nil, err
		}
	}

	// Topologically sort new closure.
	err = sortByReferences(
		result,
		func(t zbstore.Object) zbstore.Path { return t.Info().StorePath },
		func(t zbstore.Object) sets.Sorted[zbstore.Path] { return t.Info().References },
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("closure of %s missing referenced objects", paths)
	}

	return result, nil
}

// Export exports the store objects according to the request
// in `nix-store --export` format to dst.
func (store *dbStore) StoreExport(ctx context.Context, dst io.Writer, paths sets.Set[zbstore.Path], opts *zbstore.ExportOptions) error {
	var got []zbstore.Object
	var err error
	if opts != nil && opts.ExcludeReferences {
		got, err = store.ObjectBatch(ctx, paths)
	} else {
		got, err = store.closure(ctx, paths)
	}
	if err != nil {
		return fmt.Errorf("export %s: %v", joinStrings(paths.All(), ", "), err)
	}

	e := zbstore.NewExportWriter(dst)
	for _, object := range got {
		if err := e.WriteObject(ctx, object); err != nil {
			return err
		}
	}
	if err := e.Close(); err != nil {
		return fmt.Errorf("export %s: %v", joinStrings(paths.All(), ", "), err)
	}

	return nil
}

type filesystemObject struct {
	path string
	info zbstore.ObjectInfo
}

func (obj *filesystemObject) Info() *zbstore.ObjectInfo {
	return &obj.info
}

func (obj *filesystemObject) WriteNAR(ctx context.Context, w io.Writer) error {
	return nar.DumpPath(w, obj.path)
}
