// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unique"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/uuid"
	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/detect"
	"zb.256lights.llc/pkg/internal/jsonrpc"
	"zb.256lights.llc/pkg/internal/storepath"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/xio"
	"zb.256lights.llc/pkg/internal/xiter"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
	"zombiezen.com/go/xcontext"
)

// Special environment variable names.
const (
	buildSystemDepsVar = "__buildSystemDeps"
	networkVar         = "__network"
)

func (s *Server) realize(ctx context.Context, req *jsonrpc.Request) (_ *jsonrpc.Response, err error) {
	// Validate request.
	var args zbstorerpc.RealizeRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	if len(args.DrvPaths) == 0 {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("no derivation paths given"))
	}
	var drvPaths []zbstore.Path
	for _, arg := range args.DrvPaths {
		drvPath, subPath, err := s.dir.ParsePath(string(arg))
		if err != nil {
			return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
		}
		if subPath != "" {
			return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("%s is not a store object", arg))
		}
		if _, isDrv := drvPath.DerivationName(); !isDrv {
			return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("%s is not a derivation", drvPath))
		}
		drvPaths = append(drvPaths, drvPath)
	}
	buildID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	drvPathList := joinStrings(drvPaths, ", ")
	log.Infof(ctx, "New build %v: %s", buildID, drvPathList)

	drvCache, err := s.readDerivationClosure(ctx, drvPaths)
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPathList, err)
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	err = s.startBuild(ctx, conn, buildID, func(ctx context.Context) {
		wantOutputs := make(sets.Set[zbstore.OutputReference])
		for _, drvPath := range drvPaths {
			for outputName := range drvCache[drvPath].Outputs {
				wantOutputs.Add(zbstore.OutputReference{
					DrvPath:    drvPath,
					OutputName: outputName,
				})
			}
		}
		b := s.newBuilder(buildID, drvCache, args.Reuse)
		realizeError := b.realize(ctx, wantOutputs, args.KeepFailed)
		if realizeError != nil && !errors.Is(realizeError, errUnfinishedRealization) {
			log.Errorf(ctx, "Realize internal error: %v", realizeError)
		}

		recordCtx, cancel := xcontext.KeepAlive(ctx, 30*time.Second)
		defer cancel()
		conn, err := s.db.Get(recordCtx)
		if err != nil {
			log.Errorf(recordCtx, "Unable to record end of build %s: %v. Original error: %v", buildID, err, realizeError)
			return
		}
		defer s.db.Put(conn)
		if err := recordBuildEnd(conn, buildID, realizeError); err != nil {
			log.Errorf(recordCtx, "Unable to record end of build %s: %v. Original error: %v", buildID, err, realizeError)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPathList, err)
	}

	return marshalResponse(&zbstorerpc.RealizeResponse{
		BuildID: buildID.String(),
	})
}

func (s *Server) expand(ctx context.Context, req *jsonrpc.Request) (_ *jsonrpc.Response, err error) {
	// Validate request.
	var args zbstorerpc.ExpandRequest
	if err := jsonv2.Unmarshal(req.Params, &args); err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	drvPath, subPath, err := s.dir.ParsePath(string(args.DrvPath))
	if err != nil {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, err)
	}
	if subPath != "" {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("%s is not a store object", args.DrvPath))
	}
	if _, isDrv := drvPath.DerivationName(); !isDrv {
		return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("%s is not a derivation", drvPath))
	}
	temporaryDirectory := args.TemporaryDirectory
	if temporaryDirectory == "" {
		// Provide a static per-platform fallback.
		// We don't use [os.TempDir] because that leaks environment variables
		// from the build server.
		if runtime.GOOS == "windows" {
			return nil, jsonrpc.Error(jsonrpc.InvalidParams, fmt.Errorf("missing temporary directory"))
		}
		temporaryDirectory = "/tmp"
	}
	log.Infof(ctx, "Requested to expand %s", drvPath)
	if string(s.dir) != s.realDir {
		return nil, fmt.Errorf("store cannot build derivations (unsandboxed and storage directory does not match store)")
	}

	drvCache, err := s.readDerivationClosure(ctx, []zbstore.Path{drvPath})
	if err != nil {
		return nil, fmt.Errorf("expand %s: %v", drvPath, err)
	}
	buildID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("expand %s: %v", drvPath, err)
	}

	conn, err := s.db.Get(ctx)
	if err != nil {
		return nil, err
	}
	defer s.db.Put(conn)

	err = s.startBuild(ctx, conn, buildID, func(ctx context.Context) {
		drv := drvCache[drvPath]
		inputs := sets.Collect(drv.InputDerivationOutputs())

		b := s.newBuilder(buildID, drvCache, args.Reuse)
		realizeError := b.realize(ctx, inputs, false)
		if realizeError != nil && !errors.Is(realizeError, errUnfinishedRealization) {
			log.Errorf(ctx, "Realize internal error: %v", realizeError)
		}

		recordCtx, cancel := xcontext.KeepAlive(ctx, 30*time.Second)
		defer cancel()
		conn, err := s.db.Get(recordCtx)
		if err != nil {
			log.Errorf(recordCtx, "Unable to record end of build %s: %v. Original error: %v", buildID, err, realizeError)
			return
		}
		defer s.db.Put(conn)

		if realizeError != nil {
			if err := recordBuildEnd(conn, buildID, realizeError); err != nil {
				log.Errorf(recordCtx, "Unable to record end of build %s: %v. Original error: %v", buildID, err, realizeError)
			}
			return
		}

		expandedDrv, expandError := b.expand(drvPath, drv, temporaryDirectory)
		if expandError != nil {
			// Errors at this stage indicate defects in zb.
			log.Errorf(recordCtx, "Expand %s: %v", drvPath, expandError)
		}

		err = func() (err error) {
			endTx, err := sqlitex.ImmediateTransaction(conn)
			if err != nil {
				return err
			}
			defer endTx(&err)
			if err := recordBuildEnd(conn, buildID, expandError); err != nil {
				return err
			}
			if expandError == nil {
				err := recordExpandResult(conn, buildID, &zbstorerpc.ExpandResult{
					Builder: expandedDrv.Builder,
					Args:    expandedDrv.Args,
					Env:     expandedDrv.Env,
				})
				if err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			log.Errorf(recordCtx, "Unable to record end of build %s: %v", buildID, err)
			return
		}
	})
	if err != nil {
		return nil, fmt.Errorf("expand %s: %v", drvPath, err)
	}

	return marshalResponse(&zbstorerpc.ExpandResponse{
		BuildID: buildID.String(),
	})
}

// startBuild calls f in its own goroutine
// with a [context.Context] whose values are taken from parent
// and its cancellation controlled by calls to [*Server.cancelBuild] for the build ID.
// startBuild returns an error if [*Server.Drain] or [*Server.Close] have been called
// or the build could not be recorded in the database.
func (s *Server) startBuild(parent context.Context, conn *sqlite.Conn, buildID uuid.UUID, f func(ctx context.Context)) error {
	ctx := s.buildContext(context.WithoutCancel(parent), buildID.String())
	ctx, cancel := context.WithCancel(splitContext{
		cancel: s.backgroundContext,
		values: ctx,
	})

	s.activeBuildsMu.Lock()
	draining := s.drained != nil
	if !draining {
		s.activeBuilds[buildID] = cancel
	}
	s.activeBuildsMu.Unlock()

	if draining {
		cancel()
		return errors.New("server shutting down; not starting new builds")
	}

	endBuild := func() {
		s.activeBuildsMu.Lock()
		delete(s.activeBuilds, buildID)
		s.lockedUpdateDrained()
		s.activeBuildsMu.Unlock()
		cancel()
	}
	if err := recordBuildStart(conn, buildID); err != nil {
		endBuild()
		return err
	}

	s.background.Go(func() {
		defer endBuild()
		f(ctx)
	})
	return nil
}

type builder struct {
	id     uuid.UUID
	server *Server

	reusePolicy  *zbstorerpc.ReusePolicy
	derivations  map[zbstore.Path]*zbstore.Derivation
	drvHashes    map[zbstore.Path]nix.Hash
	realizations map[equivalenceClass]cachedRealization
}

type cachedRealization struct {
	// path is the path of the realized store object.
	path zbstore.Path

	// closure is a map of paths transitively referenced by the realization's path
	// to a set of equivalence classes that may have produced that path.
	// The zero [equivalenceClass] indicates that the path was a "source".
	closure map[zbstore.Path]sets.Set[equivalenceClass]
}

func (s *Server) newBuilder(id uuid.UUID, derivations map[zbstore.Path]*zbstore.Derivation, reuse *zbstorerpc.ReusePolicy) *builder {
	if reuse == nil {
		reuse = new(zbstorerpc.ReusePolicy)
	}
	return &builder{
		server:      s,
		id:          id,
		derivations: derivations,

		reusePolicy:  reuse,
		drvHashes:    make(map[zbstore.Path]nix.Hash),
		realizations: make(map[equivalenceClass]cachedRealization),
	}
}

