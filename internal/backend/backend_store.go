// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"github.com/google/uuid"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
	"zombiezen.com/go/xcontext"
)

/*
This file contains querying and manipulating functions
for the store directory and the store database.
*/

// readDerivationClosure reads the given derivations from the store
// and the transitive closure of derivations those derivations depend on.
func (s *Server) readDerivationClosure(ctx context.Context, drvPaths []zbstore.Path) (map[zbstore.Path]*zbstore.Derivation, error) {
	stack := slices.Clone(drvPaths)
	result := make(map[zbstore.Path]*zbstore.Derivation)
	for len(stack) > 0 {
		curr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if result[curr] != nil {
			continue
		}
		drv, err := s.readDerivation(ctx, curr)
		if err != nil {
			return nil, err
		}
		result[curr] = drv
		for inputDrvPath := range drv.InputDerivations {
			stack = append(stack, inputDrvPath)
		}
	}

	// Walk through closure to ensure that every named output exists.
	for drvPath, drv := range result {
		for ref := range drv.InputDerivationOutputs() {
			if !result[ref.DrvPath].Outputs.Has(ref.OutputName) {
				return result, fmt.Errorf("derivation %s depends on non-existent output %v", drvPath, ref)
			}
		}
	}

	return result, nil
}

// readDerivation reads a derivation file from the store
// and validates that it fits the constraints that this backend imposes on derivations.
// As a side effect, if readDerivation succeeds,
// callers can assume that all inputs are present in the store without acquiring the writing lock.
func (s *Server) readDerivation(ctx context.Context, drvPath zbstore.Path) (*zbstore.Derivation, error) {
	drvName, isDrv := drvPath.DerivationName()
	if !isDrv {
		return nil, fmt.Errorf("read derivation %s: not a %s file", drvPath, zbstore.DerivationExt)
	}
	log.Debugf(ctx, "Waiting for lock on %s to read derivation...", drvPath)
	unlock, err := s.writing.lock(ctx, drvPath)
	if err != nil {
		return nil, fmt.Errorf("read derivation %s: waiting for lock: %w", drvPath, err)
	}
	defer unlock()
	log.Debugf(ctx, "Reading derivation %s (lock acquired)", drvPath)
	realDrvPath := s.realPath(drvPath)
	if info, err := os.Lstat(realDrvPath); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read derivation %s: not a regular file", drvPath)
	}
	drvData, err := os.ReadFile(realDrvPath)
	if err != nil {
		return nil, fmt.Errorf("read derivation %s: %v", drvPath, err)
	}
	drv, err := zbstore.ParseDerivation(s.dir, drvName, drvData)
	if err != nil {
		return nil, fmt.Errorf("read derivation %s: %v", drvPath, err)
	}
	return drv, nil
}

func findPossibleRealizations(ctx context.Context, conn *sqlite.Conn, eqClass equivalenceClass, reuse *zbstorerpc.ReusePolicy) (presentInStore, absentFromStore sets.Set[zbstore.Path], err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("find existing realizations for %v: %v", eqClass, err)
		}
	}()

	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, nil, err
	}
	defer rollback()

	dropTrustedPublicKeys, err := createTrustedPublicKeysTable(conn, reuse.PublicKeys)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err := dropTrustedPublicKeys(); err != nil {
			log.Warnf(ctx, "%v", err)
		}
	}()

	drvHash := eqClass.drvHashKey.toHash()
	presentInStore = make(sets.Set[zbstore.Path])
	absentFromStore = make(sets.Set[zbstore.Path])
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "realizations/find.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":drv_hash_algorithm": drvHash.Type().String(),
			":drv_hash_bits":      drvHash.Bytes(nil),
			":output_name":        eqClass.outputName.Value(),
			":trust_all":          reuse.All,
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rawPath := stmt.GetText("output_path")
			outPath, err := zbstore.ParsePath(rawPath)
			if err != nil {
				log.Warnf(ctx, "Database contains realization with invalid path %q for %v (%v)", rawPath, eqClass, err)
				return nil
			}
			if stmt.GetBool("present_in_store") {
				presentInStore.Add(outPath)
			} else {
				absentFromStore.Add(outPath)
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return presentInStore, absentFromStore, nil
}

func createTrustedPublicKeysTable(conn *sqlite.Conn, keys []*zbstore.RealizationPublicKey) (dropTable func() error, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("create trusted public keys table: %w", err)
		}
	}()
	defer sqlitex.Save(conn)(&err)

	err = sqlitex.ExecuteScriptFS(conn, sqlFiles(), "realizations/create_trusted_public_keys.sql", nil)
	if err != nil {
		return nil, err
	}
	stmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "realizations/insert_trusted_public_key.sql")
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()

	for _, k := range keys {
		stmt.SetText(":format", string(k.Format))
		stmt.SetBytes(":public_key", k.Data)

		if _, err := stmt.Step(); err != nil {
			return nil, err
		}
		if err := stmt.Reset(); err != nil {
			return nil, err
		}
	}

	return func() error {
		err := sqlitex.ExecuteScriptFS(conn, sqlFiles(), "realizations/drop_trusted_public_keys.sql", nil)
		if err != nil {
			return fmt.Errorf("internal error: clean up temporary trusted public keys table: %v", err)
		}
		return nil
	}, nil
}

