// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unique"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/tailscale/hujson"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
)

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name           string
		derivations    []*zbstore.Derivation
		desiredOutputs map[string]sets.Set[string]
		want           *dependencyGraph
	}{
		{
			name: "Empty",
			want: &dependencyGraph{},
		},
		{
			name: "SingleNode",
			derivations: []*zbstore.Derivation{
				{
					Name:    "foo.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
			},
			desiredOutputs: map[string]sets.Set[string]{
				"foo.txt": sets.New("out"),
			},
			want: &dependencyGraph{
				roots: sets.New[zbstore.Path]("foo.txt"),
				nodes: map[zbstore.Path]*dependencyGraphNode{
					"foo.txt": {
						usedOutputs: sets.New(unique.Make("out")),
					},
				},
			},
		},
		{
			name: "TwoNodes",
			derivations: []*zbstore.Derivation{
				{
					Name:    "foo.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:    "bar.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
			},
			desiredOutputs: map[string]sets.Set[string]{
				"foo.txt": sets.New("out"),
				"bar.txt": sets.New("out"),
			},
			want: &dependencyGraph{
				roots: sets.New[zbstore.Path]("foo.txt", "bar.txt"),
				nodes: map[zbstore.Path]*dependencyGraphNode{
					"foo.txt": {
						usedOutputs: sets.New(unique.Make("out")),
					},
					"bar.txt": {
						usedOutputs: sets.New(unique.Make("out")),
					},
				},
			},
		},
		{
			name: "TwoNodeChain",
			derivations: []*zbstore.Derivation{
				{
					Name:    "foo.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:   "bar.txt",
					Dir:    zbstore.DefaultUnixDirectory,
					System: system.Current().String(),
					InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
						"foo.txt": sets.NewSorted("out"),
					},
					Outputs: zbstore.DefaultFloatingOutput(),
				},
			},
			desiredOutputs: map[string]sets.Set[string]{
				"bar.txt": sets.New("out"),
			},
			want: &dependencyGraph{
				roots: sets.New[zbstore.Path]("foo.txt"),
				nodes: map[zbstore.Path]*dependencyGraphNode{
					"foo.txt": {
						dependents:  sets.New[zbstore.Path]("bar.txt"),
						usedOutputs: sets.New(unique.Make("out")),
					},
					"bar.txt": {
						usedOutputs: sets.New(unique.Make("out")),
					},
				},
			},
		},
		{
			name: "Hinge",
			derivations: []*zbstore.Derivation{
				{
					Name:    "foo.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:    "bar.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:   "baz.txt",
					Dir:    zbstore.DefaultUnixDirectory,
					System: system.Current().String(),
					InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
						"foo.txt": sets.NewSorted("out"),
						"bar.txt": sets.NewSorted("out"),
					},
					Outputs: zbstore.DefaultFloatingOutput(),
				},
			},
			desiredOutputs: map[string]sets.Set[string]{
				"baz.txt": sets.New("out"),
			},
			want: &dependencyGraph{
				roots: sets.New[zbstore.Path]("foo.txt", "bar.txt"),
				nodes: map[zbstore.Path]*dependencyGraphNode{
					"foo.txt": {
						dependents:  sets.New[zbstore.Path]("baz.txt"),
						usedOutputs: sets.New(unique.Make("out")),
					},
					"bar.txt": {
						dependents:  sets.New[zbstore.Path]("baz.txt"),
						usedOutputs: sets.New(unique.Make("out")),
					},
					"baz.txt": {
						usedOutputs: sets.New(unique.Make("out")),
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			derivations, err := rewriteDerivationsForGraphTest(test.derivations)
			if err != nil {
				t.Fatal(err)
			}
			desiredOutputs, err := rewriteDesiredOutputsForGraphTest(derivations, test.desiredOutputs)
			if err != nil {
				t.Fatal(err)
			}

			got, err := analyze(derivations, desiredOutputs)
			if err != nil {
				t.Fatal("analyze:", err)
			}

			for drvPath, drv := range derivations {
				node := got.nodes[drvPath]
				if node == nil {
					t.Errorf("analyze did not return a node for %s", drvPath)
					continue
				}
				if node.derivation == nil {
					t.Errorf("analysis node for %s did not set derivation", drvPath)
				} else if node.derivation != drv {
					t.Errorf("analysis node for %s does not match derivation", drvPath)
				}
			}

			want := new(dependencyGraph)
			*want = *test.want
			want.want = make(sets.Set[zbstore.OutputReference])
			for fakePath, outputNames := range test.desiredOutputs {
				drvPath, err := pathForDrvName(maps.Keys(derivations), string(fakePath))
				if err != nil {
					t.Fatal("want.want:", err)
				}
				for outputName := range outputNames.All() {
					want.want.Add(zbstore.OutputReference{
						DrvPath:    drvPath,
						OutputName: outputName,
					})
				}
			}
			want.nodes = make(map[zbstore.Path]*dependencyGraphNode)
			for fakePath, node := range test.want.nodes {
				drvPath, err := pathForDrvName(maps.Keys(derivations), string(fakePath))
				if err != nil {
					t.Fatal("want.nodes:", err)
				}
				nodeClone := new(dependencyGraphNode)
				*nodeClone = *node
				nodeClone.dependents, err = rewriteKeys(node.dependents, func(p zbstore.Path) (zbstore.Path, error) {
					return pathForDrvName(maps.Keys(derivations), string(p))
				})
				if err != nil {
					t.Fatal("want.nodes:", err)
				}
				want.nodes[drvPath] = nodeClone
			}
			want.roots = make(sets.Set[zbstore.Path])
			for fakePath := range test.want.roots.All() {
				drvPath, err := pathForDrvName(maps.Keys(derivations), string(fakePath))
				if err != nil {
					t.Fatal("want.roots:", err)
				}
				want.roots.Add(drvPath)
			}

			diff := cmp.Diff(
				want, got,
				cmpopts.EquateEmpty(),
				cmp.AllowUnexported(dependencyGraph{}),
				cmp.AllowUnexported(dependencyGraphNode{}),
				cmp.FilterPath(func(p cmp.Path) bool {
					return p.Index(-2).Type() == reflect.TypeFor[dependencyGraphNode]() &&
						p.Index(-1).(cmp.StructField).Name() == "derivation"
				}, cmp.Ignore()),
			)
			if diff != "" {
				t.Errorf("analysis (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewDependencyOrderIterator(t *testing.T) {
	t.Parallel()

	testDataDir := filepath.Join("testdata", "TestNewDependencyOrderIterator")
	listing, err := os.ReadDir(testDataDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range listing {
		fileName := entry.Name()
		if entry.IsDir() || strings.HasPrefix(fileName, ".") {
			continue
		}
		testName, isTXTAR := strings.CutSuffix(fileName, ".txt")
		if !isTXTAR {
			continue
		}
		fileName = filepath.Join(testDataDir, fileName)

		t.Run(testName, func(t *testing.T) {
			archive, err := txtar.ParseFile(fileName)
			if err != nil {
				t.Fatal(err)
			}
			store, err := storetest.TxtarObjects(zbstore.DefaultUnixDirectory, archive.Files)
			if err != nil {
				t.Fatalf("%s: %v", fileName, err)
			}
			derivations := make(map[zbstore.Path]*zbstore.Derivation)
			for _, object := range store.BlobSlice {
				if _, isDrv := object.StorePath.DerivationName(); isDrv {
					drv, err := zbstore.ParseDerivationObject(t.Context(), object)
					if err != nil {
						t.Fatal(err)
					}
					derivations[object.StorePath] = drv
				}
			}

			jsonData, err := hujson.Standardize(archive.Comment)
			if err != nil {
				t.Fatalf("%s: %v", fileName, err)
			}
			var test struct {
				DesiredOutputs []struct {
					DrvName    string
					OutputName string
				}
				Roots []string
				Want  []string
			}
			if err := jsonv2.Unmarshal(jsonData, &test, jsonv2.RejectUnknownMembers(true)); err != nil {
				t.Fatalf("%s: %v", fileName, err)
			}
			desiredOutputs := make(sets.Set[zbstore.OutputReference], len(test.DesiredOutputs))
			for _, ref := range test.DesiredOutputs {
				drvPath := store.Rewrites[ref.DrvName]
				if drvPath == "" {
					t.Errorf("%s: unknown derivation %+q", fileName, ref.DrvName)
					continue
				}
				desiredOutputs.Add(zbstore.OutputReference{
					DrvPath:    drvPath,
					OutputName: ref.OutputName,
				})
			}
			roots := make(sets.Set[zbstore.Path], len(test.Roots))
			for _, name := range test.Roots {
				p := store.Rewrites[name]
				if p == "" {
					t.Errorf("%s: unknown derivation %+q", fileName, name)
				}
				roots.Add(p)
			}
			want := make([]zbstore.Path, 0, len(test.Want))
			for _, name := range test.Want {
				p := store.Rewrites[name]
				if p == "" {
					t.Errorf("%s: unknown derivation %+q", fileName, name)
				}
				want = append(want, p)
			}
			if t.Failed() {
				return
			}

			g, err := analyze(derivations, desiredOutputs)
			if err != nil {
				t.Fatal(err)
			}

			ctx := t.Context()
			it := newDependencyOrderIterator(g, roots.All())
			var got []zbstore.Path
			for {
				p, err := it.next(ctx)
				if err != nil {
					if !errors.Is(err, errEndIteration) {
						t.Error("it.next(ctx):", err)
					}
					break
				}
				got = append(got, p)
				it.finish(p, true)
			}

			for i, p := range got {
				if !slices.Contains(want, p) {
					t.Errorf("unexpected path %s", p)
					continue
				}
				for dep := range derivations[p].InputDerivations {
					if j := slices.Index(got, dep); i < j {
						t.Errorf("path %s comes before %s", p, dep)
					}
				}
			}
			for _, p := range want {
				if !slices.Contains(got, p) {
					t.Errorf("missing path %s", p)
				}
			}
			if t.Failed() {
				t.Log("paths =", got)
			}
		})
	}
}

// rewriteDerivationsForGraphTest creates a map of derivations cloned from the slice
// with each key being a full store path
// and each input derivation rewritten to a full path.
// rewriteDerivationsForGraphTest returns an error if the slice is not in dependency order.
func rewriteDerivationsForGraphTest(derivations []*zbstore.Derivation) (map[zbstore.Path]*zbstore.Derivation, error) {
	rewritten := make(map[zbstore.Path]*zbstore.Derivation)
	for _, drv := range derivations {
		rewrittenInputs, err := rewriteKeys(drv.InputDerivations, func(k zbstore.Path) (zbstore.Path, error) {
			return pathForDrvName(maps.Keys(rewritten), string(k))
		})
		if err != nil {
			return nil, fmt.Errorf("%s: input derivations: %v", drv.Name, err)
		}
		drv = drv.Clone()
		drv.InputDerivations = rewrittenInputs

		obj, err := drv.Export(nix.SHA256)
		if err != nil {
			return nil, err
		}
		rewritten[obj.StorePath] = drv
	}
	return rewritten, nil
}

// rewriteDesiredOutputsForGraphTest returns a new set of output references
// based on the keys in outputs and the paths in derivations.
func rewriteDesiredOutputsForGraphTest(derivations map[zbstore.Path]*zbstore.Derivation, outputs map[string]sets.Set[string]) (sets.Set[zbstore.OutputReference], error) {
	rewritten := make(sets.Set[zbstore.OutputReference])
	for drvName, outputNames := range outputs {
		drvPath, err := pathForDrvName(maps.Keys(derivations), drvName)
		if err != nil {
			return nil, fmt.Errorf("desired outputs: %v", err)
		}
		for name := range outputNames.All() {
			rewritten.Add(zbstore.OutputReference{
				DrvPath:    drvPath,
				OutputName: name,
			})
		}
	}
	return rewritten, nil
}

// pathForDrvName returns the first path that appears in paths
// that ends with name+[zbstore.DerivationExt]
// or an error if not found.
func pathForDrvName(paths iter.Seq[zbstore.Path], name string) (zbstore.Path, error) {
	for p := range paths {
		if curr, _ := p.DerivationName(); curr == name {
			return p, nil
		}
	}
	return "", fmt.Errorf("no such derivation %s", name)
}

func rewriteKeys[K1 comparable, K2 comparable, V any, M1 ~map[K1]V](m M1, f func(K1) (K2, error)) (map[K2]V, error) {
	m2 := make(map[K2]V, len(m))
	for k, v := range m {
		k2, err := f(k)
		if err != nil {
			return nil, err
		}
		m2[k2] = v
	}
	return m2, nil
}