func (b *builder) toEquivalenceClass(ref zbstore.OutputReference) (_ derivationPathAndEquivalenceClass, ok bool) {
	if ref.OutputName == "" {
		return derivationPathAndEquivalenceClass{}, false
	}
	h := b.drvHashes[ref.DrvPath]
	if h.IsZero() {
		return derivationPathAndEquivalenceClass{}, false
	}
	return derivationPathAndEquivalenceClass{
		drvPath:          ref.DrvPath,
		equivalenceClass: newEquivalenceClass(h, ref.OutputName),
	}, true
}

// lookup returns the realized path for the given output if the builder realized one.
func (b *builder) lookup(ref zbstore.OutputReference) (_ zbstore.Path, ok bool) {
	eqClassRef, ok := b.toEquivalenceClass(ref)
	if !ok {
		return "", false
	}
	r, ok := b.realizations[eqClassRef.equivalenceClass]
	return r.path, ok
}

var errUnfinishedRealization = errors.New("realization did not complete")

func (b *builder) realize(ctx context.Context, want sets.Set[zbstore.OutputReference], keepFailed bool) error {
	log.Debugf(ctx, "Will realize %v...", want)

	graph, err := analyze(b.derivations, want)
	if err != nil {
		return err
	}
	if err := b.gatherRealizations(ctx, graph); err != nil {
		return err
	}
	buildRoots, err := b.obtainBuildRoots(ctx, graph)
	if err != nil {
		return err
	}

	log.Debugf(ctx, "Realizing %v from %v...", want, buildRoots)
	drvLocks := make(map[zbstore.Path]func())
	defer func() {
		for _, unlock := range drvLocks {
			unlock()
		}
	}()
	it := newDependencyOrderIterator(graph, buildRoots.All())
	for {
		curr, err := it.next(ctx)
		if err == errEndIteration {
			return nil
		}
		if err != nil {
			return err
		}

		drv := b.derivations[curr]
		if drv == nil {
			return fmt.Errorf("realize %v: unknown derivation", curr)
		}
		log.Debugf(ctx, "Reached %v", curr)
		drvHash, err := drv.SHA256RealizationHash(b.lookup)
		if err != nil {
			return fmt.Errorf("realize %s: %v", curr, err)
		}
		log.Debugf(ctx, "Hashed %s to %v", curr, drvHash)
		b.drvHashes[curr] = drvHash

		log.Debugf(ctx, "Waiting for build lock on %s...", curr)
		unlock, err := b.server.building.lock(ctx, curr)
		if err != nil {
			return err
		}
		drvLocks[curr] = unlock
		log.Debugf(ctx, "Acquired build lock on %s", curr)
		graphNode := graph.nodes[curr]
		if err := b.do(ctx, curr, graphNode.usedOutputs, keepFailed); err != nil {
			// b.do already records the build failure,
			// so we don't need to report the same error at the build level.
			if !isBuilderFailure(err) {
				log.Errorf(ctx, "%v", err)
			}
			return errUnfinishedRealization
		}
		drvLocks[curr]()
		delete(drvLocks, curr)

		// Queue up new work.
		it.finish(curr, true)
	}
}

func (b *builder) expand(drvPath zbstore.Path, drv *zbstore.Derivation, temporaryDirectory string) (*zbstore.Derivation, error) {
	outPaths, err := tempOutputPaths(drvPath, drv.Outputs)
	if err != nil {
		return nil, fmt.Errorf("expand %s: %v", drvPath, err)
	}
	inputRewrites, err := derivationInputRewrites(drv, b.lookup)
	if err != nil {
		return nil, fmt.Errorf("expand %s: %v", drvPath, err)
	}
	r := newReplacer(xiter.Chain2(
		outputPathRewrites(outPaths),
		maps.All(inputRewrites),
	))
	expandedDrv := drv.ReplaceStrings(r)
	fillBaseEnv(expandedDrv.Env, drv.Dir, temporaryDirectory, b.server.coresPerBuild)
	return expandedDrv, nil
}

// gatherRealizations attempts to gather as many realizations in the graph
// from the local and fallback stores
// without running any builders.
func (b *builder) gatherRealizations(ctx context.Context, graph *dependencyGraph) error {
	for it := graph.iterator(); ; {
		curr, err := it.next(ctx)
		if err == errEndIteration {
			log.Debugf(ctx, "Gather complete")
			return nil
		}
		if err != nil {
			return err
		}
		log.Debugf(ctx, "Reached %v in gather", curr)
		node := graph.nodes[curr]
		if node == nil {
			return fmt.Errorf("gather realizations for %v: unknown derivation", curr)
		}

		err = b.gatherRealizationsForDerivation(ctx, curr, node)
		if err != nil {
			if errors.Is(err, errMultipleRealizations) || errors.Is(err, errRealizationNotFound) {
				log.Debugf(ctx, "Unable to gather realization for %s (%v)", curr, err)
				it.finish(curr, false)
				continue
			}
			return err
		}
		it.finish(curr, true)
	}
}

func (b *builder) gatherRealizationsForDerivation(ctx context.Context, curr zbstore.Path, node *dependencyGraphNode) (err error) {
	conn, err := b.server.db.Get(ctx)
	if err != nil {
		return fmt.Errorf("realize %s: %v", curr, err)
	}
	defer b.server.db.Put(conn)

	drvHash, err := node.derivation.SHA256RealizationHash(b.lookup)
	if err != nil {
		return fmt.Errorf("realize %s: %v", curr, err)
	}
	log.Debugf(ctx, "Hashed %s to %v", curr, drvHash)
	b.drvHashes[curr] = drvHash

	drvHashKey := makeHashKey(drvHash)
	wantEqClasses := iter.Seq[derivationPathAndEquivalenceClass](func(yield func(derivationPathAndEquivalenceClass) bool) {
		for outputName := range node.usedOutputs.All() {
			dpe := derivationPathAndEquivalenceClass{
				drvPath: curr,
				equivalenceClass: equivalenceClass{
					drvHashKey: drvHashKey,
					outputName: outputName,
				},
			}
			if !yield(dpe) {
				return
			}
		}
	})

	// Fetch realizations whose store objects exist locally.
	// (If the realization's store object does not exist locally, we accept that later,
	// but we only want to fetch realizations from the fallback store
	// if we don't have a suitable realization set locally.)
	p := b.newLocalOnlyPlanner()
	p.planSeq(ctx, conn, wantEqClasses)
	switch {
	case errors.Is(p.error, errMultipleRealizations):
		// Ignore.
		// Downloading more realizations isn't going to help:
		// we have a non-hermetic step.
		return p.error
	case errors.Is(p.error, errRealizationNotFound):
		// It's possible the fallback store has more realizations.
		// Fetch and record those.
		realizations := b.fetchRealizationsFromFallback(ctx, drvHash)
		if realizations.IsEmpty() {
			return fmt.Errorf("realize %s: %w", curr, errRealizationNotFound)
		}
		err := func() (err error) {
			end, err := sqlitex.ImmediateTransaction(conn)
			if err != nil {
				return err
			}
			defer end(&err)
			return recordRealizations(conn, realizations.All())
		}()
		if err != nil {
			return fmt.Errorf("realize %s: %v", curr, err)
		}

		// Now retry with new realization data.
		// (As noted above, this time, we accept realizations without a local store object present.)
		p := b.newPlanner()
		p.planSeq(ctx, conn, wantEqClasses)
		if p.error != nil {
			return fmt.Errorf("realize %s: %w", curr, p.error)
		}
		p.commit()
	case err != nil:
		return fmt.Errorf("realize %s: %v", curr, err)
	default:
		p.commit()
	}

	return nil
}

// obtainBuildRoots computes the set of derivations that can be used as a basis for building the rest of graph,
// downloading store objects from the fallback store as needed.
func (b *builder) obtainBuildRoots(ctx context.Context, graph *dependencyGraph) (roots sets.Set[zbstore.Path], err error) {
	roots = make(sets.Set[zbstore.Path])
	for it := graph.iterator(); ; {
		curr, err := it.next(ctx)
		if err == errEndIteration {
			return roots, nil
		}
		if err != nil {
			return nil, err
		}
		log.Debugf(ctx, "Reached %v while obtaining build roots", curr)
		processDependents, err := b.obtainBuildRootsForDerivation(ctx, graph, roots, curr)
		if err != nil {
			return nil, err
		}
		it.finish(curr, processDependents)
	}
}

