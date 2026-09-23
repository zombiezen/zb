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
	t.Parallel()

	testDataDir := filepath.Join("testdata", "TestAnalyze")
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
				Want map[string]struct {
					Dependents  []string
					UsedOutputs []string
					Want        bool
				}
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

			got, err := analyze(derivations, desiredOutputs)
			if err != nil {
				t.Fatal("analyze:", err)
			}

			if diff := cmp.Diff(got.want, desiredOutputs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("graph.want (-want +got):\n%s", diff)
			}

			for name, want := range test.Want {
				drvPath := store.Rewrites[name]
				wantNode := &dependencyGraphNode{
					derivation:  derivations[drvPath],
					usedOutputs: make(sets.Set[unique.Handle[string]]),
				}
				if drvPath == "" || wantNode.derivation == nil {
					t.Errorf("Want[%+q]: unknown derivation", name)
					continue
				}
				for _, outputName := range want.UsedOutputs {
					wantNode.usedOutputs.Add(unique.Make(outputName))
				}
				if len(want.Dependents) > 0 {
					wantNode.dependents = make(sets.Set[zbstore.Path], len(want.Dependents))
					for _, depName := range want.Dependents {
						dep := store.Rewrites[depName]
						if dep == "" {
							t.Errorf("Want[%+q].Dependents: unknown derivation %+q", name, depName)
							continue
						}
						wantNode.dependents.Add(dep)
					}
				}

				diff := cmp.Diff(
					wantNode, got.nodes[drvPath],
					cmp.AllowUnexported(dependencyGraphNode{}),
					cmp.FilterPath(
						func(p cmp.Path) bool {
							return p.Index(-2).Type() == reflect.TypeFor[dependencyGraphNode]() &&
								p.Last().(cmp.StructField).Name() == "derivation"
						},
						cmp.Comparer(func(drv1, drv2 *zbstore.Derivation) bool {
							// We specifically want pointer identity here: don't compare deeper.
							return drv1 == drv2
						}),
					),
					cmpopts.EquateEmpty(),
				)
				if diff != "" {
					t.Errorf("graph.nodes[%+q] (-want +got):\n%s", drvPath, diff)
				}
			}

			for drvPath := range got.nodes {
				name, ok := store.OriginalObjectName(drvPath)
				_, inWant := test.Want[name]
				if !ok || !inWant {
					t.Errorf("graph.nodes has unknown key %+q", drvPath)
				}
			}
		})
	}
}

func TestNewDependencyOrderIterator(t *testing.T) {
	tests := []struct {
		name           string
		derivations    []*zbstore.Derivation
		desiredOutputs map[string]sets.Set[string]
		roots          []string
		want           []string
	}{
		{
			name: "Empty",
			want: []string{},
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
			roots: []string{"foo.txt", "bar.txt"},
			want:  []string{"foo.txt", "bar.txt"},
		},
		{
			name: "Chain",
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
				"foo.txt": sets.New("out"),
				"bar.txt": sets.New("out"),
			},
			roots: []string{"foo.txt", "bar.txt"},
			want:  []string{"foo.txt", "bar.txt"},
		},
		{
			name: "Issue224",
			derivations: []*zbstore.Derivation{
				{
					Name:    "a.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:    "b.txt",
					Dir:     zbstore.DefaultUnixDirectory,
					System:  system.Current().String(),
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:   "c.txt",
					Dir:    zbstore.DefaultUnixDirectory,
					System: system.Current().String(),
					InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
						"a.txt": sets.NewSorted("out"),
						"b.txt": sets.NewSorted("out"),
					},
					Outputs: zbstore.DefaultFloatingOutput(),
				},
				{
					Name:   "d.txt",
					Dir:    zbstore.DefaultUnixDirectory,
					System: system.Current().String(),
					InputDerivations: map[zbstore.Path]*sets.Sorted[string]{
						"a.txt": sets.NewSorted("out"),
						"c.txt": sets.NewSorted("out"),
					},
					Outputs: zbstore.DefaultFloatingOutput(),
				},
			},
			desiredOutputs: map[string]sets.Set[string]{
				"d.txt": sets.New("out"),
			},
			roots: []string{"a.txt", "c.txt"},
			want:  []string{"a.txt", "c.txt", "d.txt"},
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
			g, err := analyze(derivations, desiredOutputs)
			if err != nil {
				t.Fatal(err)
			}
			roots := make(sets.Set[zbstore.Path], len(test.roots))
			for _, name := range test.roots {
				p, err := pathForDrvName(maps.Keys(derivations), name)
				if err != nil {
					t.Fatal(err)
				}
				roots.Add(p)
			}
			want := make([]zbstore.Path, 0, len(test.want))
			for _, name := range test.want {
				p, err := pathForDrvName(maps.Keys(derivations), name)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, p)
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