func recordRealizations(conn *sqlite.Conn, realizations iter.Seq2[zbstore.RealizationOutputReference, *zbstore.Realization]) (err error) {
	defer sqlitex.Save(conn)(&err)

	realizationStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "realizations/insert.sql")
	if err != nil {
		return err
	}
	defer realizationStmt.Finalize()
	refClassStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "realizations/insert_reference_class.sql")
	if err != nil {
		return err
	}
	defer refClassStmt.Finalize()
	publicKeyStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "realizations/insert_public_key.sql")
	if err != nil {
		return err
	}
	defer publicKeyStmt.Finalize()
	signatureStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "realizations/insert_signature.sql")
	if err != nil {
		return err
	}
	defer signatureStmt.Finalize()

	for ref, realization := range realizations {
		if err := upsertDrvHash(conn, ref.DerivationHash); err != nil {
			return fmt.Errorf("record realization for %v: %v", ref, err)
		}
		if err := upsertPath(conn, realization.OutputPath); err != nil {
			return fmt.Errorf("record realization for %v: %v", ref, err)
		}

		drvHashAlgorithm := ref.DerivationHash.Type().String()
		drvHashBits := ref.DerivationHash.Bytes(nil)

		realizationStmt.SetText(":drv_hash_algorithm", drvHashAlgorithm)
		realizationStmt.SetBytes(":drv_hash_bits", drvHashBits)
		realizationStmt.SetText(":output_name", ref.OutputName)
		realizationStmt.SetText(":output_path", string(realization.OutputPath))
		if _, err := realizationStmt.Step(); err != nil {
			return fmt.Errorf("record realization for %v: %v", ref, err)
		}
		if err := realizationStmt.Reset(); err != nil {
			return fmt.Errorf("record realization for %v: %v", ref, err)
		}

		refClassStmt.SetText(":referrer_drv_hash_algorithm", drvHashAlgorithm)
		refClassStmt.SetBytes(":referrer_drv_hash_bits", drvHashBits)
		refClassStmt.SetText(":referrer_path", string(realization.OutputPath))
		refClassStmt.SetText(":referrer_output_name", ref.OutputName)
		for _, rc := range realization.ReferenceClasses {
			if err := upsertPath(conn, rc.Path); err != nil {
				return fmt.Errorf("record realizations for %v: reference %s: %v", ref, rc.Path, err)
			}

			refClassStmt.SetText(":reference_path", string(rc.Path))
			if rc.Realization.IsZero() {
				refClassStmt.SetNull(":reference_drv_hash_algorithm")
				refClassStmt.SetNull(":reference_drv_hash_bits")
				refClassStmt.SetNull(":reference_output_name")
			} else {
				h := rc.Realization.DerivationHash
				if err := upsertDrvHash(conn, h); err != nil {
					return fmt.Errorf("record realization for %v: reference %s: %v", ref, rc.Path, err)
				}

				refClassStmt.SetText(":reference_drv_hash_algorithm", h.Type().String())
				refClassStmt.SetBytes(":reference_drv_hash_bits", h.Bytes(nil))
				refClassStmt.SetText(":reference_output_name", rc.Realization.OutputName)
			}

			if _, err := refClassStmt.Step(); err != nil {
				return fmt.Errorf("record realization for %v: reference %s: %v", ref, rc.Path, err)
			}
			if err := refClassStmt.Reset(); err != nil {
				return fmt.Errorf("record realization for %v: reference %s: %v", ref, rc.Path, err)
			}
		}

		for _, sig := range realization.Signatures {
			publicKeyStmt.SetText(":format", string(sig.PublicKey.Format))
			publicKeyStmt.SetBytes(":public_key", sig.PublicKey.Data)
			if _, err := publicKeyStmt.Step(); err != nil {
				return fmt.Errorf("record realization for %v: %v", ref, err)
			}
			if err := publicKeyStmt.Reset(); err != nil {
				return fmt.Errorf("record realization for %v: %v", ref, err)
			}

			signatureStmt.SetText(":drv_hash_algorithm", drvHashAlgorithm)
			signatureStmt.SetBytes(":drv_hash_bits", drvHashBits)
			signatureStmt.SetText(":output_name", ref.OutputName)
			signatureStmt.SetText(":output_path", string(realization.OutputPath))
			signatureStmt.SetText(":format", string(sig.PublicKey.Format))
			signatureStmt.SetBytes(":public_key", sig.PublicKey.Data)
			signatureStmt.SetBytes(":signature", sig.Signature)
			if _, err := signatureStmt.Step(); err != nil {
				return fmt.Errorf("record realization for %v: %v", ref, err)
			}
			if err := signatureStmt.Reset(); err != nil {
				return fmt.Errorf("record realization for %v: %v", ref, err)
			}
		}
	}

	return nil
}