// obtainBuildRootsForDerivation will walk the derivation at drvPath
// and its transitive input derivations in breadth-first order
// and attempt to download store objects matching their realizations if possible.
// It reports whether the outputs of the derivation at drvPath are present in the local store.
func (b *builder) obtainBuildRootsForDerivation(ctx context.Context, graph *dependencyGraph, roots sets.Set[zbstore.Path], drvPath zbstore.Path) (bool, error) {
	node := graph.nodes[drvPath]
	if node == nil {
		return false, fmt.Errorf("obtain build roots for %s: unknown derivation", drvPath)
	}
	if !b.shouldObtainBuildRoots(graph, drvPath, node) {
		return true, nil
	}

	log.Debugf(ctx, "Walking back from %s while obtaining build roots", drvPath)

	conn, err := b.server.db.Get(ctx)
	if err != nil {
		return false, fmt.Errorf("obtain build roots for %s: %v", drvPath, err)
	}
	defer b.server.db.Put(conn)

	queue := []zbstore.Path{drvPath}
	var ignoreStack []zbstore.Path
	for len(queue) > 0 {
		curr := queue[0]
		queue = slices.Delete(queue, 0, 1)
		node := graph.nodes[curr]
		if node == nil {
			return false, fmt.Errorf("obtain build roots for %s: %s: unknown derivation", drvPath, curr)
		}
		drvHash := b.drvHashes[curr]
		if drvHash.IsZero() {
			return false, fmt.Errorf("obtain build roots for %s: %s: missing hash", drvPath, curr)
		}
		drvHashKey := makeHashKey(drvHash)
		paths := make(map[zbstore.Path]sets.Set[equivalenceClass])
		if b.allUsedRealizationsPresent(graph, curr) {
			for handle := range node.usedOutputs {
				eqClass := equivalenceClass{
					drvHashKey: drvHashKey,
					outputName: handle,
				}
				r := b.realizations[eqClass]
				addToMultiMap(paths, r.path, eqClass)
			}

			err := b.server.copyFromFallback(ctx, conn, func(yield func(pathAndEquivalenceClass) bool) {
				for path, eqClassesForPath := range paths {
					for eqClass := range eqClassesForPath.All() {
						if !yield(pathAndEquivalenceClass{path, eqClass}) {
							return
						}
					}
				}
			})
			if err == nil {
				log.Debugf(ctx, "Adding build root %s", curr)
				roots.Add(curr)
				if curr == drvPath {
					return true, nil
				}
				continue
			}
			log.Debugf(ctx, "Use %s as build root: %v", curr, err)
		}

		ignoreStack = append(ignoreStack, curr)
		b.ignoreRealizations(graph, roots, &ignoreStack)
		if len(node.derivation.InputDerivations) == 0 {
			log.Debugf(ctx, "Adding build root %s because it has no input derivations", curr)
			roots.Add(curr)
		} else {
			queue = slices.AppendSeq(queue, maps.Keys(node.derivation.InputDerivations))
		}
	}

	return false, nil
}

func (b *builder) shouldObtainBuildRoots(graph *dependencyGraph, drvPath zbstore.Path, node *dependencyGraphNode) bool {
	for outputName := range node.usedOutputs {
		// If this is one of the requested outputs, then we want to ensure the store objects exist locally
		// or that we can get dependencies as close as possible.
		ref := zbstore.OutputReference{
			DrvPath:    drvPath,
			OutputName: outputName.Value(),
		}
		if graph.want.Has(ref) {
			return true
		}
	}

	// If we haven't recorded realizations for all the outputs we need for the build,
	// then we'll get dependencies as close as possible.
	// (obtainBuildRootsForDerivation will start a BFS on the first iteration in this case.)
	if !b.allUsedRealizationsPresent(graph, drvPath) {
		return true
	}
	// If any dependent derivations are missing realizations needed for the build,
	// then try to use this derivation as a build root.
	// We may not visit the dependents if another part of the graph's realizations are ignored,
	// so we preemptively walk.
	// (See https://github.com/256lights/zb/issues/288)
	for dependent := range node.dependents.All() {
		if !b.allUsedRealizationsPresent(graph, dependent) {
			return true
		}
	}

	return false
}

// allUsedRealizationsPresent reports whether all the outputs used for the derivation path in the graph
// have realizations set.
func (b *builder) allUsedRealizationsPresent(graph *dependencyGraph, drvPath zbstore.Path) bool {
	node := graph.nodes[drvPath]
	if node == nil {
		return false
	}
	drvHash := b.drvHashes[drvPath]
	if drvHash.IsZero() {
		return false
	}
	drvHashKey := makeHashKey(drvHash)
	for outputName := range node.usedOutputs {
		eqClass := equivalenceClass{
			drvHashKey: drvHashKey,
			outputName: outputName,
		}
		if _, ok := b.realizations[eqClass]; !ok {
			return false
		}
	}
	return true
}

func (b *builder) ignoreRealizations(graph *dependencyGraph, roots sets.Set[zbstore.Path], drvPaths *[]zbstore.Path) {
	for curr := range graph.transitiveDependents(drvPaths) {
		h := b.drvHashes[curr]
		if !h.IsZero() {
			delete(b.drvHashes, curr)
			k := makeHashKey(h)
			for outputName := range b.derivations[curr].Outputs {
				delete(b.realizations, equivalenceClass{
					drvHashKey: k,
					outputName: unique.Make(outputName),
				})
			}
		}
		roots.Delete(curr)
	}
}

// derivationBuildState holds information used throughout a call to [*builder.do].
type derivationBuildState struct {
	startTime         time.Time
	drvPath           zbstore.Path
	outputNames       sets.Set[unique.Handle[string]]
	derivation        *zbstore.Derivation
	derivationHash    nix.Hash
	derivationHashKey hashKey

	buildResultID int64
}

