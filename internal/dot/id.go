// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package dot

import (
	"slices"
	"strings"
)

// An ID is a DOT language identifier.
type ID string

// String returns the identifier, quoted if necessary.
func (id ID) String() string {
	if !id.safe() {
		s, _ := id.AppendText(nil)
		return string(s)
	}
	return string(id)
}

// MarshalText encodes the DOT representation of the identifier
// and returns the result.
// MarshalText never returns an error.
func (id ID) MarshalText() ([]byte, error) {
	return id.AppendText(nil)
}

// AppendText appends the ID to dst with proper quoting
// and returns the new slice.
// AppendText never returns an error.
func (id ID) AppendText(dst []byte) ([]byte, error) {
	if id.safe() {
		return append(dst, id...), nil
	}
	n := len(`""`)
	for _, b := range []byte(id) {
		if b == '"' {
			n++
		}
	}
	dst = slices.Grow(dst, n)
	dst = append(dst, '"')
	for {
		i := strings.IndexByte(string(id), '"')
		if i == -1 {
			break
		}
		dst = append(dst, id[:i]...)
		dst = append(dst, `\"`...)
		id = id[i+1:]
	}
	dst = append(dst, id...)
	dst = append(dst, '"')
	return dst, nil
}

func (id ID) safe() bool {
	switch {
	case len(id) > 0 && isIDCharacter(id[0]) && !isKeyword(string(id)):
		for _, b := range []byte(id[1:]) {
			if !isIDCharacter(b) && !isDigit(b) {
				return false
			}
		}
		return true
	case len(id) >= 2 && id[0] == '-' && (id[1] == '.' || isDigit(id[1])):
		id = id[1:]
		fallthrough
	case len(id) > 0 && (isDigit(id[0]) || id[0] == '.'):
		i, f, _ := strings.Cut(string(id), ".")
		if i == "" && f == "" {
			return false
		}
		for _, c := range []byte(i) {
			if !isDigit(c) {
				return false
			}
		}
		for _, c := range []byte(f) {
			if !isDigit(c) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isIDCharacter(c byte) bool {
	return 'a' <= c && c <= 'z' ||
		'A' <= c && c <= 'Z' ||
		c == '_' ||
		c >= 0x80
}

func isDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

func isKeyword(s string) bool {
	keywords := [...]string{
		"node",
		"edge",
		"graph",
		"digraph",
		"subgraph",
		"strict",
	}
	matchesSize := false
	for _, kw := range keywords {
		if len(s) == len(kw) {
			matchesSize = true
			break
		}
	}
	if !matchesSize {
		return false
	}
	lower := []byte(s)
	for i, c := range lower {
		if 'A' <= c && c <= 'Z' {
			lower[i] = c - 'A' + 'a'
		}
	}
	for _, kw := range keywords {
		if s == kw {
			return true
		}
	}
	return false
}

// A NodeID is composed of an [ID], an optional "port" [ID], and a [CompassPoint].
// The zero value is an empty [ID] with [DefaultCompassPoint].
type NodeID struct {
	id      ID
	port    ID
	pt      CompassPoint
	hasPort bool

	_ [0]func() // Prevent comparisons.
}

// MakeNodeID returns a [NodeID] with the given [ID] and [CompassPoint].
// If [CompassPoint.IsValid] reports false, then MakeNodeID panics.
func MakeNodeID(id ID, pt CompassPoint) NodeID {
	if !pt.IsValid() {
		panic("invalid CompassPoint")
	}
	return NodeID{
		id: id,
		pt: pt,
	}
}

// MakeNodeIDWithPort returns a [NodeID] with the given [ID], port, and [CompassPoint].
// If [CompassPoint.IsValid] reports false, then MakeNodeIDWithPort panics.
func MakeNodeIDWithPort(id ID, port ID, pt CompassPoint) NodeID {
	if !pt.IsValid() {
		panic("invalid CompassPoint")
	}
	return NodeID{
		id:      id,
		port:    port,
		hasPort: true,
		pt:      pt,
	}
}

// ID returns the [ID] of a [NodeID].
func (id NodeID) ID() ID {
	return id.id
}

// Port returns the "port" of a [NodeID].
func (id NodeID) Port() (_ ID, ok bool) {
	if !id.hasPort {
		return "", false
	}
	return id.port, true
}

// CompassPoint returns the [CompassPoint] of a [NodeID].
func (id NodeID) CompassPoint() CompassPoint {
	if id.pt == "" {
		return DefaultCompassPoint
	}
	return id.pt
}

// String returns the identifier, quoted as necessary.
func (id NodeID) String() string {
	if id.id.safe() && !id.hasPort && (id.pt == "" || id.pt == DefaultCompassPoint) {
		return string(id.id)
	}
	s, _ := id.AppendText(nil)
	return string(s)
}

// MarshalText encodes the DOT representation of the identifier
// and returns the result.
// MarshalText never returns an error.
func (id NodeID) MarshalText() ([]byte, error) {
	return id.AppendText(nil)
}

// AppendText appends the node ID to dst and returns the new slice.
// AppendText never returns an error.
func (id NodeID) AppendText(dst []byte) ([]byte, error) {
	dst, _ = id.id.AppendText(dst)
	if id.hasPort {
		dst = append(dst, ':')
		dst, _ = id.port.AppendText(dst)
	}
	if id.pt != "" && id.pt != DefaultCompassPoint {
		dst = append(dst, ':')
		dst = append(dst, id.pt...)
	}
	return dst, nil
}

// CompassPoint is an enumeration of points that an edge can be attached to.
type CompassPoint string

// Defined [CompassPoint] values.
const (
	DefaultCompassPoint CompassPoint = "_"

	North     CompassPoint = "n"
	NorthEast CompassPoint = "ne"
	East      CompassPoint = "e"
	SouthEast CompassPoint = "se"
	South     CompassPoint = "s"
	SouthWest CompassPoint = "sw"
	West      CompassPoint = "w"
	NorthWest CompassPoint = "nw"
	Center    CompassPoint = "c"
)

// IsValid reports whether pt is one of the defined values.
func (pt CompassPoint) IsValid() bool {
	return pt == DefaultCompassPoint ||
		pt == North ||
		pt == NorthEast ||
		pt == East ||
		pt == SouthEast ||
		pt == South ||
		pt == SouthWest ||
		pt == West ||
		pt == NorthWest ||
		pt == Center
}