// pathInfo returns basic information about an object in the store.
// If there is no such object in the database,
// then pathInfo returns an error that wraps [zbstore.ErrNotFound].
func pathInfo(conn *sqlite.Conn, path zbstore.Path) (_ *zbstore.ObjectInfo, err error) {
	defer sqlitex.Save(conn)(&err)

	var info *zbstore.ObjectInfo
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "info.sql", &sqlitex.ExecOptions{
		Named: map[string]any{":path": string(path)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			info = new(zbstore.ObjectInfo)
			info.StorePath = path
			info.NARSize = stmt.GetInt64("nar_size")
			var err error
			info.NARHash, err = nix.ParseHash(stmt.GetText("nar_hash"))
			if err != nil {
				return err
			}
			info.ContentAddress, err = nix.ParseContentAddress(stmt.GetText("ca"))
			if err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("path info for %s: %v", path, err)
	}
	if info == nil {
		return nil, fmt.Errorf("path info for %s: %w", path, zbstore.ErrNotFound)
	}

	info.References, err = listReferences(conn, path)
	if err != nil {
		return nil, fmt.Errorf("path info for %s: %v", path, err)
	}

	return info, nil
}

func listReferences(conn *sqlite.Conn, path zbstore.Path) (sets.Sorted[zbstore.Path], error) {
	var references sets.Sorted[zbstore.Path]
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "references.sql", &sqlitex.ExecOptions{
		Named: map[string]any{":path": string(path)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := zbstore.ParsePath(stmt.GetText("path"))
			if err != nil {
				return err
			}
			references.Add(ref)
			return nil
		},
	})
	if err != nil {
		return sets.Sorted[zbstore.Path]{}, fmt.Errorf("references for %s: %v", path, err)
	}
	return references, nil
}

// closurePaths finds all store paths that the given path transitively refers to
// and calls the yield function with each path,
// including the original path itself.
// If an equivalence class is given,
// then any given path may have zero or more non-zero equivalence classes associated with it,
// indicating which equivalence class produced the path
// during evaluation of the given equivalence class.
// If closurePaths does not return an error,
// closurePaths is guaranteed to have called yield at least once.
// If the path is not found in the database,
// then closurePaths returns an error that wraps [zbstore.ErrNotFound].
//
// closurePaths uses information from both the references table and the reference classes table.
// closurePaths may return an incomplete closure for paths that don't exist on the disk.
func closurePaths(conn *sqlite.Conn, pe pathAndEquivalenceClass, yield func(pathAndEquivalenceClass) bool) error {
	errStop := errors.New("stop iteration")

	args := map[string]any{
		":path":               string(pe.path),
		":drv_hash_algorithm": nil,
		":drv_hash_bits":      nil,
		":output_name":        nil,
	}
	if !pe.equivalenceClass.isZero() {
		h := pe.equivalenceClass.drvHashKey.toHash()
		args[":drv_hash_algorithm"] = h.Type().String()
		args[":drv_hash_bits"] = h.Bytes(nil)
		args[":output_name"] = pe.equivalenceClass.outputName.Value()
	}

	dir := pe.path.Dir()
	calledYield := false
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "closure.sql", &sqlitex.ExecOptions{
		Named: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rawPath := stmt.GetText("path")
			var row pathAndEquivalenceClass
			var sub string
			var err error
			row.path, sub, err = dir.ParsePath(rawPath)
			if err != nil {
				return fmt.Errorf("path: %v", err)
			}
			if sub != "" {
				return fmt.Errorf("path %s: must not contain a sub-path", rawPath)
			}
			if hashTypeName := stmt.GetText("drv_hash_algorithm"); hashTypeName != "" {
				ht, err := nix.ParseHashType(hashTypeName)
				if err != nil {
					return fmt.Errorf("path %s: derivation hash: %v", row.path, err)
				}
				bitsLength := stmt.GetLen("drv_hash_bits")
				if bitsLength != ht.Size() {
					return fmt.Errorf("path %s: derivation hash: incorrect size for %v (found %d instead of %d)",
						row.path, ht, bitsLength, ht.Size())
				}
				bits := make([]byte, bitsLength)
				stmt.GetBytes("drv_hash_bits", bits)
				outputName := stmt.GetText("output_name")
				if outputName != "" && !zbstore.IsValidOutputName(outputName) {
					return fmt.Errorf("path %s: output name %q is not valid", row.path, outputName)
				}
				row.equivalenceClass = newEquivalenceClass(nix.NewHash(ht, bits), outputName)
			}
			calledYield = true
			if !yield(row) {
				return errStop
			}
			return nil
		},
	})
	if err != nil && !errors.Is(err, errStop) {
		return fmt.Errorf("find closure of %s: %v", pe.path, err)
	}
	if !calledYield {
		return fmt.Errorf("find closure of %s: %w", pe.path, zbstore.ErrNotFound)
	}
	return nil
}