// do ensures that a single derivation has realizations for the given set of outputs,
// either by reusing existing realizations or by building it.
// b.drvHashes must have a non-zero value for drvPath before calling do
// (which implies the caller realized all of the derivation's inputs)
// or else do returns an error.
func (b *builder) do(ctx context.Context, drvPath zbstore.Path, outputNames sets.Set[unique.Handle[string]], keepFailed bool) (err error) {
	state := &derivationBuildState{
		startTime:      time.Now(),
		drvPath:        drvPath,
		outputNames:    outputNames,
		derivation:     b.derivations[drvPath],
		derivationHash: b.drvHashes[drvPath],
	}
	if state.derivation == nil {
		return fmt.Errorf("build %s: unknown derivation", drvPath)
	}
	if state.derivationHash.IsZero() {
		return fmt.Errorf("build %s: missing hash", drvPath)
	}
	state.derivationHashKey = makeHashKey(state.derivationHash)
	for outputName := range outputNames.All() {
		if state.derivation.Outputs[outputName.Value()] == nil {
			ref := zbstore.OutputReference{
				DrvPath:    drvPath,
				OutputName: outputName.Value(),
			}
			return fmt.Errorf("build %v: no such output", ref)
		}
	}

	conn, err := b.server.db.Get(ctx)
	if err != nil {
		return err
	}
	defer b.server.db.Put(conn)

	if err := b.reuseRealizations(ctx, conn, state); err == nil || !isRealizationPlanningError(err) {
		return err
	}

	defer func() {
		endFn, txError := sqlitex.ImmediateTransaction(conn)
		if txError != nil {
			log.Warnf(ctx, "For build %s: %v", drvPath, txError)
			return
		}
		finalizeError := finalizeBuildResult(ctx, conn, b.server.logDir, &buildFinalResults{
			buildID: b.id,
			drvPath: drvPath,
			id:      state.buildResultID,
			endTime: time.Now(),
			error:   err,
		})
		endFn(&finalizeError)
		if finalizeError != nil {
			log.Warnf(ctx, "For build %s: %v", drvPath, finalizeError)
		}
	}()

	// If fixed output, acquire write lock on output path.
	var unlockFixedOutput func()
	if outputPath, err := state.derivation.OutputPath(zbstore.DefaultDerivationOutputName); err == nil {
		log.Debugf(ctx, "%s has fixed output %s. Waiting for lock to check for reuse...", drvPath, outputPath)
		unlock, err := b.server.writing.lock(ctx, outputPath)
		if err != nil {
			return fmt.Errorf("build %s: wait for %s: %w", drvPath, outputPath, err)
		}
		var unlockOnce sync.Once
		unlockFixedOutput = func() {
			unlockOnce.Do(func() {
				unlock()
			})
		}
		defer unlockFixedOutput()

		_, err = os.Lstat(b.server.realPath(outputPath))
		log.Debugf(ctx, "%s exists=%t (output of %s)", outputPath, err == nil, drvPath)
		if err == nil {
			outputs := zbstore.RealizationMap{
				DerivationHash: state.derivationHash,
				Realizations: map[string][]*zbstore.Realization{
					zbstore.DefaultDerivationOutputName: {
						{
							OutputPath: outputPath,
							// Fixed outputs don't have references.
						},
					},
				},
			}
			if err := b.recordRealizations(ctx, conn, state.buildResultID, outputs); err != nil {
				return fmt.Errorf("build %s: %v", drvPath, err)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("build %s: %v", drvPath, err)
		}

		// TODO(someday): b.copyFromFallbackAndFinalizeBuildResult
	}

	// Verify that builder can run.
	if !canBuildLocally(state.derivation) {
		return fmt.Errorf("build %s: a %s system is required, but host is a %v system",
			drvPath, state.derivation.System, system.Current())
	}
	buildSystemDeps := state.derivation.Env[buildSystemDepsVar]
	if hasPlaceholders(state.derivation, buildSystemDeps) {
		return fmt.Errorf("build %s: %s contains placeholders", drvPath, buildSystemDeps)
	}
	for dep := range strings.FieldsSeq(buildSystemDeps) {
		if !xmaps.HasKey(b.server.sandboxPaths, dep) {
			return fmt.Errorf("build %s: system dependency %s not allowed", drvPath, buildSystemDeps)
		}
	}
	for _, input := range state.derivation.InputSources.All() {
		log.Debugf(ctx, "Waiting for lock on %s (input to %s)...", input, drvPath)
		unlockInput, err := b.server.writing.lock(ctx, input)
		if err != nil {
			return fmt.Errorf("build %s: wait for %s: %w", drvPath, input, err)
		}
		_, err = os.Lstat(b.server.realPath(input))
		unlockInput()
		log.Debugf(ctx, "%s exists=%t (input to %s)", input, err == nil, drvPath)
		if err != nil {
			return fmt.Errorf("build %s: input %s not present (%v)", drvPath, input, err)
		}
	}
	buildUser, err := b.server.users.acquire(ctx)
	if err != nil {
		return fmt.Errorf("build %s: %v", drvPath, err)
	}
	if buildUser != nil {
		log.Debugf(ctx, "Using build user %v", buildUser)
	}
	defer b.server.users.release(buildUser)

	// Arrange for builder to run.
	var runner runnerFunc
	switch {
	case state.derivation.System == builtinSystem:
		log.Debugf(ctx, "Runner for %s is builtin", drvPath)
		runner = runBuiltin
	case b.server.sandbox:
		log.Debugf(ctx, "Runner for %s is sandbox", drvPath)
		runner = runSandboxed
	default:
		log.Debugf(ctx, "Runner for %s is unsandboxed", drvPath)
		runner = runSubprocess
	}
	tempOutPaths, err := b.runBuilder(ctx, conn, drvPath, state.buildResultID, keepFailed, buildUser, runner)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			for _, outPath := range tempOutPaths {
				realOutPath := b.server.realPath(outPath)
				if err := os.RemoveAll(realOutPath); err != nil {
					log.Warnf(ctx, "Cleanup failure: %v", err)
				}
			}
		}
	}()

	// Save outputs as store objects.
	inputs, err := b.inputs(conn, drvPath)
	if err != nil {
		return err
	}
	inputPaths := sets.CollectSorted(maps.Keys(inputs))
	outputs := zbstore.RealizationMap{
		DerivationHash: state.derivationHash,
		Realizations:   make(map[string][]*zbstore.Realization),
	}
	objectsToUpload := make([]*zbstore.ObjectInfo, 0, len(tempOutPaths))
	for outputName, tempOutputPath := range tempOutPaths {
		ref := zbstore.OutputReference{
			DrvPath:    drvPath,
			OutputName: outputName,
		}
		info, err := b.postprocess(ctx, conn, ref, tempOutputPath, unlockFixedOutput, inputPaths)
		if err != nil {
			return fmt.Errorf("build %s: %v", drvPath, err)
		}
		delete(tempOutPaths, outputName) // No longer needs cleanup if we fail.
		objectsToUpload = append(objectsToUpload, info)

		eqClass := equivalenceClass{
			drvHashKey: state.derivationHashKey,
			outputName: unique.Make(outputName),
		}
		prev, previouslyRealized := b.realizations[eqClass]
		if previouslyRealized && info.StorePath != prev.path {
			// This should have been prevented at a higher level,
			// but we do a safety check here anyway.
			return fmt.Errorf("build %s: output %s: new path %s conflicts with existing %s",
				drvPath, outputName, info.StorePath, prev.path)
		}

		r := &zbstore.Realization{OutputPath: info.StorePath}
		for ref, eqClasses := range inputs {
			if info.References.Has(ref) {
				for eqClass := range eqClasses.All() {
					r.ReferenceClasses = append(r.ReferenceClasses, &zbstore.ReferenceClass{
						Path:        ref,
						Realization: eqClass.toRealizationOutputReference(),
					})
				}
			}
		}
		r.Signatures, err = b.server.keyring.Sign(zbstore.RealizationOutputReference{
			DerivationHash: state.derivationHash,
			OutputName:     outputName,
		}, r)
		if err != nil {
			log.Warnf(ctx, "Signing built realization: %v", err)
		}
		outputs.Realizations[outputName] = []*zbstore.Realization{r}
	}

	if b.server.upload != nil {
		srv := b.server
		srv.detachFromBuild(ctx, func(ctx context.Context) {
			srv.uploadClosure(ctx, slices.Values(objectsToUpload))
		})
	}

	// Record realizations.
	if err := b.recordRealizations(ctx, conn, state.buildResultID, outputs); err != nil {
		return fmt.Errorf("build %s: %v", drvPath, err)
	}

	log.Infof(ctx, "Built %s: %s", drvPath, formatOutputPaths(maps.Collect(func(yield func(string, zbstore.Path) bool) {
		for ref, r := range outputs.All() {
			if !yield(ref.OutputName, r.OutputPath) {
				return
			}
		}
	})))
	return nil
}

// reuseRealizations creates a new build result for the derivation being built in state,
// then attempts to reuse available realizations (locally and in the fallback store)
// to satisfy the build request.
// If reuseRealizations returns an error for which [isRealizationPlanningError] reports true,
// then the build result was created successfully
// (and reuseRealizations will store its ID in state.buildResultID),
// but there's no suitable realization that can be reused.
//
// In a perfect world, this function would not also create the build result,
// since that mixes concerns.
// However, since this is the first database transaction during [*builder.do],
// it ends up occurring here.
func (b *builder) reuseRealizations(ctx context.Context, conn *sqlite.Conn, state *derivationBuildState) error {
	var p *realizationPlanner
	transactionError := func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)

		state.buildResultID, err = insertBuildResult(conn, b.id, state.drvPath, state.derivationHash, state.startTime)
		if err != nil {
			return err
		}

		p, err = b.planRealizationsAndFinalizeBuildResult(ctx, conn, state)
		if err != nil {
			return err
		}

		// Regardless of whether the realization search succeeded or not,
		// we want to set outputs for this build results during this transaction.
		// If the search did not pull up anything, we set a default of nulls for all requested outputs.
		if !p.isAvailableLocally() {
			err := setBuildResultOutputs(conn, state.buildResultID, buildResultOutputsFromPlanner(state, nil))
			if err != nil {
				// If setting the outputs failed, then it's probably an I/O error of some sort.
				// Finalizing the build result probably won't succeed,
				// so we just return the error and bail early.
				return err
			}
		}

		return nil
	}()
	if transactionError != nil {
		return fmt.Errorf("build %s: %v", state.drvPath, transactionError)
	}

	switch {
	case p.isAvailableLocally():
		p.commit()
		return nil
	case p.isPermanentError():
		return fmt.Errorf("build %s: find existing realizations: %w", state.drvPath, p.error)
	case p.error == nil && !p.isAvailableLocally():
		switch err := b.copyFromFallbackAndFinalizeBuildResult(ctx, conn, state, p); {
		case err == nil:
			p.commit()
			return nil
		case isCopyFromFallbackError(err):
			// If downloading store objects from the fallback fails,
			// then we can treat this case like not finding a realization.
			log.Debugf(ctx, "build %s: during first %v", state.drvPath, err)
		default:
			return fmt.Errorf("build %s: %v", state.drvPath, err)
		}
	}

	newRealizations := b.fetchRealizationsFromFallback(ctx, state.derivationHash)
	if newRealizations.IsEmpty() {
		return fmt.Errorf("build %s: %w", state.drvPath, errRealizationNotFound)
	}

	// Store new realizations and retry planning.
	var finalizeError error
	transactionError = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)

		if err := recordRealizations(conn, newRealizations.All()); err != nil {
			return err
		}

		// If finalizing the build result fails, don't fail the transaction:
		// we want to record the realizations!
		p, finalizeError = b.planRealizationsAndFinalizeBuildResult(ctx, conn, state)

		return nil
	}()
	if err := cmp.Or(transactionError, finalizeError); err != nil {
		return fmt.Errorf("build %s: %v", state.drvPath, err)
	}
	if p.error != nil {
		return fmt.Errorf("build %s: find existing realizations: %w", state.drvPath, p.error)
	}

	if !p.isAvailableLocally() {
		if err := b.copyFromFallbackAndFinalizeBuildResult(ctx, conn, state, p); err != nil {
			if isCopyFromFallbackError(err) {
				// If downloading store objects from the fallback fails,
				// then we can treat this case like not finding a realization.
				log.Debugf(ctx, "build %s: %v", state.drvPath, err)
				err = fmt.Errorf("build %s: %w", state.drvPath, errRealizationNotFound)
			}
			return err
		}
	}

	p.commit()
	return nil
}

