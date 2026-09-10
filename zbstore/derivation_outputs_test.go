// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
)

func TestOutputs(t *testing.T) {
	testHash := mustParseHash(t, "sha256:c98c24b677eff44860afea6f493bbaec5bb1c4cbb209c6fc2bbb47f66ff2ad31")

	tests := []struct {
		name    string
		outputs func() (Outputs, error)

		want []*Output

		doNotWantNames          []string
		wantFixedContentAddress ContentAddress
		wantFixed               bool
		wantFloating            bool
		wantString              string
	}{
		{
			name: "Zero",
			outputs: func() (Outputs, error) {
				return Outputs{}, nil
			},
			doNotWantNames: []string{"out", "dev", "foo"},
			wantString:     "[]",
		},
		{
			name: "DefaultFloatingOutput",
			outputs: func() (Outputs, error) {
				return DefaultFloatingOutput(), nil
			},
			want: []*Output{
				{Name: DefaultOutputName},
			},
			wantFloating:   true,
			doNotWantNames: []string{"dev", "foo"},
			wantString:     `[("out","","r:sha256","")]`,
		},
		{
			name: "FloatingOutputs",
			outputs: func() (Outputs, error) {
				return FloatingOutputs(sets.New("dev", "out"))
			},
			want: []*Output{
				{Name: "dev"},
				{Name: "out"},
			},
			wantFloating:   true,
			doNotWantNames: []string{"foo"},
			wantString:     `[("dev","","r:sha256",""),("out","","r:sha256","")]`,
		},
		{
			name: "FixedOutput",
			outputs: func() (Outputs, error) {
				ca := nix.FlatFileContentAddress(testHash)
				return FixedOutput(ca), nil
			},
			want: []*Output{
				{
					Name:           "out",
					ContentAddress: nix.FlatFileContentAddress(testHash),
				},
			},
			wantFixed:               true,
			wantFixedContentAddress: nix.FlatFileContentAddress(testHash),
			doNotWantNames:          []string{"dev", "foo"},
			wantString:              `[("out","","sha256","c98c24b677eff44860afea6f493bbaec5bb1c4cbb209c6fc2bbb47f66ff2ad31")]`,
		},
	}

	t.Run("All", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}

				got := slices.Collect(outputs.All("", ""))
				if diff := cmp.Diff(test.want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("outputs.All(\"\", \"\") (-want +got):\n%s", diff)
				}

				if test.wantFixed {
					dir := DefaultUnixDirectory
					drvName := "hello.txt"
					wantPath, err := FixedCAOutputPath(dir, drvName, test.want[0].ContentAddress, References{})
					if err != nil {
						t.Error(err)
					} else {
						got := slices.Collect(outputs.All(DefaultUnixDirectory, "hello.txt"))
						want := []*Output{
							{
								Name:           test.want[0].Name,
								ContentAddress: test.want[0].ContentAddress,
								Path:           wantPath,
							},
						}
						if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
							t.Errorf("outputs.All(%q, %q) (-want +got):\n%s", dir, drvName, diff)
						}
					}
				}
			})
		}
	})

	t.Run("Names", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				got := slices.Collect(outputs.Names())
				var want []string
				for _, output := range test.want {
					want = append(want, output.Name)
				}
				if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("outputs.Names() (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("Has", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				for _, output := range test.want {
					if got := outputs.Has(output.Name); !got {
						t.Errorf("outputs.Has(%q) = false; want true", output.Name)
					}
				}
				for _, name := range test.doNotWantNames {
					if got := outputs.Has(name); got {
						t.Errorf("outputs.Has(%q) = true; want false", name)
					}
				}
			})
		}
	})

	t.Run("Len", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				if got, want := outputs.Len(), len(test.want); got != want {
					t.Errorf("outputs.Len() = %d; want %d", got, want)
				}
			})
		}
	})

	t.Run("IsFixed", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				if got := outputs.IsFixed(); got != test.wantFixed {
					t.Errorf("outputs.IsFixed() = %t; want %t", got, test.wantFixed)
				}
			})
		}
	})

	t.Run("FixedContentAddress", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				got, ok := outputs.FixedContentAddress()
				if !got.Equal(test.wantFixedContentAddress) || ok != test.wantFixed {
					t.Errorf("outputs.FixedContentAddress() = %v, %t; want %v, %t",
						got, ok, test.wantFixedContentAddress, test.wantFixed)
				}
			})
		}
	})

	t.Run("IsFloating", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				if got := outputs.IsFloating(); got != test.wantFloating {
					t.Errorf("outputs.IsFloating() = %t; want %t", got, test.wantFloating)
				}
			})
		}
	})

	t.Run("String", func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outputs, err := test.outputs()
				if err != nil {
					t.Fatal(err)
				}
				if got := outputs.String(); got != test.wantString {
					t.Errorf("outputs.String() = %s; want %s", got, test.wantString)
				}
			})
		}
	})
}
