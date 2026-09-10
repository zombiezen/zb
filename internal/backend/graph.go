// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"unique"

	"zb.256lights.llc/pkg/internal/xslices"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

// dependencyGraph stores indices of a set of derivations that are useful for realization.
type dependencyGraph struct {
	want sets.Set[zbstore.OutputReference]
	// nodes is a map of .drv file path to [*dependencyGraphNode].
	nodes map[zbstore.Path]*dependencyGraphNode
	// roots is the set of .drv files that have no input derivations.
	roots sets.Set[zbstore.Path]
}

// get gets or creates a node in graph.nodes for the given path.
// If created, then the node's derivation is set to drv.
func (graph *dependencyGraph) get(path zbstore.Path, drv *zbstore.Derivation) *dependencyGraphNode {
	node := graph.nodes[path]
	if node == nil {
		node = &dependencyGraphNode{derivation: drv}
		graph.nodes[path] = node
	}
	return node
}

// iterator returns a [*dependencyOrderIterator] over all the nodes in the graph.
func (graph *dependencyGraph) iterator() *dependencyOrderIterator {
	// We assume that graph.roots has been filled in properly,
	// so we can skip the complexity in [newDependencyOrderIterator].
	return &dependencyOrderIterator{
		graph:    graph,
		stack:    slices.AppendSeq(make([]zbstore.Path, 0, graph.roots.Len()), graph.roots.All()),
		finished: make(map[zbstore.Path]bool),
	}
}

// transitiveDependents returns a single-use iterator over the given paths
// and the transitive closure of all paths in the graph that depend on the given paths.
// After iteration is finished, the paths slice will be cleared and set to length 0.
// transitiveDependents does not guarantee ordering,
// nor does it guarantee that it will not yield the same path more than once.
func (graph *dependencyGraph) transitiveDependents(paths *[]zbstore.Path) iter.Seq[zbstore.Path] {
	return func(yield func(zbstore.Path) bool) {
		for len(*paths) > 0 {
			curr := xslices.Last(*paths)
			*paths = xslices.Pop(*paths, 1)
			if !yield(curr) {
				clear(*paths)
				*paths = (*paths)[:0]
				return
			}
			var next sets.Set[zbstore.Path]
			if node := graph.nodes[curr]; node != nil {
				next = node.dependents
			}
			*paths = slices.Grow(*paths, next.Len())
			*paths = slices.AppendSeq(*paths, next.All())
		}
	}
}

// dependencyGraphNode stores auxiliary information about a [*zbstore.Derivation].
type dependencyGraphNode struct {
	derivation *zbstore.Derivation

	// dependents is the set of paths of derivations that depend on this one.
	dependents sets.Set[zbstore.Path]
	// usedOutputs is the set of output names that a build must have realizations for.
	usedOutputs sets.Set[unique.Handle[string]]
}

// analyze produces a [dependencyGraph] for the given set of desired outputs.
func analyze(derivations map[zbstore.Path]*zbstore.Derivation, want sets.Set[zbstore.OutputReference]) (*dependencyGraph, error) {
	result := &dependencyGraph{
		want:  want,
		roots: make(sets.Set[zbstore.Path]),
		nodes: make(map[zbstore.Path]*dependencyGraphNode),
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
		result.get(ref.DrvPath, drv)

		h, err := pseudoHashDrv(drv)
		if err != nil {
			return nil, fmt.Errorf("analyze %s: %v", ref.DrvPath, err)
		}
		hk := makeHashKey(h)
		drvHashes[ref.DrvPath] = hk
		addToMultiMap(used, hk, unique.Make(ref.OutputName))

		// Fill in reverse dependency graph.
		if len(drv.InputDerivations) == 0 {
			result.roots.Add(ref.DrvPath)
		} else {
			for inputDrvPath, outputNames := range drv.InputDerivations {
				inputNode := result.get(inputDrvPath, derivations[inputDrvPath])
				if inputNode.dependents == nil {
					inputNode.dependents = make(sets.Set[zbstore.Path])
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

// newDependencyOrderIterator returns a new [*dependencyOrderIterator].
// TODO(now)
func newDependencyOrderIterator(g *dependencyGraph, roots iter.Seq[zbstore.Path]) *dependencyOrderIterator {
	type stackEntry struct {
		path     zbstore.Path
		fromRoot zbstore.Path
	}

	rootSet := make(sets.Set[zbstore.Path])
	// Maintain a slice list so we can keep the order.
	var rootList []zbstore.Path
	for root := range roots {
		// Guarantee that roots appear in the graph.
		if !rootSet.Has(root) && g.nodes[root] != nil {
			rootList = append(rootList, root)
			rootSet.Add(root)
		}
	}

	// Depth-first search over requested roots.
	var stack []stackEntry
	markSet := make(sets.Set[zbstore.Path])
	for _, root := range rootList {
		if !rootSet.Has(root) {
			// Skip if we already determined the root is a dependency of another root.
			continue
		}

		markSet.Add(root)
		node := g.nodes[root] // Guaranteed to be non-nil above.
		stack = slices.Grow(stack, len(node.derivation.InputDerivations))
		for drvPath := range node.derivation.InputDerivations {
			stack = append(stack, stackEntry{
				fromRoot: root,
				path:     drvPath,
			})
		}
		for len(stack) > 0 {
			curr := xslices.Last(stack)
			stack = xslices.Pop(stack, 1)

			markSet.Add(root)
			nextRoot := curr.fromRoot
			if rootSet.Has(curr.path) {
				// curr.fromRoot transitively depends on curr.path, another root.
				rootSet.Delete(curr.fromRoot)
				nextRoot = curr.path
			}

			if node := g.nodes[curr.path]; node != nil {
				stack = slices.Grow(stack, len(node.derivation.InputDerivations))
				for drvPath := range node.derivation.InputDerivations {
					stack = append(stack, stackEntry{
						fromRoot: nextRoot,
						path:     drvPath,
					})
				}
			}
		}
	}

	finished := make(map[zbstore.Path]bool)
	for curr := range markSet {
		node := g.nodes[curr]
		hasUnmarkedDependents := false
		for rdep := range node.dependents {
			if !markSet.Has(rdep) {
				hasUnmarkedDependents = true
				break
			}
		}
		if len(node.dependents) > 0 && !hasUnmarkedDependents {
			finished[curr] = true
		}
	}

	rootList = slices.DeleteFunc(rootList, func(p zbstore.Path) bool {
		return !rootSet.Has(p)
	})
	for root := range g.roots {
		if !rootSet.Has(root) && !markSet.Has(root) {
			rootList = append(rootList, root)
		}
	}
	return &dependencyOrderIterator{
		graph:    g,
		stack:    rootList,
		finished: finished,
	}
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
	if node == nil {
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