// planRealizationsAndFinalizeBuildResult finds a set of realizations for the derivation named by state.drvPath.
// If a set is found and the output store objects are present locally
// or if no set is found due to an internal error,
// then planRealizationsAndFinalizeBuildResult will set the build result outputs
// and finalize the build result.
func (b *builder) planRealizationsAndFinalizeBuildResult(ctx context.Context, conn *sqlite.Conn, state *derivationBuildState) (_ *realizationPlanner, err error) {
	defer sqlitex.Save(conn)(&err)

	p := b.newPlanner()
	p.planSeq(ctx, conn, func(yield func(derivationPathAndEquivalenceClass) bool) {
		for outputName := range state.outputNames.All() {
			dpe := derivationPathAndEquivalenceClass{
				drvPath: state.drvPath,
				equivalenceClass: equivalenceClass{
					drvHashKey: state.derivationHashKey,
					outputName: outputName,
				},
			}
			if !yield(dpe) {
				return
			}
		}
	})

	switch {
	case p.isAvailableLocally():
		outputs := buildResultOutputsFromPlanner(state, p)
		if err := setBuildResultOutputs(conn, state.buildResultID, outputs); err != nil {
			return nil, err
		}
		bfr := &buildFinalResults{
			buildID: b.id,
			drvPath: state.drvPath,
			id:      state.buildResultID,
			endTime: time.Now(),
		}
		if err := finalizeBuildResult(ctx, conn, b.server.logDir, bfr); err != nil {
			return nil, err
		}
	case p.error != nil && !isRealizationPlanningError(p.error):
		bfr := &buildFinalResults{
			buildID: b.id,
			drvPath: state.drvPath,
			id:      state.buildResultID,
			endTime: time.Now(),
			error:   p.error,
		}
		if err := finalizeBuildResult(ctx, conn, b.server.logDir, bfr); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// copyFromFallbackAndFinalizeBuildResult attempts to copy any absent store objects
// identified by the [realizationPlanner]
// from the fallback store.
// If this succeeds, then copyFromFallbackAndFinalizeBuildResult will record the build result.
// Callers can use [isCopyFromFallbackError] to determine whether any error returned from this function
// indicates a failure to copy the absent store objects from the fallback store.
func (b *builder) copyFromFallbackAndFinalizeBuildResult(ctx context.Context, conn *sqlite.Conn, state *derivationBuildState, p *realizationPlanner) error {
	err := b.server.copyFromFallback(ctx, conn, func(yield func(pathAndEquivalenceClass) bool) {
		for eqClass := range p.absent.All() {
			r := p.planned[eqClass] // Always present: p.absent is a set of keys in p.planned.
			if !yield(pathAndEquivalenceClass{r.path, eqClass}) {
				return
			}
		}
	})
	if err != nil {
		return err
	}
	endTime := time.Now()

	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	defer endFn(&err)

	outputs := buildResultOutputsFromPlanner(state, p)
	if err := setBuildResultOutputs(conn, state.buildResultID, outputs); err != nil {
		return err
	}
	return finalizeBuildResult(ctx, conn, b.server.logDir, &buildFinalResults{
		buildID: b.id,
		drvPath: state.drvPath,
		id:      state.buildResultID,
		endTime: endTime,
	})
}

func buildResultOutputsFromPlanner(state *derivationBuildState, p *realizationPlanner) iter.Seq2[string, zbstore.Path] {
	return func(yield func(string, zbstore.Path) bool) {
		for outputName := range state.outputNames.All() {
			eqClass := equivalenceClass{
				drvHashKey: state.derivationHashKey,
				outputName: outputName,
			}
			r, _ := p.get(eqClass)
			if !yield(outputName.Value(), r.path) {
				return
			}
		}
	}
}

func (b *builder) fetchRealizationsFromFallback(ctx context.Context, drvHash nix.Hash) zbstore.RealizationMap {
	if b.reusePolicy.IsZero() {
		// If our reuse policy won't permit any realizations, there's no point.
		log.Debugf(ctx, "Skipping fallback store for %v (build does not allow reuse)", drvHash)
		return zbstore.RealizationMap{DerivationHash: drvHash}
	}
	log.Debugf(ctx, "Fetching realizations for %v from fallback store...", drvHash)
	realizations, err := b.server.fallback.FetchRealizations(ctx, drvHash)
	if err != nil {
		log.Warnf(ctx, "Failed to fetch realizations: %v", err)
	}
	if log.IsEnabled(log.Debug) {
		for outputName, realizationList := range realizations.Realizations {
			if len(realizationList) == 0 {
				continue
			}
			ref := zbstore.RealizationOutputReference{
				DerivationHash: drvHash,
				OutputName:     outputName,
			}
			log.Debugf(ctx, "Found %d realizations for %v from fallback store", len(realizationList), ref)
		}
	}
	return realizations
}

// inputs computes the closure of all inputs used by the derivation at drvPath.
func (b *builder) inputs(conn *sqlite.Conn, drvPath zbstore.Path) (map[zbstore.Path]sets.Set[equivalenceClass], error) {
	drv := b.derivations[drvPath]
	if drv == nil {
		return nil, fmt.Errorf("input closure for %s: unknown derivation", drvPath)
	}
	result := make(map[zbstore.Path]sets.Set[equivalenceClass])
	for input := range drv.InputDerivationOutputs() {
		dpe, ok := b.toEquivalenceClass(input)
		if !ok {
			return nil, fmt.Errorf("input closure for %s: missing derivation hash for %v", drvPath, input)
		}
		out, ok := b.realizations[dpe.equivalenceClass]
		if !ok {
			return nil, fmt.Errorf("input closure for %s: missing realization for %v", drvPath, input)
		}
		for refPath, refClasses := range out.closure {
			dst := result[refPath]
			if dst == nil {
				dst = make(sets.Set[equivalenceClass])
				result[refPath] = dst
			}
			dst.AddSeq(refClasses.All())
		}
	}

	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return nil, fmt.Errorf("input closure for %s: %v", drvPath, err)
	}
	defer rollback()

	for _, input := range drv.InputSources.All() {
		err := closurePaths(conn, pathAndEquivalenceClass{path: input}, func(pe pathAndEquivalenceClass) bool {
			addToMultiMap(result, pe.path, pe.equivalenceClass)
			return true
		})
		if err != nil {
			return nil, fmt.Errorf("input closure for %s: %v", drvPath, err)
		}
	}

	return result, nil
}

// A runnerFunc is a function that can execute a builder.
//
// A runnerFunc should:
//   - Run the builder program
//     with the working directory
//     (and TMPDIR or equivalent environment variables)
//     set to invocation.buildDir.
//     Mapping the location is acceptable,
//     as long as files are physically stored in invocation.buildDir.
//   - Return a [builderFailure] if the builder did not run succesfully
//     (e.g. a user build failure).
//     Any other type of error is treated as an internal backend failure.
//   - Create filesystem objects in invocation.realStoreDir
//     for each output path in invocation.outputPaths.
type runnerFunc func(ctx context.Context, invocation *builderInvocation) error

type builderInvocation struct {
	// derivation is the derivation whose builder should be executed.
	// The caller is responsible for expanding any placeholders
	// in the derivation's fields.
	derivation *zbstore.Derivation
	// derivationPath is the path of the derivation whose builder is being executed.
	derivationPath zbstore.Path
	// outputPaths is the map of output name to path this builder is expected to produce.
	outputPaths map[string]zbstore.Path

	// realStoreDir is the directory where the store is located in the local filesystem.
	realStoreDir string
	// buildDir is the temporary directory created for this build.
	buildDir string
	// logWriter is where all builder output should be sent.
	logWriter io.Writer
	// lookup returns the store path for the given derivation output.
	// lookup should return paths for the inputs to the derivation the runner is building
	// at least.
	lookup func(ref zbstore.OutputReference) (zbstore.Path, bool)
	// closure calls yield for each store object
	// in the transitive closure of the store object at the given path.
	closure func(path zbstore.Path, yield func(zbstore.Path) bool) error
	// user is the Unix user to run the build as.
	// If nil, then the current process's user should be used.
	user *BuildUser
	// cores is a hint from the user to the builder
	// on the number of concurrent jobs to perform.
	cores int
	// sandboxPaths is a map of paths inside the sandbox
	// to paths on the host machine.
	// For sandboxed runners, these paths will be made available inside the sandbox.
	sandboxPaths map[string]string
}

// builderLogInterval is the maximum time between flushes of the builder log.
const builderLogInterval = 100 * time.Millisecond

func (b *builder) runBuilder(ctx context.Context, conn *sqlite.Conn, drvPath zbstore.Path, buildResultID int64, keepFailed bool, buildUser *BuildUser, f runnerFunc) (outPaths map[string]zbstore.Path, err error) {
	drvName, isDrv := drvPath.DerivationName()
	if !isDrv {
		return nil, fmt.Errorf("build %s: not a derivation", drvPath)
	}
	drv := b.derivations[drvPath]
	if drv == nil {
		return nil, fmt.Errorf("build %s: unknown derivation", drvPath)
	}

	outPaths, err = tempOutputPaths(drvPath, drv.Outputs)
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPath, err)
	}
	if log.IsEnabled(log.Debug) {
		log.Debugf(ctx, "Output map for %s: %s", drvPath, formatOutputPaths(outPaths))
	}
	inputRewrites, err := derivationInputRewrites(drv, b.lookup)
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPath, err)
	}

	buildDir, err := os.MkdirTemp(b.server.buildDir, "zb-build-"+drvName+"-*")
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPath, err)
	}
	startedRun := false
	defer func() {
		if err != nil && startedRun && keepFailed {
			if b.server.allowKeepFailed {
				log.Infof(ctx, "Build of %s failed and user requested build directory %s be kept", drvPath, buildDir)
				if runtime.GOOS != "windows" {
					if err := os.Chmod(buildDir, 0o755); err != nil {
						log.Warnf(ctx, "Unable to make %s readable: %v", buildDir, err)
					}
				}
				return
			}
			log.Debugf(ctx, "Build of %s failed and user requested build directory be kept, but server policy is to discard.", drvPath)
		}
		if err := os.RemoveAll(buildDir); err != nil {
			log.Warnf(ctx, "Failed to clean up %s: %v", buildDir, err)
		}
	}()
	if buildUser != nil {
		if err := os.Chown(buildDir, buildUser.UID, -1); err != nil {
			return nil, fmt.Errorf("build %s: %v", drvPath, err)
		}
	}
	logFile, err := createBuilderLog(b.server.logDir, b.id, drvPath)
	if err != nil {
		return nil, fmt.Errorf("build %s: %v", drvPath, err)
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			log.Warnf(ctx, "Closing build log for %s: %v", drvPath, err)
		}
	}()

	r := newReplacer(xiter.Chain2(
		outputPathRewrites(outPaths),
		maps.All(inputRewrites),
	))
	expandedDrv := drv.ReplaceStrings(r)

	log.Debugf(ctx, "Starting builder for %s...", drvPath)
	if err := recordBuilderStart(conn, buildResultID, time.Now()); err != nil {
		log.Warnf(ctx, "For %s: %v", drvPath, err)
	}
	startedRun = true
	builderError := f(ctx, &builderInvocation{
		derivation:     expandedDrv,
		derivationPath: drvPath,
		outputPaths:    outPaths,

		realStoreDir: b.server.realDir,
		buildDir:     buildDir,
		logWriter:    logFile,
		user:         buildUser,
		sandboxPaths: filterSandboxPaths(b.server.sandboxPaths, drv.Env[buildSystemDepsVar]),
		cores:        b.server.coresPerBuild,

		lookup: b.lookup,
		closure: func(path zbstore.Path, yield func(zbstore.Path) bool) error {
			pe := pathAndEquivalenceClass{path: path}
			return closurePaths(conn, pe, func(pe pathAndEquivalenceClass) bool {
				return yield(pe.path)
			})
		},
	})
	builderEndTime := time.Now()

	if builderError == nil {
		// Verify that builder produced all outputs.
		for outputName, outputPath := range outPaths {
			if _, err := os.Lstat(b.server.realPath(outputPath)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					builderError = builderFailure{fmt.Errorf("builder failed to produce output $%s", outputName)}
				} else {
					builderError = builderFailure{fmt.Errorf("output $%s: %v", outputName, err)}
				}
				break
			}
		}
	}

	if builderError != nil {
		log.Debugf(ctx, "Builder for %s has failed: %v", drvPath, builderError)
		var buf []byte
		buf = append(buf, "*** Build failed"...)
		if isBuilderFailure(builderError) {
			// Internal errors are appended to the log during [finalizeBuildResult].
			buf = append(buf, ": "...)
			buf = append(buf, builderError.Error()...)
		}
		buf = append(buf, "\n"...)
		if keepFailed && b.server.allowKeepFailed {
			buf = append(buf, "Build directory available at "...)
			buf = append(buf, buildDir...)
			buf = append(buf, "\n"...)
		}
		if _, err := logFile.Write(buf); err != nil {
			log.Debugf(ctx, "While writing failed build directory info: %v", err)
		}
	}

	if err := recordBuilderEnd(conn, buildResultID, builderEndTime); err != nil {
		log.Warnf(ctx, "For %s: %v", drvPath, err)
	}
	if builderError != nil {
		for outName, outPath := range outPaths {
			if err := os.RemoveAll(string(outPath)); err != nil {
				ref := zbstore.OutputReference{
					DrvPath:    drvPath,
					OutputName: outName,
				}
				log.Warnf(ctx, "Clean up %v from failed build: %v", ref, err)
			}
		}
		return nil, fmt.Errorf("build %s: %w", drvPath, builderError)
	}

	log.Debugf(ctx, "Builder for %s has finished successfully", drvPath)
	return outPaths, nil
}