// sortByReferences sorts the slice into dependency order.
func sortByReferences[T any](slice []T, pathFunc func(T) zbstore.Path, refsFunc func(T) sets.Sorted[zbstore.Path], ignoreUnknown bool) error {
	sortStatus := make(map[zbstore.Path]bool, len(slice))
	for _, v := range slice {
		sortStatus[pathFunc(v)] = false
	}

	for sortEnd := range slice {
		unsorted := slice[sortEnd:]
		i := slices.IndexFunc(unsorted, func(v T) bool {
			p := pathFunc(v)
			r := refsFunc(v)
			for ref := range r.Values() {
				if ref == p {
					continue
				}
				if isSorted, known := sortStatus[ref]; !isSorted && (known || !ignoreUnknown) {
					return false
				}
			}
			return true
		})
		if i == -1 {
			return errors.New("impossible dependency sort")
		}

		// Move object to front of unsorted slice.
		unsorted[0], unsorted[i] = unsorted[i], unsorted[0]
		sortStatus[pathFunc(unsorted[0])] = true
	}
	return nil
}

// objectExists checks for the existence of a store object in the store database.
func objectExists(conn *sqlite.Conn, path zbstore.Path) (bool, error) {
	var exists bool
	err := sqlitex.ExecuteFS(conn, sqlFiles(), "object_exists.sql", &sqlitex.ExecOptions{
		Named: map[string]any{":path": string(path)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			exists = stmt.ColumnBool(0)
			return nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("check existence of %s: %v", path, err)
	}
	return exists, nil
}

func insertObject(ctx context.Context, conn *sqlite.Conn, info *zbstore.ObjectInfo) (err error) {
	log.Debugf(ctx, "Registering metadata for %s", info.StorePath)

	defer sqlitex.Save(conn)(&err)

	if err := upsertPath(conn, zbstore.Path(info.StorePath)); err != nil {
		return fmt.Errorf("insert %s into database: %v", info.StorePath, err)
	}
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "insert_object.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":path":     string(info.StorePath),
			":nar_size": info.NARSize,
			":nar_hash": info.NARHash.SRI(),
			":ca":       info.ContentAddress.String(),
		},
	})
	if sqlite.ErrCode(err) == sqlite.ResultConstraintRowID {
		return fmt.Errorf("insert %s into database: store object exists", info.StorePath)
	}
	if err != nil {
		return fmt.Errorf("insert %s into database: %v", info.StorePath, err)
	}

	addRefStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "add_reference.sql")
	if err != nil {
		return fmt.Errorf("insert %s into database: %v", info.StorePath, err)
	}
	defer addRefStmt.Finalize()

	addRefStmt.SetText(":referrer", string(info.StorePath))
	for ref := range info.References.Values() {
		if err := upsertPath(conn, ref); err != nil {
			return fmt.Errorf("insert %s into database: %v", info.StorePath, err)
		}
		addRefStmt.SetText(":reference", string(ref))
		if _, err := addRefStmt.Step(); err != nil {
			return fmt.Errorf("insert %s into database: add reference %s: %v", info.StorePath, ref, err)
		}
		if err := addRefStmt.Reset(); err != nil {
			return fmt.Errorf("insert %s into database: add reference %s: %v", info.StorePath, ref, err)
		}
	}

	return nil
}

func upsertPath(conn *sqlite.Conn, path zbstore.Path) error {
	if path == "" {
		return nil
	}
	err := sqlitex.ExecuteFS(conn, sqlFiles(), "upsert_path.sql", &sqlitex.ExecOptions{
		Named: map[string]any{":path": string(path)},
	})
	if err != nil {
		return fmt.Errorf("upsert path %s: %v", path, err)
	}
	return nil
}

func upsertDrvHash(conn *sqlite.Conn, h nix.Hash) error {
	if h.IsZero() {
		return nil
	}
	err := sqlitex.ExecuteFS(conn, sqlFiles(), "upsert_drv_hash.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":algorithm": h.Type().String(),
			":bits":      h.Bytes(nil),
		},
	})
	if err != nil {
		return fmt.Errorf("upsert derivation hash %v: %v", h, err)
	}
	return nil
}

func recordBuildStart(conn *sqlite.Conn, buildID uuid.UUID) error {
	now := time.Now()
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/start.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id":         buildID.String(),
			":timestamp_millis": now.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("create new build record for %s: %v", buildID, err)
	}
	return nil
}

func recordBuildEnd(conn *sqlite.Conn, buildID uuid.UUID, buildError error) error {
	now := time.Now()
	var buildErrorArg any = nil
	if buildError != nil && !errors.Is(buildError, errUnfinishedRealization) {
		buildErrorArg = buildError.Error()
	}
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/end.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id":         buildID.String(),
			":build_error":      buildErrorArg,
			":timestamp_millis": now.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("record build end for %s: %v", buildID, err)
	}
	return nil
}

type deleteOldBuildOptions struct {
	logDir string
	keep   iter.Seq[uuid.UUID]
}

