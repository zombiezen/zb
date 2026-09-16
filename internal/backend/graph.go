// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"unique"

	"zb.256lights.llc/pkg/internal/xslices"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

// dependencyGraph stores indices of a set of derivations that are useful for realization.
type dependencyGraph struct {
	// nodes is a map of .drv file path to [dependencyGraphNode].
	nodes map[zbstore.Path]dependencyGraphNode
}

// iterator returns a [*dependencyOrderIterator] over all the nodes in the graph.
func (graph *dependencyGraph) iterator() *dependencyOrderIterator {
	// We assume that graph.roots has been filled in properly,
	// so we can skip the complexity in [newDependencyOrderIterator].
	it := &dependencyOrderIterator{
		graph:    graph,
		finished: make(map[zbstore.Path]bool),
	}
	rootCount := 0
	for _, node := range graph.nodes {
		if len(node.derivation.InputDerivations) == 0 {
			rootCount++
		}
	}
	it.stack = make([]zbstore.Path, 0, rootCount)
	for drvPath, node := range graph.nodes {
		if len(node.derivation.InputDerivations) == 0 {
			it.stack = append(it.stack, drvPath)
		}
	}
	return it
}

// dependencyGraphNode stores auxiliary information about a [*zbstore.Derivation].
type dependencyGraphNode struct {
	derivation *zbstore.Derivation

	// dependents is the set of paths of derivations that depend on this one.
	dependents sets.Set[zbstore.Path]
	// usedOutputs is the set of output names that a build must have realizations for.
	usedOutputs sets.Set[unique.Handle[string]]
	// want is true if this derivation is named in the set of desired outputs.
	want bool
}

// analyze produces a [dependencyGraph] for the given set of desired outputs.
func analyze(derivations map[zbstore.Path]*zbstore.Derivation, want sets.Set[zbstore.OutputReference]) (*dependencyGraph, error) {
	result := &dependencyGraph{
		nodes: make(map[zbstore.Path]dependencyGraphNode),
	}
	get := func(path zbstore.Path, drv *zbstore.Derivation) dependencyGraphNode {
		node := result.nodes[path]
		if node.derivation == nil {
			node = dependencyGraphNode{derivation: drv}
			for ref := range want {
				if ref.DrvPath == path {
					node.want = true
					break
				}
			}
		}
		return node
	}

	drvHashes := make(map[zbstore.Path]hashKey)
	used := make(map[hashKey]sets.Set[unique.Handle[string]])
	stack := slices.Collect(want.All())
	for len(stack) > 0 {
		ref := xslices.Last(stack)
		stack = xslices.Pop(stack, 1)
		if _, hashed := drvHashes[ref.DrvPath]; hashed {
			// Already visited this derivation.
			continue
		}

		drv := derivations[ref.DrvPath]
		if drv == nil {
			return result, fmt.Errorf("analyze %s: unknown derivation", ref.DrvPath)
		}
		// Ensure we have a node for every derivation.
		result.nodes[ref.DrvPath] = get(ref.DrvPath, drv)

		h, err := pseudoHashDrv(drv)
		if err != nil {
			return nil, fmt.Errorf("analyze %s: %v", ref.DrvPath, err)
		}
		hk := makeHashKey(h)
		drvHashes[ref.DrvPath] = hk
		addToMultiMap(used, hk, unique.Make(ref.OutputName))

		// Fill in reverse dependency graph.
		if len(drv.InputDerivations) > 0 {
			for inputDrvPath, outputNames := range drv.InputDerivations {
				inputNode := get(inputDrvPath, derivations[inputDrvPath])
				if inputNode.dependents == nil {
					inputNode.dependents = make(sets.Set[zbstore.Path])
					result.nodes[inputDrvPath] = inputNode
				}
				inputNode.dependents.Add(ref.DrvPath)
				for outputName := range outputNames.Values() {
					stack = append(stack, zbstore.OutputReference{
						DrvPath:    inputDrvPath,
						OutputName: outputName,
					})
				}
			}
		}
	}

	// Fill in the usedOutputs as a separate pass.
	// If we had multiple derivations that are structurally the same,
	// they may use distinct output sets and we want to build the outputs.
	//
	// Multi-output derivations are particularly troublesome for us
	// because if we realize they need to be built
	// after we've already picked a realization for one of the outputs,
	// the build can invalidate the usage of other realizations.
	// (However, this can only occur if more than one output is used in the build.)
	// As long as the derivation is *mostly* deterministic,
	// then we have a good shot of being able to reuse more realizations throughout the rest of the build process
	// because of the early cutoff optimization from content-addressing.
	for drvPath, currentNode := range result.nodes {
		currentNode.usedOutputs = used[drvHashes[drvPath]]
		result.nodes[drvPath] = currentNode
	}

	return result, nil
}

func addToMultiMap[K comparable, V comparable, M ~map[K]sets.Set[V]](m M, k K, v V) {
	dst := m[k]
	if dst == nil {
		dst = make(sets.Set[V])
		m[k] = dst
	}
	dst.Add(v)
}

// dependencyOrderIterator walks a [dependencyGraph] in dependency order
// (i.e. derivations are returned after all their input derivations are processed).
type dependencyOrderIterator struct {
	graph *dependencyGraph

	mu       sync.Mutex
	stack    []zbstore.Path
	finished map[zbstore.Path]bool
	pending  int
	waiting  chan struct{}
}

// next returns the next derivation path in dependency order.
// It is the caller's responsibility to call [*dependencyOrderIterator.finish]
// on the returned path when the path has been processed.
// next will block until there is at least one derivation
// whose input derivations all have been marked with [*dependencyOrderIterator.finish].
// If there are no more derivations to visit, next returns ("", [errEndIteration]).
func (it *dependencyOrderIterator) next(ctx context.Context) (zbstore.Path, error) {
	it.mu.Lock()
	for len(it.stack) == 0 {
		if it.pending <= 0 {
			it.mu.Unlock()
			return "", errEndIteration
		}
		waiting := it.waiting
		if waiting == nil {
			waiting = make(chan struct{})
			it.waiting = waiting
		}
		it.mu.Unlock()

		select {
		case <-waiting:
			it.mu.Lock()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	p := xslices.Last(it.stack)
	it.stack = xslices.Pop(it.stack, 1)
	it.pending++
	it.mu.Unlock()
	return p, nil
}

var errEndIteration = errors.New("end iteration")

// finish marks the derivation with the given path as having finished processing,
// optionally allowing the derivation's dependents to be returned by next.
func (it *dependencyOrderIterator) finish(path zbstore.Path, processDependents bool) {
	node := it.graph.nodes[path]
	if node.derivation == nil {
		return
	}

	it.mu.Lock()
	defer it.mu.Unlock()
	if _, hasKey := it.finished[path]; hasKey {
		return
	}
	it.finished[path] = processDependents
	it.pending--

	if processDependents {
		for possible := range node.dependents {
			canAdd := true
			for input := range it.graph.nodes[possible].derivation.InputDerivations {
				if !it.finished[input] {
					canAdd = false
					break
				}
			}
			if canAdd {
				it.stack = append(it.stack, possible)
				if it.waiting != nil {
					close(it.waiting)
					it.waiting = nil
				}
			}
		}
	}

	if it.pending <= 0 && len(it.stack) == 0 && it.waiting != nil {
		close(it.waiting)
		it.waiting = nil
	}
}