// runSubprocess runs a builder by running a subprocess.
// It satisfies the [runnerFunc] signature.
func runSubprocess(ctx context.Context, invocation *builderInvocation) error {
	if string(invocation.derivation.Dir) != invocation.realStoreDir {
		return fmt.Errorf("store is unsandboxed and storage directory does not match store (%s)", invocation.derivation.Dir)
	}

	c := exec.CommandContext(ctx, invocation.derivation.Builder, invocation.derivation.Args...)
	setCancelFunc(c)
	env := maps.Clone(invocation.derivation.Env)
	fillBaseEnv(env, invocation.derivation.Dir, invocation.buildDir, invocation.cores)
	for k, v := range xmaps.Sorted(env) {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Dir = invocation.buildDir
	c.Stdout = invocation.logWriter
	c.Stderr = invocation.logWriter
	c.SysProcAttr = sysProcAttrForUser(invocation.user)

	if err := c.Run(); err != nil {
		return builderFailure{err}
	}

	return nil
}

// outputPathRewrites returns an iterator of mappings of output placeholders
// to store paths in outputMap.
func outputPathRewrites(outputMap map[string]zbstore.Path) iter.Seq2[string, zbstore.Path] {
	return func(yield func(string, zbstore.Path) bool) {
		for outputName, outputPath := range outputMap {
			placeholder := zbstore.HashPlaceholder(outputName)
			if !yield(placeholder, outputPath) {
				return
			}
		}
	}
}

// derivationInputRewrites returns a substitution map
// of output placeholders to realization paths.
func derivationInputRewrites(drv *zbstore.Derivation, realization func(ref zbstore.OutputReference) (zbstore.Path, bool)) (map[string]zbstore.Path, error) {
	// TODO(maybe): Also rewrite transitive derivation hashes?
	result := make(map[string]zbstore.Path)
	for ref := range drv.InputDerivationOutputs() {
		placeholder := zbstore.UnknownCAOutputPlaceholder(ref)
		rpath, ok := realization(ref)
		if !ok {
			return nil, fmt.Errorf("compute input rewrites: missing realization for %v", ref)
		}
		result[placeholder] = rpath
	}
	return result, nil
}

// hasPlaceholders reports whether s contains any placeholders
// that would be substituted when evaluated for drv.
func hasPlaceholders(drv *zbstore.Derivation, s string) bool {
	for outputName := range drv.Outputs {
		if strings.Contains(s, zbstore.HashPlaceholder(outputName)) {
			return true
		}
	}
	for ref := range drv.InputDerivationOutputs() {
		if strings.Contains(s, zbstore.UnknownCAOutputPlaceholder(ref)) {
			return true
		}
	}
	return false
}

func tempOutputPaths(drvPath zbstore.Path, outputs map[string]*zbstore.DerivationOutputType) (map[string]zbstore.Path, error) {
	fakeDrv := &zbstore.Derivation{
		Dir:     drvPath.Dir(),
		Outputs: outputs,
	}
	var ok bool
	fakeDrv.Name, ok = drvPath.DerivationName()
	if !ok {
		return nil, fmt.Errorf("compute output paths for %s: not a derivation", drvPath)
	}

	paths := make(map[string]zbstore.Path)
	for outName := range outputs {
		if p, err := fakeDrv.OutputPath(outName); err == nil {
			paths[outName] = p
			continue
		}

		tp, err := tempPath(zbstore.OutputReference{
			DrvPath:    drvPath,
			OutputName: outName,
		})
		if err != nil {
			return nil, err
		}
		paths[outName] = tp
	}
	return paths, nil
}

// postprocess computes the metadata for a realized output
// and ensures that it is recorded in the store.
// inputs is the set of store paths that were inputs for the realized derivation.
// buildPath is the path of the store object created during realization.
// If the output is fixed, then buildPath must be the store path computed by [zbstore.Derivation.OutputPath]
// and unlockBuildPath must be the unlock function obtained from b.server.writing.
// If the outputType is floating,
// then postprocess will move the store object at buildPath to its computed path.
func (b *builder) postprocess(ctx context.Context, conn *sqlite.Conn, output zbstore.OutputReference, buildPath zbstore.Path, unlockBuildPath func(), inputs *sets.Sorted[zbstore.Path]) (*zbstore.ObjectInfo, error) {
	drv := b.derivations[output.DrvPath]
	if drv == nil {
		return nil, fmt.Errorf("post-process %v: unknown derivation", output)
	}
	outputType, hasOutput := drv.Outputs[output.OutputName]
	if !hasOutput {
		return nil, fmt.Errorf("post-process %v: no such output", output)
	}

	var info *zbstore.ObjectInfo
	var err error
	if ca, ok := outputType.FixedCA(); ok {
		if unlockBuildPath == nil {
			return nil, fmt.Errorf("post-process %v: write lock was not held", output)
		}
		defer unlockBuildPath()

		info, err = b.postprocessFixedOutput(ctx, conn, buildPath, ca)
	} else {
		if unlockBuildPath != nil {
			unlockBuildPath()
			return nil, fmt.Errorf("post-process %v: unexpected write lock", output)
		}
		// outputType has presumably been validated with [validateOutputs].
		info, err = b.postprocessFloatingOutput(ctx, conn, buildPath, inputs)
	}
	return info, err
}

// postprocessFixedOutput computes the NAR hash of the given store path
// and verifies that it matches the content address.
func (b *builder) postprocessFixedOutput(ctx context.Context, conn *sqlite.Conn, outputPath zbstore.Path, ca zbstore.ContentAddress) (info *zbstore.ObjectInfo, err error) {
	log.Debugf(ctx, "Verifying fixed output %s...", outputPath)

	realOutputPath := b.server.realPath(outputPath)
	h := nix.NewHasher(nix.SHA256)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := nar.DumpPath(io.MultiWriter(h, pw), realOutputPath); err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()
	defer func() {
		pr.Close()
		<-done
	}()

	obj := &pipeObject{
		info: zbstore.ObjectInfo{
			StorePath:      outputPath,
			ContentAddress: ca,
		},
		narContent: pr,
	}
	narSize, err := zbstore.VerifyObject(ctx, io.Discard, obj, &zbstore.ContentAddressOptions{
		CreateTemp: b.server.caCreateTemp,
		Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
	})
	if err != nil {
		return nil, err
	}

	info = new(obj.info)
	info.NARSize = narSize
	info.NARHash = h.SumHash()
	err = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)
		return insertObject(ctx, conn, info)
	}()
	if err != nil {
		err = fmt.Errorf("post-process %v: %v", outputPath, err)
	}

	freeze(ctx, realOutputPath)

	return info, nil
}

