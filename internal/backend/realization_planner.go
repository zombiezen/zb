// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"unique"

	"zb.256lights.llc/pkg/internal/zbstorerpc"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/sqlite"
)

// A realizationPlanner collects compatible realizations from a store database.
// These realizations can be stored in a map atomically with [*realizationPlanner.commit].
type realizationPlanner struct {
	// committed is the set of realizations that have already been selected.
	committed realizationMap
	// planned is the set of realizations that are being collected.
	planned realizationMap
	// absent is the set of keys of planned whose path does not exist in the store.
	// If absent is nil, then the planner will only pick realizations that exist in the store.
	absent sets.Set[equivalenceClass]
	// reusePolicy defines which realizations are permitted for selection.
	reusePolicy *zbstorerpc.ReusePolicy
	error       error
}

// newPlanner returns a new [*realizationPlanner].
// Any known realization may be picked (unlike with [*builder.newLocalOnlyPlanner]),
// but the returned planner will prefer to pick realizations
// whose store objects are present in the server's directory.
func (b *builder) newPlanner() *realizationPlanner {
	p := b.newLocalOnlyPlanner()
	p.absent = make(sets.Set[equivalenceClass])
	return p
}

// newLocalOnlyPlanner returns a new [*realizationPlanner]
// that will only pick realizations
// whose store objects are present in the server's directory.
func (b *builder) newLocalOnlyPlanner() *realizationPlanner {
	return &realizationPlanner{
		committed:   b.realizations,
		planned:     make(realizationMap),
		reusePolicy: b.reusePolicy,
	}
}

// has reports whether the derivation hash is present in p.committed or p.plan.
func (p *realizationPlanner) has(drvHash hashKey) bool {
	return p != nil && (p.planned.has(drvHash) || p.committed.has(drvHash))
}

// get returns the [cachedRealization] in p.committed or p.plan,
// preferring those in p.planned.
func (p *realizationPlanner) get(eqClass equivalenceClass) (_ cachedRealization, ok bool) {
	if p == nil {
		return cachedRealization{}, false
	}
	r, ok := p.planned.get(eqClass)
	if !ok {
		r, ok = p.committed.get(eqClass)
	}
	return r, ok
}

// commit copies the realizations in p.planned to p.committed,
// then clears p.planned and p.absent,
// unless p.error != nil.
func (p *realizationPlanner) commit() {
	if p == nil || p.error != nil {
		return
	}
	maps.Copy(p.committed, p.planned)
	clear(p.planned)
	p.absent.Clear()
}

// planFixed stores the realization of a fixed-output derivation in p.planned.
// planFixed will check the database for whether the object exists.
// p.error is set to an error on failure.
// If the planner is local-only and the object does not exist,
// then p.error will be set to an error that unwraps to [errRealizationNotFound].
func (p *realizationPlanner) planFixed(conn *sqlite.Conn, drvPath zbstore.Path, drvHash hashKey, outputPath zbstore.Path) {
	if p.error != nil || p.planned.has(drvHash) {
		return
	}
	dpe := derivationPathAndEquivalenceClass{
		drvPath: drvPath,
		equivalenceClass: equivalenceClass{
			drvHashKey: drvHash,
			outputName: unique.Make(zbstore.DefaultOutputName),
		},
	}
	present, err := objectExists(conn, outputPath)
	if err != nil {
		p.error = err
		return
	}
	if !present {
		if p.absent == nil {
			p.error = fmt.Errorf("insert fixed realization for %v: %w", dpe.toOutputReference(), errRealizationNotFound)
			return
		}
		p.absent.Add(dpe.equivalenceClass)
	}
	p.planned.setFixed(drvHash, outputPath)
}