// deleteOldBuilds deletes builds that occurred before the time cutoff.
// Any build IDs yielded by the keep sequence will be retained.
func deleteOldBuilds(ctx context.Context, conn *sqlite.Conn, cutoff time.Time, opts *deleteOldBuildOptions) (numDeleted int64, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("delete old builds: %v", err)
		}
	}()
	defer sqlitex.Save(conn)(&err)

	err = sqlitex.ExecuteTransient(conn, `create temp table "active_builds" ("uuid" blob not null unique);`, nil)
	if err != nil {
		return 0, err
	}
	if opts != nil && opts.keep != nil {
		stmt, _, err := conn.PrepareTransient(`insert into temp."active_builds" values (?);`)
		if err != nil {
			return 0, err
		}
		defer stmt.Finalize()
		for id := range opts.keep {
			stmt.BindBytes(1, id[:])
			var stmtErrors [2]error
			_, stmtErrors[0] = stmt.Step()
			stmtErrors[1] = stmt.Reset()
			for _, err := range stmtErrors {
				if err != nil {
					return 0, err
				}
			}
		}
	}

	var n int64
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/gc.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":cutoff_millis": cutoff.UnixMilli(),
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n++
			if opts == nil || opts.logDir == "" {
				return nil
			}

			buildID, err := uuid.Parse(stmt.GetText("id"))
			if err != nil {
				log.Debugf(ctx, "Not deleting logs for build with invalid ID %q", buildID)
				return nil
			}
			if err := os.RemoveAll(buildLogRoot(opts.logDir, buildID)); err != nil {
				log.Warnf(ctx, "Failed to clean up logs for build %v: %v", buildID, err)
			}
			return nil
		},
	})
	if err != nil {
		return 0, err
	}
	err = sqlitex.ExecuteTransient(conn, `drop table temp."active_builds";`, nil)
	if err != nil {
		return n, err
	}
	return n, nil
}

func recordExpandResult(conn *sqlite.Conn, buildID uuid.UUID, result *zbstorerpc.ExpandResult) error {
	argsJSON := "[]"
	if len(result.Args) > 0 {
		var err error
		argsJSON, err = marshalJSONString(result.Args)
		if err != nil {
			return fmt.Errorf("record build end for %s: %v", buildID, err)
		}
	}
	envJSON := "{}"
	if len(result.Env) > 0 {
		var err error
		envJSON, err = marshalJSONString(result.Env)
		if err != nil {
			return fmt.Errorf("record build end for %s: %v", buildID, err)
		}
	}
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/set_extract.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id": buildID.String(),
			":builder":  result.Builder,
			":args":     argsJSON,
			":env":      envJSON,
		},
	})
	if err != nil {
		return fmt.Errorf("record build end for %s: %v", buildID, err)
	}
	return nil
}

func insertBuildResult(conn *sqlite.Conn, buildID uuid.UUID, drvPath zbstore.Path, drvHash nix.Hash, t time.Time) (buildResultID int64, err error) {
	defer sqlitex.Save(conn)(&err)
	if err := upsertPath(conn, drvPath); err != nil {
		return -1, fmt.Errorf("record build result for %s in %v: %v", drvPath, buildID, err)
	}
	if err := upsertDrvHash(conn, drvHash); err != nil {
		return -1, fmt.Errorf("record build result for %s in %v: %v", drvPath, buildID, err)
	}
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/insert_result.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id":           buildID.String(),
			":drv_path":           string(drvPath),
			":drv_hash_algorithm": drvHash.Type().String(),
			":drv_hash_bits":      drvHash.Bytes(nil),
			":timestamp_millis":   t.UnixMilli(),
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			buildResultID = stmt.ColumnInt64(0)
			return nil
		},
	})
	if err != nil {
		return -1, fmt.Errorf("record build result for %s in %v: %v", drvPath, buildID, err)
	}
	return buildResultID, nil
}

// findBuildResults appends the build results in the build with the given ID to dst.
// If drvPath is not empty, then only the result for drvPath is appended (if it exists).
// If logDir is not empty, then the LogSize field in [*zbstorerpc.BuildResult] will be populated.
func findBuildResults(dst []*zbstorerpc.BuildResult, conn *sqlite.Conn, logDir string, buildID uuid.UUID, drvPath zbstore.Path) (_ []*zbstorerpc.BuildResult, err error) {
	defer sqlitex.Save(conn)(&err)

	signatureStmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "build/result_signatures.sql")
	if err != nil {
		return dst, fmt.Errorf("list build results for %v: %v", buildID, err)
	}
	defer signatureStmt.Finalize()
	initDstLen := len(dst)
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/results.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":build_id": buildID.String(),
			":drv_path": string(drvPath),
		},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			drvPath, err := zbstore.ParsePath(stmt.GetText("drv_path"))
			if err != nil {
				return err
			}
			var curr *zbstorerpc.BuildResult
			if len(dst) > initDstLen && dst[len(dst)-1].DrvPath == drvPath {
				curr = dst[len(dst)-1]
			} else {
				var drvHash nix.Hash
				if algo := stmt.GetText("drv_hash_algorithm"); algo != "" {
					buf := make([]byte, stmt.GetLen("drv_hash_bits"))
					stmt.GetBytes("drv_hash_bits", buf)
					var err error
					drvHash, err = unmarshalHash(algo, buf)
					if err != nil {
						return err
					}
				}

				curr = &zbstorerpc.BuildResult{
					DrvPath: drvPath,
					DrvHash: zbstorerpc.NonNull(drvHash),
					Status:  zbstorerpc.BuildStatus(stmt.GetText("status")),
					Outputs: []*zbstorerpc.RealizeOutput{},
				}
				if logDir != "" {
					logInfo, err := os.Stat(builderLogPath(logDir, buildID, drvPath))
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("stat log for %s: %v", drvPath, err)
					}
					if err == nil {
						curr.LogSize = logInfo.Size()
					}
				}
				dst = append(dst, curr)
			}

			if outputName := stmt.GetText("output_name"); outputName != "" {
				newOutput := &zbstorerpc.RealizeOutput{
					Name: outputName,
				}
				if s := stmt.GetText("output_path"); s != "" {
					p, err := zbstore.ParsePath(s)
					if err != nil {
						return fmt.Errorf("output %s: %v", outputName, err)
					}
					newOutput.Path = zbstorerpc.NonNull(p)

					newOutput.Signatures, err = signaturesForRealization(signatureStmt, buildID, drvPath, outputName, p)
					if err != nil {
						// signaturesForRealization includes the outputName in the error message,
						// so no need to additionally wrap.
						return err
					}
				}
				curr.Outputs = append(curr.Outputs, newOutput)
			}

			return nil
		},
	})
	if err != nil {
		if drvPath == "" {
			return dst, fmt.Errorf("list build results for %v: %v", buildID, err)
		}
		return dst, fmt.Errorf("fetch build result for %s in build %v: %v", drvPath, buildID, err)
	}
	return dst, nil
}

