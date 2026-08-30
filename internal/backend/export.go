// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	jsonv2 "github.com/go-json-experiment/json"
	"golang.org/x/sync/errgroup"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix/nar"
)

// Export exports the store objects according to the request
// in `nix-store --export` format to dst.
func (s *Server) Export(ctx context.Context, dst io.Writer, req *zbstorerpc.ExportRequest) error {
	e := zbstore.NewExportWriter(dst)

	var manifest []*zbstore.ExportTrailer
	var err error
	if req.ExcludeReferences {
		manifest, err = s.fetchInfoForExport(ctx, req.Paths)
	} else {
		manifest, err = s.findExportClosure(ctx, req.Paths)
	}
	if err != nil {
		return fmt.Errorf("export %s: %v", joinStrings(req.Paths, ", "), err)
	}

	for _, object := range manifest {
		if err := nar.DumpPath(e, s.realPath(object.StorePath)); err != nil {
			return fmt.Errorf("export %s: %v", object.StorePath, err)
		}
		if err := e.Trailer(object); err != nil {
			return fmt.Errorf("export %s: %v", object.StorePath, err)
		}
	}
	if err := e.Close(); err != nil {
		return fmt.Errorf("export %s: %v", joinStrings(req.Paths, ", "), err)
	}

	return nil
}

func (s *Server) export(ctx context.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	args := new(zbstorerpc.ExportRequest)
	if err := jsonv2.Unmarshal(req.Params, args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}

	pr, pw := io.Pipe()
	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		err := s.Export(ctx, pw, args)
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

// fetchInfoForExport generates export trailers for the given paths.
func (s *Server) fetchInfoForExport(ctx context.Context, paths []zbstore.Path) ([]*zbstore.ExportTrailer, error) {
	if len(paths) == 0 {
		return nil, nil
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

	var result []*zbstore.ExportTrailer
	for _, path := range paths {
		info, err := pathInfo(conn, path)
		if err == nil {
			result = append(result, info.ExportTrailer())
		} else if err != nil && !errors.Is(err, zbstore.ErrNotFound) {
			return nil, err
		}
	}
	return result, nil
}

// findExportClosure returns a list of export trailers
// for all the store objects that are transitively referenced by the given paths.
// The list is in topological order,
// so each store object in the list will only reference itself
// or store objects that come before it in the list.
func (s *Server) findExportClosure(ctx context.Context, paths []zbstore.Path) ([]*zbstore.ExportTrailer, error) {
	if len(paths) == 0 {
		return nil, nil
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

	var result []*zbstore.ExportTrailer
	hasPath := func(s []*zbstore.ExportTrailer, path zbstore.Path) bool {
		return slices.ContainsFunc(s, func(t *zbstore.ExportTrailer) bool {
			return t.StorePath == path
		})
	}
	for _, path := range paths {
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
			result = append(result, info.ExportTrailer())
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
		func(t *zbstore.ExportTrailer) zbstore.Path { return t.StorePath },
		func(t *zbstore.ExportTrailer) sets.Sorted[zbstore.Path] { return t.References },
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("closure of %s missing referenced objects", paths)
	}

	return result, nil
}