// planFloating finds realizations to use from the store database
// for the given set of floating derivation outputs
// that are compatible with existing realizations in the planner
// and with elements in the set.
// planFloating sets p.error to an error that unwraps to [errRealizationNotFound]
// if no such set of realizations could be found,
// or an error that unwraps to [errMultipleRealizations] if multiple such realizations were found.
// If p.error == nil after planFloating returns,
// then [*realizationPlanner.get] will return a value for all outputs.
//
// planFloating may add realizations to p.planned for equivalence classes beyond the given one
// because selecting a realization may imply selecting realizations from its closure.
func (p *realizationPlanner) planFloating(ctx context.Context, conn *sqlite.Conn, drvPath zbstore.Path, drvHash hashKey, outputNames iter.Seq[unique.Handle[string]]) {
	if p.error != nil || p.has(drvHash) {
		return
	}
	defer func() {
		if errors.Is(p.error, errRealizationNotFound) {
			log.Debugf(ctx, "No suitable realizations exist for %v", drvPath)
		}
	}()
	rollback, err := readonlySavepoint(conn)
	if err != nil {
		p.error = err
		return
	}
	defer rollback()

	log.Debugf(ctx, "Searching for realizations for %v...", drvPath)
	for outputName := range outputNames {
		dpe := derivationPathAndEquivalenceClass{
			drvPath: drvPath,
			equivalenceClass: equivalenceClass{
				drvHashKey: drvHash,
				outputName: outputName,
			},
		}

		presentInStore, absentFromStore, err := findPossibleRealizations(ctx, conn, dpe.equivalenceClass, p.reusePolicy)
		if err != nil {
			p.error = err
			return
		}

		r := cachedRealization{outputName: outputName}
		present := false
		r.path, r.closure, err = p.pick(ctx, conn, dpe, presentInStore)
		switch {
		case err == nil:
			present = true
		case errors.Is(err, errRealizationNotFound):
			if p.absent == nil {
				p.error = err
				return
			}
			log.Debugf(ctx, "No locally present realizations found for %v. Widening...", dpe.toOutputReference())
			r.path, r.closure, err = p.pick(ctx, conn, dpe, absentFromStore)
			if errors.Is(err, errMultipleRealizations) {
				log.Debugf(ctx, "Found conflicts in absent realizations for %v (%v). Downgrading to not found.", dpe.toOutputReference(), err)
				p.error = fmt.Errorf("pick compatible realization for %v: %w", dpe.toOutputReference(), errRealizationNotFound)
				return
			}
			if err != nil {
				p.error = err
				return
			}
		default:
			p.error = err
			return
		}

		// Now that we selected our realization, fill out the closures.
		log.Debugf(ctx, "Using sole viable candidate %s for %v", r.path, dpe.toOutputReference())
		for refPath, eqClasses := range r.closure {
			refPathExists := true
			if !present {
				var err error
				refPathExists, err = objectExists(conn, refPath)
				if err != nil {
					return
				}
			}

			for eqClass := range eqClasses.All() {
				if eqClass.isZero() {
					continue
				}
				if _, exists := p.get(eqClass); exists {
					continue
				}
				pe := pathAndEquivalenceClass{
					path:             refPath,
					equivalenceClass: eqClass,
				}
				closureRealization := cachedRealization{
					path:    refPath,
					closure: make(map[zbstore.Path]sets.Set[equivalenceClass]),
				}
				err = closurePaths(conn, pe, func(pe pathAndEquivalenceClass) bool {
					addToMultiMap(closureRealization.closure, pe.path, pe.equivalenceClass)
					return true
				})
				if err != nil {
					p.error = fmt.Errorf("pick compatible realization for %v: %v", dpe.toOutputReference(), err)
					return
				}
				p.planned.insert(drvHash, r)
				if !refPathExists {
					p.absent.Add(eqClass)
				}
			}
		}
	}
}