// signaturesForRealization fetches the list of signatures stored for the given realization.
func signaturesForRealization(stmt *sqlite.Stmt, buildID uuid.UUID, drvPath zbstore.Path, outputName string, outputPath zbstore.Path) ([]*zbstore.RealizationSignature, error) {
	var result []*zbstore.RealizationSignature
	stmt.SetText(":build_id", buildID.String())
	stmt.SetText(":drv_path", string(drvPath))
	stmt.SetText(":output_name", outputName)
	stmt.SetText(":output_path", string(outputPath))

	for {
		hasRow, err := stmt.Step()
		if err != nil {
			_ = stmt.Reset()
			return nil, fmt.Errorf("output %s: signatures: %v", outputName, err)
		}
		if !hasRow {
			break
		}

		buf := make([]byte, stmt.GetLen("public_key")+stmt.GetLen("signature"))
		newSignature := &zbstore.RealizationSignature{
			PublicKey: zbstore.RealizationPublicKey{
				Format: zbstore.RealizationSignatureFormat(stmt.GetText("format")),
			},
		}
		n := stmt.GetBytes("public_key", buf)
		newSignature.PublicKey.Data = buf[:n:n]
		buf = buf[n:]
		n = stmt.GetBytes("signature", buf)
		newSignature.Signature = buf[:n:n]

		result = append(result, newSignature)
	}
	if err := stmt.Reset(); err != nil {
		return result, fmt.Errorf("signatures: %v", err)
	}
	return result, nil
}

func recordBuilderStart(conn *sqlite.Conn, buildResultID int64, t time.Time) error {
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/set_builder_start.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":id":               buildResultID,
			":timestamp_millis": t.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("record builder start: %v", err)
	}
	return nil
}

// setBuildResultOutputs sets the outputs for the build result with the given ID.
// If a path is empty, then the output's path will be null.
func setBuildResultOutputs(conn *sqlite.Conn, buildResultID int64, outputs iter.Seq2[string, zbstore.Path]) (err error) {
	defer sqlitex.Save(conn)(&err)

	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/clear_outputs.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":id": buildResultID,
		},
	})
	if err != nil {
		return fmt.Errorf("record build result outputs: %v", err)
	}

	stmt, err := sqlitex.PrepareTransientFS(conn, sqlFiles(), "build/insert_output.sql")
	if err != nil {
		return fmt.Errorf("record build result outputs: %v", err)
	}
	defer stmt.Finalize()

	stmt.SetInt64(":id", buildResultID)
	for outputName, outputPath := range outputs {
		if outputPath != "" {
			if err := upsertPath(conn, outputPath); err != nil {
				return fmt.Errorf("record build result %s -> %s: %v", outputName, outputPath, err)
			}
		}

		stmt.SetText(":output_name", outputName)
		stmt.SetText(":output_path", string(outputPath))
		var execErrors [2]error
		_, execErrors[0] = stmt.Step()
		execErrors[1] = stmt.Reset()
		for _, err := range execErrors {
			if err != nil {
				return fmt.Errorf("record build result %s -> %s: %v", outputName, outputPath, err)
			}
		}
	}

	return nil
}

func recordBuilderEnd(conn *sqlite.Conn, buildResultID int64, t time.Time) error {
	err := sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/set_builder_end.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":id":               buildResultID,
			":timestamp_millis": t.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("record builder end: %v", err)
	}
	return nil
}

type buildFinalResults struct {
	buildID uuid.UUID
	drvPath zbstore.Path
	id      int64
	endTime time.Time
	error   error
}