func (b *builder) postprocessFloatingOutput(ctx context.Context, conn *sqlite.Conn, buildPath zbstore.Path, inputs *sets.Sorted[zbstore.Path]) (*zbstore.ObjectInfo, error) {
	log.Debugf(ctx, "Processing floating output %s...", buildPath)
	realBuildPath := b.server.realPath(buildPath)
	scan, err := scanFloatingOutput(ctx, realBuildPath, buildPath.Digest(), inputs, b.server.caCreateTemp)
	if err != nil {
		return nil, fmt.Errorf("post-process %s: %v", buildPath, err)
	}

	finalPath, err := zbstore.FixedCAOutputPath(buildPath.Dir(), buildPath.Name(), scan.ca, scan.refs)
	if err != nil {
		return nil, fmt.Errorf("post-process %s: %v", buildPath, err)
	}
	log.Debugf(ctx, "Determined %s hashes to %s, acquring lock...", buildPath, finalPath)
	unlock, err := b.server.writing.lock(ctx, finalPath)
	if err != nil {
		return nil, fmt.Errorf("post-process %s: waiting for lock: %w", buildPath, err)
	}
	defer unlock()
	log.Debugf(ctx, "Acquired lock on %s", finalPath)

	info := &zbstore.ObjectInfo{
		StorePath:      finalPath,
		NARSize:        scan.narSize,
		References:     *scan.refs.ToSet(finalPath),
		ContentAddress: scan.ca,
	}
	if !scan.refs.Self {
		info.NARHash = scan.narHash
	}

	// Bail early if this output exists in the store already.
	realFinalPath := b.server.realPath(finalPath)
	if _, err := os.Lstat(realFinalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("post-process %s: %v", buildPath, err)
	} else if err == nil {
		log.Debugf(ctx, "%s is the same output as %s (reusing)", buildPath, finalPath)
		if err := os.RemoveAll(realBuildPath); err != nil {
			log.Warnf(ctx, "Cleanup failure: %v", err)
		}
		return info, nil
	}

	log.Debugf(ctx, "Moving %s to %s (self-references=%t)", realBuildPath, realFinalPath, scan.refs.Self)
	err = finalizeFloatingOutput(finalPath.Dir(), realBuildPath, realFinalPath, scan.analysis)
	if err != nil {
		return nil, fmt.Errorf("post-process %s: %v", buildPath, err)
	}
	if scan.refs.Self {
		h := nix.NewHasher(nix.SHA256)
		if err := nar.DumpPath(h, realFinalPath); err != nil {
			return nil, fmt.Errorf("post-process %s: %v", buildPath, err)
		}
		info.NARHash = h.SumHash()
	}

	err = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)
		return insertObject(ctx, conn, info)
	}()
	if err != nil {
		return nil, fmt.Errorf("post-process %v: %v", buildPath, err)
	}

	freeze(ctx, realFinalPath)

	return info, nil
}

type outputScanResults struct {
	ca       zbstore.ContentAddress
	narHash  nix.Hash // only filled in if refs.Self is false
	narSize  int64
	analysis *zbstore.SelfReferenceAnalysis
	refs     zbstore.References
}

// scanFloatingOutput gathers information about a newly built filesystem object.
// The digest is used to detect self references.
// closure is the transitive closure of store objects the derivation depends on,
// which form the superset of all non-self-references that the scan can detect.
func scanFloatingOutput(ctx context.Context, path string, digest string, closure *sets.Sorted[zbstore.Path], createTemp bytebuffer.Creator) (*outputScanResults, error) {
	log.Debugf(ctx, "Scanning for references in %s. Possible: %s", path, closure)
	wc := new(xio.WriteCounter)
	h := nix.NewHasher(nix.SHA256)
	refFinder := detect.NewRefFinder(func(yield func(string) bool) {
		for _, input := range closure.All() {
			if !yield(input.Digest()) {
				return
			}
		}
	})
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := nar.DumpPath(io.MultiWriter(wc, h, refFinder, pw), path); err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()
	defer func() {
		pr.Close()
		<-done
	}()

	ca, analysis, err := zbstore.SourceSHA256ContentAddress(pr, &zbstore.ContentAddressOptions{
		Digest:     digest,
		CreateTemp: createTemp,
		Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
	})
	if err != nil {
		return nil, err
	}

	refs := zbstore.References{
		Self: analysis.HasSelfReferences(),
	}
	digestsFound := refFinder.Found()
	for _, digest := range digestsFound.All() {
		// Since all store paths have the same prefix followed by digest,
		// we can use binary search on a sorted set of store paths to find the corresponding digest.
		i, ok := sort.Find(closure.Len(), func(i int) int {
			return strings.Compare(digest, closure.At(i).Digest())
		})
		if !ok {
			return nil, fmt.Errorf("scan internal error: could not find digest %q in inputs", digest)
		}
		refs.Others.Add(closure.At(i))
	}
	log.Debugf(ctx, "Found references in %s (self=%t): %s", path, refs.Self, &refs.Others)

	result := &outputScanResults{
		ca:       ca,
		narSize:  int64(*wc),
		refs:     refs,
		analysis: analysis,
	}
	if !refs.Self {
		result.narHash = h.SumHash()
	}
	return result, nil
}