// pick finds the only realization in the existing set
// that has closures compatible with the rest of the planner's realizations.
// pick will return an error that unwraps to [errRealizationNotFound]
// if there is no such realization,
// or an error that unwraps to [errMultipleRealizations] if there are multiple such realizations.
func (p *realizationPlanner) pick(ctx context.Context, conn *sqlite.Conn, dpe derivationPathAndEquivalenceClass, existing sets.Set[zbstore.Path]) (selectedPath zbstore.Path, closure map[zbstore.Path]sets.Set[equivalenceClass], err error) {
	rollback, err := readonlySavepoint(conn)
	if err != nil {
		return "", nil, fmt.Errorf("pick compatible realization for %v: %v", dpe.toOutputReference(), err)
	}
	defer rollback()

	closure = make(map[zbstore.Path]sets.Set[equivalenceClass])
	remaining := existing.Clone()
	for outputPath := range existing.All() {
		log.Debugf(ctx, "Checking whether %s can be used as %v...", outputPath, dpe.toOutputReference())
		remaining.Delete(outputPath)

		if !zbstore.IsValidOutputPath(dpe.toOutputReference(), outputPath) {
			continue
		}
		pe := pathAndEquivalenceClass{
			path:             outputPath,
			equivalenceClass: dpe.equivalenceClass,
		}
		clear(closure)
		canUse := true
		err := closurePaths(conn, pe, func(ref pathAndEquivalenceClass) bool {
			canUse = p.isCompatible(ref)
			if canUse {
				addToMultiMap(closure, ref.path, ref.equivalenceClass)
			} else {
				r, _ := p.get(ref.equivalenceClass)
				log.Debugf(ctx, "Cannot use %s as %v: depends on %s (need %s)",
					outputPath, dpe.toOutputReference(), ref.path, r.path)
			}
			return canUse
		})
		if err != nil {
			return "", nil, fmt.Errorf("pick compatible realization for %v: %v", dpe.toOutputReference(), err)
		}
		if canUse {
			log.Debugf(ctx, "Found %s as a candidate for %v", outputPath, dpe.toOutputReference())
			selectedPath = outputPath
			break
		}
	}

	if selectedPath == "" {
		return "", nil, fmt.Errorf("pick compatible realization for %v: %w", dpe.toOutputReference(), errRealizationNotFound)
	}

	// In the case where there are multiple valid candidates
	// that match the client's reuse policy,
	// we don't use any of them and rebuild.
	for outputPath := range remaining.All() {
		pe := pathAndEquivalenceClass{
			path:             outputPath,
			equivalenceClass: dpe.equivalenceClass,
		}
		canUse := true
		err := closurePaths(conn, pe, func(ref pathAndEquivalenceClass) bool {
			canUse = p.isCompatible(ref)
			return canUse
		})
		if err != nil {
			return "", nil, fmt.Errorf("pick compatible realization for %v: %v", dpe.toOutputReference(), err)
		}
		if canUse {
			// Multiple realizations are compatible.
			// Return none of them.
			log.Debugf(ctx, "Both %s and %s are viable candidates for %v. Returning nothing.", outputPath, selectedPath, dpe.toOutputReference())
			return "", nil, fmt.Errorf("pick compatible realization for %v: %w", dpe.toOutputReference(), errMultipleRealizations)
		}
	}

	return selectedPath, closure, nil
}

// isAvailableLocally reports whether planning succeeded
// and all planned realizations are present in the local store.
func (p *realizationPlanner) isAvailableLocally() bool {
	return p.error == nil && len(p.absent) == 0
}

// isPermanentError reports whether p.error indicates a failure
// that cannot be addressed by adding more realizations to the store database.
func (p *realizationPlanner) isPermanentError() bool {
	return p.error != nil && !errors.Is(p.error, errRealizationNotFound)
}

// isCompatible reports whether the given path can be used
// for the given (potentially zero) equivalence class
// with respect to the rest of the builder's realizations.
func (p *realizationPlanner) isCompatible(pe pathAndEquivalenceClass) bool {
	if pe.equivalenceClass.isZero() {
		// Sources can't conflict.
		return true
	}
	used, hasExisting := p.get(pe.equivalenceClass)
	return !hasExisting || pe.path == used.path
}

var (
	errRealizationNotFound  = errors.New("no suitable realizations exist")
	errMultipleRealizations = errors.New("multiple valid realizations")
)

func isRealizationPlanningError(err error) bool {
	return errors.Is(err, errRealizationNotFound) || errors.Is(err, errMultipleRealizations)
}
