// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unique"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/tailscale/hujson"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
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

			for name, want := range test.Want {
				drvPath := store.Rewrites[name]
				wantNode := dependencyGraphNode{
					derivation:  derivations[drvPath],
					usedOutputs: make(sets.Set[unique.Handle[string]]),
					want:        want.Want,
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
