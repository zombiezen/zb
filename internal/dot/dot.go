// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

// Package dot provides functions for marshaling the [Graphviz DOT language].
//
// [Graphviz DOT language]: https://graphviz.org/doc/info/lang.html
package dot

import (
	"fmt"
	"iter"
	"slices"

	"zb.256lights.llc/pkg/internal/xmaps"
)

// A Graph is the top-level DOT syntax.
type Graph struct {
	Subgraph
	// Directed is true if edges in the graph are not symmetric.
	Directed bool
	// If Strict is true, there can be at most one edge with a given tail node and head node in directed graphs,
	// and at most one edge connected to the same two nodes in undirected graphs.
	Strict bool
}

// MarshalText encodes the DOT representation of the graph
// and returns the result.
func (g *Graph) MarshalText() ([]byte, error) {
	return g.AppendText(nil)
}

// AppendText appends the DOT representation of the graph to the end of dst
// and returns the updated slice.
func (g *Graph) AppendText(dst []byte) ([]byte, error) {
	if g.Strict {
		dst = append(dst, "strict "...)
	}
	if g.Directed {
		dst = append(dst, "digraph "...)
	} else {
		dst = append(dst, "graph "...)
	}
	if g.HasID() {
		dst, _ = g.ID.AppendText(dst)
		dst = append(dst, ' ')
	}
	var err error
	dst, err = appendBlock(dst, xmaps.Sorted(g.Attributes), slices.Values(g.Statements), &marshalOptions{
		directed: g.Directed,
	})
	if err != nil {
		return dst, err
	}
	dst = append(dst, '\n')
	return dst, nil
}

// A Subgraph is a [Statement] that groups nodes and edges.
type Subgraph struct {
	ID           ID
	AllowEmptyID bool

	Attributes map[ID]Attribute
	Statements []Statement
}

// HasID reports whether the subgraph has an [ID] set.
func (g *Subgraph) HasID() bool {
	return g.ID != "" || g.AllowEmptyID
}

func (g *Subgraph) appendStatement(dst []byte, opts *marshalOptions) ([]byte, error) {
	dst = opts.appendIndentation(dst)
	if g.HasID() {
		dst = append(dst, "subgraph "...)
		dst, _ = g.ID.AppendText(dst)
		dst = append(dst, ' ')
	}
	var err error
	dst, err = appendBlock(dst, xmaps.Sorted(g.Attributes), slices.Values(g.Statements), opts)
	if err != nil {
		return dst, err
	}
	dst = append(dst, '\n')
	return dst, nil
}

func appendBlock(dst []byte, attributes iter.Seq2[ID, Attribute], statements iter.Seq[Statement], opts *marshalOptions) ([]byte, error) {
	dst = append(dst, "{\n"...)
	nestedOpts := opts.indent()

	hasAttributes := false
	for k, v := range attributes {
		hasAttributes = true
		dst = nestedOpts.appendIndentation(dst)
		dst, _ = k.AppendText(dst)
		dst = append(dst, " = "...)
		dst = v.appendText(dst)
		dst = append(dst, '\n')
	}
	if hasAttributes {
		dst = append(dst, '\n')
	}

	for stmt := range statements {
		var err error
		dst, err = stmt.appendStatement(dst, nestedOpts)
		if err != nil {
			return dst, err
		}
		dst = append(dst, '\n')
	}

	dst = opts.appendIndentation(dst)
	dst = append(dst, "}"...)
	return dst, nil
}

// A Statement is a [*NodeStatement], an [*EdgeStatement], or a [*Subgraph].
type Statement interface {
	appendStatement(dst []byte, opts *marshalOptions) ([]byte, error)
}

// NodeStatement is a [Statement] that declares a node and its attributes.
type NodeStatement struct {
	ID         NodeID
	Attributes map[ID]Attribute
}

func (stmt *NodeStatement) appendStatement(dst []byte, opts *marshalOptions) ([]byte, error) {
	dst = opts.appendIndentation(dst)
	dst, _ = stmt.ID.AppendText(dst)
	if len(stmt.Attributes) > 0 {
		dst = append(dst, ' ')
		dst = appendAttributeList(dst, stmt.Attributes)
	}
	return dst, nil
}

// EdgeStatement is a [Statement] that declares an edge between two or more nodes.
type EdgeStatement struct {
	// Operands is a chain of edges.
	// Each element of the top-level slice is a set of nodes
	// that will have edges to the set of nodes in the next element.
	// Marshaling an EdgeStatement with less than two operands is an error.
	Operands   [][]*NodeStatement
	Attributes map[ID]Attribute
}

func (stmt *EdgeStatement) appendStatement(dst []byte, opts *marshalOptions) ([]byte, error) {
	if len(stmt.Operands) < 2 {
		return dst, fmt.Errorf("marshal dot edge statement: less than two operands")
	}
	appendOp := func(dst []byte, op []*NodeStatement, opts *marshalOptions) ([]byte, error) {
		if len(op) == 1 && len(op[0].Attributes) == 0 {
			return op[0].ID.AppendText(dst)
		}
		return appendBlock(dst, func(yield func(ID, Attribute) bool) {}, func(yield func(Statement) bool) {
			for _, stmt := range op {
				if !yield(stmt) {
					return
				}
			}
		}, opts)
	}

	dst = opts.appendIndentation(dst)
	var err error
	dst, err = appendOp(dst, stmt.Operands[0], opts)
	if err != nil {
		return dst, fmt.Errorf("marshal dot edge statement: %v", err)
	}
	for _, op := range stmt.Operands[1:] {
		dst = append(dst, ' ')
		dst = append(dst, opts.edgeOperator()...)
		dst = append(dst, ' ')
		dst, err = appendOp(dst, op, opts)
		if err != nil {
			return dst, fmt.Errorf("marshal dot edge statement: %v", err)
		}
	}
	if len(stmt.Attributes) > 0 {
		dst = append(dst, ' ')
		dst = appendAttributeList(dst, stmt.Attributes)
	}
	return dst, nil
}

// Attribute is an attribute value.
type Attribute struct {
	Value string
	HTML  bool
}

func (a Attribute) appendText(dst []byte) []byte {
	if a.HTML {
		dst = append(dst, '<')
		dst = append(dst, a.Value...)
		dst = append(dst, '>')
	} else {
		dst, _ = ID(a.Value).AppendText(dst)
	}
	return dst
}

func appendAttributeList(dst []byte, a map[ID]Attribute) []byte {
	dst = append(dst, '[')
	first := true
	for k, v := range xmaps.Sorted(a) {
		if first {
			first = false
		} else {
			dst = append(dst, ' ')
		}
		dst, _ = k.AppendText(dst)
		dst = append(dst, '=')
		dst = v.appendText(dst)
	}
	dst = append(dst, ']')
	return dst
}

type marshalOptions struct {
	indentation int
	directed    bool
}

func (opts *marshalOptions) indent() *marshalOptions {
	if opts == nil {
		return &marshalOptions{indentation: 1}
	}
	opts = new(*opts)
	opts.indentation++
	return opts
}

func (opts *marshalOptions) appendIndentation(dst []byte) []byte {
	if opts == nil {
		return dst
	}
	for range opts.indentation {
		dst = append(dst, '\t')
	}
	return dst
}

func (opts *marshalOptions) edgeOperator() string {
	if opts != nil && opts.directed {
		return "->"
	} else {
		return "--"
	}
}