// finalizeFloatingOutput moves a store object on the local filesystem to its final location,
// rewriting any self references as needed.
// The last path element of each path must be a valid store path name,
// the object names must be identical.
// dir is used purely for error messages.
func finalizeFloatingOutput(dir zbstore.Directory, buildPath, finalPath string, analysis *zbstore.SelfReferenceAnalysis) error {
	fakeBuildPath, err := dir.Object(filepath.Base(buildPath))
	if err != nil {
		return fmt.Errorf("move %s to %s: %v", buildPath, finalPath, err)
	}
	fakeFinalPath, err := dir.Object(filepath.Base(finalPath))
	if err != nil {
		return fmt.Errorf("move %s to %s: %v", buildPath, finalPath, err)
	}
	if fakeBuildPath.Name() != fakeFinalPath.Name() {
		return fmt.Errorf("move %s to %s: object names do not match", buildPath, finalPath)
	}

	for i := range analysis.Paths {
		hdr := &analysis.Paths[i]
		err := rewriteAtPath(
			filepath.Join(buildPath, filepath.FromSlash(hdr.Path)),
			hdr.ContentOffset,
			fakeFinalPath.Digest(),
			analysis.RewritesInRange(hdr.ContentOffset, hdr.ContentOffset+hdr.Size),
		)
		if err != nil {
			return fmt.Errorf("move %s to %s: %v", buildPath, finalPath, err)
		}
	}

	return os.Rename(buildPath, finalPath)
}

func rewriteAtPath(path string, baseOffset int64, newDigest string, rewriters []zbstore.Rewriter) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch info.Mode().Type() {
	case 0:
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		writeError := zbstore.Rewrite(f, baseOffset, newDigest, rewriters)
		closeError := f.Close()
		if writeError != nil {
			return fmt.Errorf("rewrite %s: %v", path, writeError)
		}
		if closeError != nil {
			return fmt.Errorf("rewrite %s: %v", path, closeError)
		}
	case os.ModeSymlink:
		oldTarget, err := os.Readlink(path)
		if err != nil {
			return err
		}
		buf := bytebuffer.New([]byte(oldTarget))
		if err := zbstore.Rewrite(buf, baseOffset, newDigest, rewriters); err != nil {
			return err
		}
		sb := new(strings.Builder)
		sb.Grow(int(buf.Size()))
		buf.Seek(0, io.SeekStart)
		buf.WriteTo(sb)
		newTarget := sb.String()
		if newTarget == oldTarget {
			// Nothing to do.
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace symlink %s: %v", path, err)
		}
		if err := os.Symlink(newTarget, path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("rewrite %s: unknown file type", path)
	}
	return nil
}

// recordRealizations calls [recordRealizations] and [recordBuildOutputs] in a transaction
// and on success, saves the realizations into b.realizations.
// The outputs must exist in the store.
func (b *builder) recordRealizations(ctx context.Context, conn *sqlite.Conn, buildResultID int64, outputs zbstore.RealizationMap) (err error) {
	if outputs.IsEmpty() {
		return nil
	}

	if log.IsEnabled(log.Debug) {
		outputPaths := make(map[string]zbstore.Path)
		for ref, r := range outputs.All() {
			outputPaths[ref.OutputName] = r.OutputPath
		}
		log.Debugf(ctx, "Recording realizations for %v: %s", outputs.DerivationHash, formatOutputPaths(outputPaths))
	}

	if b.server.upload != nil {
		srv := b.server
		srv.detachFromBuild(ctx, func(ctx context.Context) {
			srv.uploadRealizations(ctx, outputs)
		})
	}

	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("record realizations for %v: %v", outputs.DerivationHash, err)
	}
	defer endFn(&err)
	if err := recordRealizations(conn, outputs.All()); err != nil {
		return err
	}
	buildOutputs := func(yield func(string, zbstore.Path) bool) {
		for ref, r := range outputs.All() {
			if !yield(ref.OutputName, r.OutputPath) {
				return
			}
		}
	}
	if err := setBuildResultOutputs(conn, buildResultID, buildOutputs); err != nil {
		return err
	}

	for ref, r := range outputs.All() {
		closure := make(map[zbstore.Path]sets.Set[equivalenceClass])
		eqClass := realizationOutputReferenceKey(ref)
		pe := pathAndEquivalenceClass{path: r.OutputPath, equivalenceClass: eqClass}
		err := closurePaths(conn, pe, func(pe pathAndEquivalenceClass) bool {
			addToMultiMap(closure, pe.path, pe.equivalenceClass)
			return true
		})
		if err != nil {
			return err
		}
		b.realizations[eqClass] = cachedRealization{
			path:    r.OutputPath,
			closure: closure,
		}
	}
	return nil
}

func canBuildLocally(drv *zbstore.Derivation) bool {
	if drv.System == builtinSystem {
		return true
	}
	host := system.Current()
	want, err := system.Parse(drv.System)
	if err != nil {
		return false
	}

	// If the OS/architecture pair matches exactly, don't bother with anything else.
	if host.OS == want.OS && host.Arch == want.Arch {
		return true
	}

	// Perform a fuzzy match on operating systems and architectures we know about.
	sameOS := want.OS.IsMacOS() && host.OS.IsMacOS() ||
		want.OS.IsLinux() && host.OS.IsLinux() ||
		want.OS.IsWindows() && host.OS.IsWindows()
	if !sameOS {
		return false
	}
	// TODO(someday): There's probably more subtlety to the ARM comparison.
	sameFamily := want.Arch.IsX86() && host.Arch.IsX86() ||
		want.Arch.IsARM() && host.Arch.IsARM() ||
		want.Arch.IsRISCV() && host.Arch.IsRISCV()
	if !sameFamily {
		return false
	}
	return host.Arch.Is64Bit() && (want.Arch.Is64Bit() || want.Arch.Is32Bit()) ||
		host.Arch.Is32Bit() && want.Arch.Is32Bit()
}

// filterSandboxPaths computes the final mapping of paths to make available to the sandbox
// based on the __buildSystemDeps value in the derivation.
// If a path in depsList does not exist in sandboxPaths, it is ignored.
func filterSandboxPaths(sandboxPaths map[string]SandboxPath, depsList string) map[string]string {
	if len(sandboxPaths) == 0 {
		return nil
	}
	result := make(map[string]string, len(sandboxPaths))
	for path, opts := range sandboxPaths {
		if opts.AlwaysPresent {
			result[path] = cmp.Or(opts.Path, path)
		}
	}
	for path := range strings.FieldsSeq(depsList) {
		if opts, ok := sandboxPaths[path]; ok && !xmaps.HasKey(result, path) {
			result[path] = cmp.Or(opts.Path, path)
		}
	}
	return result
}

// tempPath generates a [zbstore.Path] that can be used as a temporary build path
// for the given derivation output.
// The path will be unique across the store,
// assuming SHA-256 hash collisions cannot occur.
// tempPath is deterministic:
// given the same drvPath and outputName,
// it will return the same path.
func tempPath(ref zbstore.OutputReference) (zbstore.Path, error) {
	name, err := ref.Suffix()
	if err != nil {
		return "", fmt.Errorf("make build temp path: %v", err)
	}
	h := sha256.New()
	io.WriteString(h, "rewrite:")
	io.WriteString(h, string(ref.DrvPath))
	io.WriteString(h, ":name:")
	io.WriteString(h, ref.OutputName)
	h2 := nix.NewHash(nix.SHA256, make([]byte, nix.SHA256.Size()))
	dir := ref.DrvPath.Dir()
	digest := storepath.MakeDigest(h, string(dir), h2, name)
	p, err := dir.Object(digest + "-" + name)
	if err != nil {
		return "", fmt.Errorf("make build temp path for %v: %v", ref, err)
	}
	return p, nil
}

func joinStrings[T ~string, S ~[]T](elems S, sep string) string {
	switch len(elems) {
	case 0:
		return ""
	case 1:
		return string(elems[0])
	}

	var n int
	if len(sep) > 0 {
		n += len(sep) * (len(elems) - 1)
	}
	for _, elem := range elems {
		n += len(elem)
	}

	var b strings.Builder
	b.Grow(n)
	b.WriteString(string(elems[0]))
	for _, s := range elems[1:] {
		b.WriteString(sep)
		b.WriteString(string(s))
	}
	return b.String()
}

func formatOutputPaths(m map[string]zbstore.Path) string {
	sb := new(strings.Builder)
	for i, outputName := range xmaps.SortedKeys(m) {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(outputName)
		sb.WriteString(" -> ")
		sb.WriteString(string(m[outputName]))
	}
	return sb.String()
}

func newReplacer[K, V ~string](rewrites iter.Seq2[K, V]) *strings.Replacer {
	var args []string
	for k, v := range rewrites {
		args = append(args, string(k), string(v))
	}
	return strings.NewReplacer(args...)
}

type builderFailure struct {
	err error
}

func (bf builderFailure) Error() string { return bf.err.Error() }
func (bf builderFailure) Unwrap() error { return bf.err }

func isBuilderFailure(err error) bool {
	_, ok := errors.AsType[builderFailure](err)
	return ok
}