func finalizeBuildResult(ctx context.Context, conn *sqlite.Conn, logDir string, result *buildFinalResults) (err error) {
	// If build is being cancelled, allow some amount of time to write.
	ctx, cancel := xcontext.KeepAlive(ctx, 30*time.Second)
	defer cancel()
	oldDone := conn.SetInterrupt(ctx.Done())
	defer conn.SetInterrupt(oldDone)

	defer sqlitex.Save(conn)(&err)

	status := zbstorerpc.BuildSuccess
	if result.error != nil {
		if isBuilderFailure(result.error) {
			status = zbstorerpc.BuildFail
		} else {
			status = zbstorerpc.BuildError
			var buf []byte
			buf = append(buf, "zb internal error: "...)
			buf = append(buf, result.error.Error()...)
			buf = append(buf, '\n')

			if err := appendToBuilderLog(logDir, result.buildID, result.drvPath, buf); err != nil {
				log.Warnf(ctx, "Failed to write build error to log: %v", err)
			}
		}
	}
	err = sqlitex.ExecuteTransientFS(conn, sqlFiles(), "build/end_result.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":id":               result.id,
			":status":           string(status),
			":timestamp_millis": result.endTime.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("record final build result: %v", err)
	}
	return nil
}

// buildLogRoot returns the path to the directory that contains the logs for build with ID buildID.
func buildLogRoot(dir string, buildID uuid.UUID) string {
	buildIDString := buildID.String()
	return filepath.Join(dir, buildIDString[:4], buildIDString)
}

// builderLogPath returns the filesystem path for the build log with the given identifiers.
// This will always be a direct child of the directory returned by [buildLogRoot]
// for buildLogRoot(dir, buildID).
func builderLogPath(dir string, buildID uuid.UUID, drvPath zbstore.Path) string {
	buildIDString := buildID.String()
	name := strings.TrimSuffix(drvPath.Base(), zbstore.DerivationExt) + ".txt"
	return filepath.Join(dir, buildIDString[:4], buildIDString, name)
}

// createBuilderLog creates a new builder log file for writing.
// If the log file already exists, createBuilderLog returns an error.
func createBuilderLog(dir string, buildID uuid.UUID, drvPath zbstore.Path) (*os.File, error) {
	path := builderLogPath(dir, buildID, drvPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, fmt.Errorf("create log for %s in build %s: %v", drvPath.Base(), buildID, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, fmt.Errorf("create log for %s in build %s: %v", drvPath.Base(), buildID, err)
	}
	return f, nil
}

// appendToBuilderLog writes data to the end of the builder log file.
func appendToBuilderLog(dir string, buildID uuid.UUID, drvPath zbstore.Path, data []byte) error {
	path := builderLogPath(dir, buildID, drvPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return fmt.Errorf("create log for %s in build %s: %v", drvPath.Base(), buildID, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	var logErrors [2]error
	_, logErrors[0] = f.Write(data)
	logErrors[1] = f.Close()
	for _, err := range logErrors {
		if err != nil {
			return err
		}
	}
	return nil
}

func prepareConn(conn *sqlite.Conn) error {
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA auto_vacuum = incremental;", nil); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode = wal;", nil); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA synchronous = normal;", nil); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA optimize = 0x10002;", nil); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = on;", nil); err != nil {
		return err
	}
	// uuid(TEXT) -> BLOB | NULL
	// Parse UUID, returning NULL if it does not represent a valid UUID.
	err := conn.CreateFunction("uuid", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		AllowIndirect: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			u, err := uuid.Parse(args[0].Text())
			if err != nil {
				return sqlite.Value{}, nil
			}
			return sqlite.BlobValue(u[:]), nil
		},
	})
	if err != nil {
		return err
	}
	// uuidhex(any) -> TEXT | NULL
	// Format UUID in canonical dash-separated lower hex format.
	// If argument is not a BLOB, it is converted to TEXT and parsing is attempted.
	// If parsing fails or the argument is a BLOB with a length other than 16,
	// then uuidhex returns NULL.
	err = conn.CreateFunction("uuidhex", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		AllowIndirect: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			var u uuid.UUID
			switch args[0].Type() {
			case sqlite.TypeBlob:
				b := args[0].Blob()
				if len(b) != len(u) {
					return sqlite.Value{}, nil
				}
				copy(u[:], b)
			default:
				var err error
				u, err = uuid.Parse(args[0].Text())
				if err != nil {
					return sqlite.Value{}, nil
				}
			}
			return sqlite.TextValue(u.String()), nil
		},
	})
	if err != nil {
		return err
	}
	return nil
}

//go:embed sql/*.sql
//go:embed sql/build/*.sql
//go:embed sql/delete/*.sql
//go:embed sql/realizations/*.sql
//go:embed sql/running_server/*.sql
//go:embed sql/schema/*.sql
var rawSQLFiles embed.FS

func sqlFiles() fs.FS {
	sub, err := fs.Sub(rawSQLFiles, "sql")
	if err != nil {
		panic(err)
	}
	return sub
}

