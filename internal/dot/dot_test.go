// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package dot

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var marshalTests = []struct {
	name  string
	graph *Graph
	want  string
}{
	{
		name:  "Empty",
		graph: new(Graph),
		want: "" +
			"graph {\n" +
			"}\n",
	},
	{
		name: "TwoUndirectedEdges",
		graph: &Graph{
			Subgraph: Subgraph{
				Statements: []Statement{
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{{ID: MakeNodeID("B", DefaultCompassPoint)}},
						},
					},
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{{ID: MakeNodeID("C", DefaultCompassPoint)}},
						},
					},
				},
			},
		},
		want: "" +
			"graph {\n" +
			"\tA -- B\n" +
			"\tA -- C\n" +
			"}\n",
	},
	{
		name: "TwoDirectedEdges",
		graph: &Graph{
			Directed: true,
			Subgraph: Subgraph{
				Statements: []Statement{
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{{ID: MakeNodeID("B", DefaultCompassPoint)}},
						},
					},
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{{ID: MakeNodeID("C", DefaultCompassPoint)}},
						},
					},
				},
			},
		},
		want: "" +
			"digraph {\n" +
			"\tA -> B\n" +
			"\tA -> C\n" +
			"}\n",
	},
	{
		name: "SubgraphOperand",
		graph: &Graph{
			Directed: true,
			Subgraph: Subgraph{
				Statements: []Statement{
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{
								{ID: MakeNodeID("B", DefaultCompassPoint)},
								{ID: MakeNodeID("C", DefaultCompassPoint)},
							},
						},
					},
				},
			},
		},
		want: "" +
			"digraph {\n" +
			"\tA -> {\n" +
			"\t\tB\n" +
			"\t\tC\n" +
			"\t}\n" +
			"}\n",
	},
	{
		name: "GraphAttributes",
		graph: &Graph{
			Subgraph: Subgraph{
				Attributes: map[ID]Attribute{
					"ranksep": {Value: "2.0"},
					"rankdir": {Value: "LR"},
				},
			},
		},
		want: "" +
			"graph {\n" +
			"\trankdir = LR\n" +
			"\tranksep = 2.0\n" +
			"\n" +
			"}\n",
	},
	{
		name: "NodeAttributes",
		graph: &Graph{
			Subgraph: Subgraph{
				Statements: []Statement{
					&NodeStatement{
						ID: MakeNodeID("A", DefaultCompassPoint),
						Attributes: map[ID]Attribute{
							"label": {Value: "<b>foo bar</b>", HTML: true},
							"shape": {Value: "box"},
						},
					},
				},
			},
		},
		want: "" +
			"graph {\n" +
			"\tA [label=<<b>foo bar</b>> shape=box]\n" +
			"}\n",
	},
	{
		name: "EdgeAttributes",
		graph: &Graph{
			Directed: true,
			Subgraph: Subgraph{
				Statements: []Statement{
					&EdgeStatement{
						Operands: [][]*NodeStatement{
							{{ID: MakeNodeID("A", DefaultCompassPoint)}},
							{{ID: MakeNodeID("B", DefaultCompassPoint)}},
						},
						Attributes: map[ID]Attribute{
							"headlabel": {Value: "foo bar"},
							"taillabel": {Value: "baz"},
						},
					},
				},
			},
		},
		want: "" +
			"digraph {\n" +
			"\tA -> B [headlabel=\"foo bar\" taillabel=baz]\n" +
			"}\n",
	},
}

func TestGraphMarshalText(t *testing.T) {
	for _, test := range marshalTests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.graph.AppendText(nil)
			if err != nil {
				t.Error(err)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
}

func TestSyntax(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping due to -short")
	}
	nopProgram, err := exec.LookPath("nop")
	if err != nil {
		t.Skip("Graphviz not available:", err)
	}
	for _, test := range marshalTests {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command(nopProgram, "-p")
			cmd.Stdin = strings.NewReader(test.want)
			w := t.Output()
			cmd.Stdout = w
			cmd.Stderr = w
			if err := cmd.Run(); err != nil {
				t.Error(err)
			}
		})
	}
}