var schemaState struct {
	init   sync.Once
	schema sqlitemigration.Schema
	err    error
}

func loadSchema() sqlitemigration.Schema {
	schemaState.init.Do(func() {
		for i := 1; ; i++ {
			migration, err := fs.ReadFile(sqlFiles(), fmt.Sprintf("schema/%02d.sql", i))
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				schemaState.err = err
				return
			}
			schemaState.schema.Migrations = append(schemaState.schema.Migrations, string(migration))
		}
	})

	if schemaState.err != nil {
		panic(schemaState.err)
	}
	return schemaState.schema
}

func marshalJSONString(v any) (string, error) {
	sb := new(strings.Builder)
	err := jsonv2.MarshalWrite(sb, v, jsontext.EscapeForHTML(false))
	return sb.String(), err
}

func unmarshalJSONString(data string, out any, opts ...jsonv2.Options) error {
	return jsonv2.UnmarshalRead(strings.NewReader(data), out, opts...)
}

func unmarshalHash(algo string, bits []byte) (nix.Hash, error) {
	t, err := nix.ParseHashType(algo)
	if err != nil {
		return nix.Hash{}, err
	}
	if got, want := len(bits), t.Size(); got != want {
		return nix.Hash{}, fmt.Errorf("unmarshal hash: digest is incorrect size (%d instead of %d) for %s",
			got, want, t)
	}
	return nix.NewHash(t, bits), nil
}

// readonlySavepoint starts a new SAVEPOINT.
// The caller is responsible for calling endFn
// to roll back the SAVEPOINT and remove it from the transaction stack.
func readonlySavepoint(conn *sqlite.Conn) (rollbackFunc func(), err error) {
	name := "readonlySavepoint" // safe as names can be reused
	var pc [3]uintptr
	if n := runtime.Callers(0, pc[:]); n > 0 {
		frames := runtime.CallersFrames(pc[:n])
		if _, more := frames.Next(); more { // runtime.Callers
			if _, more := frames.Next(); more { // readonlySavepoint
				frame, _ := frames.Next() // caller we care about
				if frame.Function != "" {
					name = frame.Function
				}
			}
		}
	}

	startedTransaction := conn.AutocommitEnabled()
	if err := sqlitex.Execute(conn, `SAVEPOINT "`+name+`";`, nil); err != nil {
		return nil, err
	}
	if startedTransaction {
		rollbackFunc = func() {
			panicError := recover()

			if conn.AutocommitEnabled() {
				// Transaction exited by application. Nothing to roll back.
				if panicError != nil {
					panic(panicError)
				}
				return
			}

			// Always run ROLLBACK even if the connection has been interrupted.
			oldDoneChan := conn.SetInterrupt(nil)
			defer conn.SetInterrupt(oldDoneChan)

			if err := sqlitex.Execute(conn, "ROLLBACK;", nil); err != nil {
				panic(err.Error())
			}
			if panicError != nil {
				panic(panicError)
			}
		}
	} else {
		rollbackFunc = func() {
			panicError := recover()

			if conn.AutocommitEnabled() {
				// Transaction exited by application. Nothing to roll back.
				if panicError != nil {
					panic(panicError)
				}
				return
			}

			// Always run ROLLBACK even if the connection has been interrupted.
			oldDoneChan := conn.SetInterrupt(nil)
			defer conn.SetInterrupt(oldDoneChan)

			if err := sqlitex.Execute(conn, `ROLLBACK TO "`+name+`";`, nil); err != nil {
				panic(err.Error())
			}
			if err := sqlitex.Execute(conn, `RELEASE "`+name+`";`, nil); err != nil {
				panic(err.Error())
			}
			if panicError != nil {
				panic(panicError)
			}
		}
	}

	return rollbackFunc, nil
}

// updateHeartbeat writes the current time to the running server state in the database.
func updateHeartbeat(conn *sqlite.Conn) error {
	now := time.Now()
	err := sqlitex.ExecuteScriptFS(conn, sqlFiles(), "running_server/tick_ttl.sql", &sqlitex.ExecOptions{
		Named: map[string]any{
			":now": now.UnixMilli(),
		},
	})
	if err != nil {
		return fmt.Errorf("update server heartbeat: %v", err)
	}
	return nil
}

func clearServerState(conn *sqlite.Conn) error {
	err := sqlitex.ExecuteFS(conn, sqlFiles(), "running_server/clear.sql", nil)
	if err != nil {
		return fmt.Errorf("clear server state: %v", err)
	}

	return nil
}

// lastHeartbeat reads the last time [updateHeartbeat] was called on the database.
// If there is no heartbeat, then lastHeartbeat returns the zero time.
func lastHeartbeat(conn *sqlite.Conn) (time.Time, error) {
	var lastHeartbeat time.Time
	err := sqlitex.ExecuteFS(conn, sqlFiles(), "running_server/get.sql", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			lastHeartbeat = time.UnixMilli(stmt.GetInt64("running_as_of"))

			return nil
		},
	})

	if err != nil {
		return time.Time{}, fmt.Errorf("get last server heartbeat: %v", err)
	}

	return lastHeartbeat, nil
}
