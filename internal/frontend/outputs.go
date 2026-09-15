// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/luacode"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

const maxOutputsRecursionDepth = 200

// OutputMap is an immutable map of names to [*Output] values.
// Names are organized into numbered groups.
// The zero value is an empty map.
type OutputMap struct {
	groups []map[string]*Output
}

// defaultOutputKey is the canonical key for the default output in a group.
const defaultOutputKey = ""

// Default returns the [*Output] with the empty string name in the map.
// If not present, then Default returns an [*Output] that evaluates to "nil".
func (outs OutputMap) Default() *Output {
	out, _ := outs.Get(defaultOutputKey)
	return out
}

// Group returns a map of the outputs in the i'th group,
// where 1 is the first group.
// If i is non-positive or greater than the number of groups in the map,
// then Group returns an empty map.
func (outs OutputMap) Group(i int) OutputMap {
	if i < 1 || i > len(outs.groups) {
		return OutputMap{}
	}
	return OutputMap{groups: outs.groups[i-1 : i]}
}

func (outs OutputMap) groupCount() int {
	n := len(outs.groups)
	for n > 0 && len(outs.groups[n-1]) == 0 {
		n--
	}
	return n
}

// All returns an iterator over the [*Output] values in key order.
func (outs OutputMap) All() iter.Seq2[string, *Output] {
	return func(yield func(string, *Output) bool) {
		if n := outs.groupCount(); n == 0 {
			return
		} else if n == 1 {
			for _, k := range appendSortedOutputKeys([]string(nil), outs.groups[0]) {
				if !yield(k, outs.groups[0][k]) {
					return
				}
			}
			return
		}

		maxKeys := 0
		for _, group := range outs.groups {
			maxKeys = max(maxKeys, len(group))
		}
		keys := make([]string, 0, maxKeys)

		for _, k := range appendSortedOutputKeys(keys, outs.groups[0]) {
			v := outs.groups[0][k]
			if k != defaultOutputKey {
				k = "1-" + k
			}
			if !yield(k, v) {
				return
			}
		}
		for i, group := range outs.groups[1:] {
			clear(keys)
			for _, k := range appendSortedOutputKeys(keys[:0], group) {
				v := group[k]
				if k == defaultOutputKey {
					k = fmt.Sprintf("%d", i+2)
				} else {
					k = fmt.Sprintf("%d-%s", i+2, k)
				}
				if !yield(k, v) {
					return
				}
			}
		}
	}
}

// Get returns the output in the map with the given name.
// If there is no such output, Get returns ("nil", false).
func (outs OutputMap) Get(name string) (_ *Output, ok bool) {
	switch n := outs.groupCount(); {
	case n == 0:
		return nilOutput(), false
	case n == 1 || name == defaultOutputKey:
		out, ok := outs.groups[0][name]
		if !ok {
			return nilOutput(), false
		}
		return out, true
	case !('1' <= name[0] && name[0] <= '9'):
		return nilOutput(), false
	}

	seqEnd := 1
	for ; seqEnd < len(name) && name[seqEnd] != '-'; seqEnd++ {
		if !('0' <= name[seqEnd] && name[seqEnd] <= '9') {
			return nilOutput(), false
		}
	}
	keyStart := seqEnd + 1
	if keyStart > len(name) {
		// Entirely numeric.
		keyStart = len(name)
	} else if keyStart == len(name) {
		// Invalid key: we drop the hyphen if the remaining key is empty.
		return nilOutput(), false
	}

	i, err := strconv.Atoi(name[:seqEnd])
	if err != nil || i < 0 || i >= len(outs.groups) {
		return nilOutput(), false
	}
	out, ok := outs.groups[i][name[keyStart:]]
	if !ok {
		return nilOutput(), false
	}
	return out, true
}

// DerivationPaths returns the derivation paths that appear in the i'th group,
// where 1 is the first group.
// If i is non-positive or greater than the number of groups in the map,
// then DerivationPaths returns nil.
func (outs OutputMap) DerivationPaths(i int) []zbstore.Path {
	if i < 1 || i > len(outs.groups) {
		return nil
	}
	group := outs.groups[i-1]
	var n int
	for _, out := range group {
		n += len(out.refs)
	}
	if n == 0 {
		return nil
	}
	drvPaths := make([]zbstore.Path, 0, n)
	for _, out := range xmaps.Sorted(group) {
		for ref := range out.refs {
			if !slices.Contains(drvPaths, ref.DrvPath) {
				drvPaths = append(drvPaths, ref.DrvPath)
			}
		}
	}
	return drvPaths
}

// OutputReferences returns a set of all [zbstore.OutputReference] values that appear in the map.
func (outs OutputMap) OutputReferences() sets.Set[zbstore.OutputReference] {
	result := make(sets.Set[zbstore.OutputReference])
	for _, group := range outs.groups {
		for _, out := range group {
			if out != nil {
				for ref := range out.refs {
					result.Add(ref)
				}
			}
		}
	}
	return result
}

// An Output is a string with dependency information.
// It usually evaluates to a [zbstore.Path].
// The zero value represents an empty string.
type Output struct {
	template string
	paths    sets.Set[zbstore.Path]
	refs     map[zbstore.OutputReference]string
}

// nilOutput is the result of calling newOutput on tostring(nil).
var nilOutput = sync.OnceValue(func() *Output {
	nilString := luacode.Value{}.String()
	return &Output{template: nilString}
})

func newOutput(s string, sctx sets.Set[string]) (*Output, error) {
	out := &Output{template: s}
	for c := range sctx {
		v, err := parseContextString(c)
		if err != nil {
			return nil, fmt.Errorf("internal error: %v", err)
		}
		switch {
		case v.path != "" && strings.Contains(s, string(v.path)):
			if out.paths == nil {
				out.paths = make(sets.Set[zbstore.Path])
			}
			out.paths.Add(v.path)
		case !v.outputReference.IsZero():
			placeholder := v.outputReference.Placeholder()
			if strings.Contains(s, placeholder) {
				if out.refs == nil {
					out.refs = make(map[zbstore.OutputReference]string)
				}
				out.refs[v.outputReference] = placeholder
			}
		}
	}
	return out, nil
}

// OutputReferences returns a copy of the set of output references
// that the output requires for [*Output.Evaluate].
func (out *Output) OutputReferences() sets.Set[zbstore.OutputReference] {
	if out == nil || len(out.refs) == 0 {
		return nil
	}
	refs := make(sets.Set[zbstore.OutputReference], len(out.refs))
	refs.AddSeq(maps.Keys(out.refs))
	return refs
}

// Evaluate returns the string the [*Output] represents
// and the [zbstore.Path] values the string contains.
// Evaluate returns an error if it does not receive a path for a [zbstore.OutputReference]
// returned from [*Output.OutputReferences].
func (out *Output) Evaluate(m map[zbstore.OutputReference]zbstore.Path) (string, sets.Set[zbstore.Path], error) {
	if out == nil {
		return "", nil, nil
	}
	paths := out.paths.Clone()
	var replacements []string
	for ref, placeholder := range out.refs {
		p, ok := m[ref]
		if !ok {
			return "", nil, fmt.Errorf("evaluate output: missing output for %v", ref)
		}
		replacements = append(replacements, placeholder, string(p))
		paths.Add(p)
	}
	s := strings.NewReplacer(replacements...).Replace(out.template)
	return s, paths, nil
}

// String returns the string the [*Output] represents
// with placeholders replaced with the derivation path and the output name
// inside of substitution brackets.
func (out *Output) String() string {
	if out == nil {
		return ""
	}
	if len(out.refs) == 0 {
		return out.template
	}
	var replacements []string
	for ref, placeholder := range out.refs {
		replacements = append(replacements, placeholder, "⸂"+ref.String()+"⸃")
	}
	return strings.NewReplacer(replacements...).Replace(out.template)
}

// outputsMeta triggers the __outputs metamethod
// for the value at the index on the Lua stack
// with the system argument at the top of the stack.
// outputsMeta pops the system argument from the top of the stack
// and replaces it with the result of the metamethod.
// outputsMeta always pops the system argument,
// even if there is an error.
//
// The metavalue for the __outputs event can be any value.
// If the metavalue is a function, it is called with the value and system as arguments,
// and the result of the call (adjusted to one value) is the result of the operation.
// If the metavalue is nil, the result of the operation is the value.
// Otherwise, outputsMeta applies the rules above recursively to the metavalue.
func outputsMeta(ctx context.Context, l *lua.State, idx int) error {
	if !l.CheckStack(2) {
		return fmt.Errorf("'__outputs': stack overflow")
	}
	l.PushValue(idx)
	for range maxOutputsRecursionDepth {
		switch lua.Metafield(l, -1, "__outputs") {
		case lua.TypeNil:
			l.Remove(-2) // Pop system argument.
			return nil
		case lua.TypeFunction:
			l.Insert(-3)    // Move function before arguments.
			l.Rotate(-2, 1) // Swap system and self argument.
			if err := l.Call(ctx, 2, 1); err != nil {
				return err
			}
			return nil
		default:
			l.Remove(-2) // Remove previous value.
		}
	}
	l.Pop(2)
	return fmt.Errorf("'__outputs' chain too long; possible loop")
}

// objectOutputs gets the group of outputs
// for the value at the index on the Lua stack
// with the system argument at the top of the stack.
// objectOutputs pops the system argument from the top of the stack.
func objectOutputs(ctx context.Context, l *lua.State, idx int) (map[string]*Output, error) {
	systemArgument := l.Top()
	defer l.SetTop(systemArgument - 1)

	idx = l.AbsIndex(idx)
	l.PushValue(systemArgument)
	if err := outputsMeta(ctx, l, idx); err != nil {
		return nil, err
	}

	if l.Type(-1) != lua.TypeTable {
		s, sctx, err := lua.ToString(ctx, l, -1)
		if err != nil {
			return nil, err
		}
		out, err := newOutput(s, sctx)
		if err != nil {
			return nil, err
		}
		return map[string]*Output{defaultOutputKey: out}, nil
	}

	if !l.CheckStack(7) {
		return nil, fmt.Errorf("get outputs: stack overflow")
	}

	if lua.Metafield(l, -1, "__pairs") != lua.TypeNil {
		l.Insert(-2)
		if err := l.Call(ctx, 1, 3); err != nil {
			return nil, err
		}
	} else {
		l.PushPureFunction(0, baseNext)
		l.Insert(-2) // Move baseNext before table.
		l.PushNil()
	}

	result := make(map[string]*Output)
	var string1Output *Output
	var number1Output *Output
	for {
		l.PushValue(-3)  // iterator function
		l.PushValue(-3)  // state
		l.Rotate(-3, -1) // move control variable to top
		if err := l.Call(ctx, 2, 2); err != nil {
			return nil, err
		}
		keyType := l.Type(-2)
		if keyType == lua.TypeNil {
			break
		}

		l.PushValue(-2) // Copy key to avoid mutation.
		key, ok := l.ToString(-1)
		if !ok {
			l.Pop(2) // Pop value and key copy.
			continue
		}
		l.Pop(1) // Pop key copy.

		l.PushValue(systemArgument)
		out, err := defaultOutput(ctx, l, -2)
		if err != nil {
			// TODO(someday): Add key information.
			// Probably need to add a PCall?
			return nil, err
		}
		result[key] = out
		if key == "1" {
			switch keyType {
			case lua.TypeNumber:
				number1Output = out
			case lua.TypeString:
				string1Output = out
			}
		}
		l.Pop(1) // Pop value.
	}

	nextKey, stopKeys := iter.Pull(defaultOutputKeys)
	defer stopKeys()
	if firstKey, _ := nextKey(); result[firstKey] == nil {
		for {
			k, ok := nextKey()
			if !ok {
				break
			}
			if k == "1" && number1Output != nil && string1Output != nil {
				result[firstKey] = number1Output
				result["1"] = string1Output
				break
			}
			if out := result[k]; out != nil {
				result[firstKey] = out
				delete(result, k)
				break
			}
		}
	}
	return result, nil
}

func outputsFunction(ctx context.Context, l *lua.State) (int, error) {
	l.SetTop(2)
	outputs, err := objectOutputs(ctx, l, 1)
	if err != nil {
		return 0, err
	}
	l.CreateTable(0, len(outputs))
	for k, v := range outputs {
		pushOutput(l, v)
		if err := l.RawSetField(-2, k); err != nil {
			return 0, fmt.Errorf("%soutputs: %v", lua.Where(l, 1), err)
		}
	}
	return 1, nil
}

func pushOutput(l *lua.State, out *Output) {
	if out == nil {
		l.PushString("")
		return
	}
	n := out.paths.Len() + out.OutputReferences().Len()
	if n == 0 {
		l.PushString(out.template)
		return
	}
	sctx := make(sets.Set[string], n)
	for p := range out.paths {
		sctx.Add(contextValue{path: p}.String())
	}
	for ref := range out.refs {
		sctx.Add(contextValue{outputReference: ref}.String())
	}
	l.PushStringContext(out.template, sctx)
}

// defaultOutputKeys is an [iter.Seq] over the table keys for the default output
// in descending order.
func defaultOutputKeys(yield func(string) bool) {
	if !yield(defaultOutputKey) {
		return
	}
	if !yield("1") {
		return
	}
	if !yield(zbstore.DefaultOutputName) {
		return
	}
}

// defaultOutput gets the default output
// for the value at the index on the Lua stack
// with the system argument at the top of the stack.
// defaultOutput pops the system argument from the top of the stack.
func defaultOutput(ctx context.Context, l *lua.State, idx int) (*Output, error) {
	systemArgument := l.Top()
	defer l.SetTop(systemArgument - 1)

	l.PushValue(idx)
descend:
	for range maxOutputsRecursionDepth {
		l.PushValue(systemArgument)
		if err := outputsMeta(ctx, l, -2); err != nil {
			return nil, err
		}
		l.Remove(-2) // Remove old value.

		if l.Type(-1) != lua.TypeTable {
			s, sctx, err := lua.ToString(ctx, l, -1)
			if err != nil {
				return nil, err
			}
			out, err := newOutput(s, sctx)
			if err != nil {
				return nil, err
			}
			return out, nil
		}

		for name := range defaultOutputKeys {
			if i, err := lualex.ParseInt(name); err == nil {
				switch tp, err := l.Index(ctx, -1, i); {
				case err != nil:
					return nil, err
				case tp != lua.TypeNil:
					continue descend
				}
				l.Pop(1)
			}

			switch tp, err := l.Field(ctx, -1, name); {
			case err != nil:
				return nil, err
			case tp != lua.TypeNil:
				continue descend
			}
			l.Pop(1)
		}

		return nilOutput(), nil
	}

	return nil, fmt.Errorf("default output chain too long; possible loop")
}

func defaultOutputFunction(ctx context.Context, l *lua.State) (int, error) {
	l.SetTop(2)
	out, err := defaultOutput(ctx, l, 1)
	if err != nil {
		return 0, err
	}
	pushOutput(l, out)
	return 1, nil
}

// SystemTriple returns the string passed to the __outputs metamethod for a given system.
func SystemTriple(sys system.System) string {
	result := sys.Arch.String() + "-" + sys.Vendor.String() + "-" + sys.OS.String()
	if !sys.Env.IsUnknown() && !(sys.Env == "msvc" && sys.OS.IsWindows()) {
		result += "-" + sys.Env.String()
	}
	return result
}

func baseNext(ctx context.Context, l *lua.State) (int, error) {
	if got, want := l.Type(1), lua.TypeTable; got != want {
		return 0, lua.NewTypeError(l, 1, want.String())
	}
	l.SetTop(2)
	if !l.Next(1) {
		l.PushNil()
		return 1, nil
	}
	return 2, nil
}

func appendSortedOutputKeys[K ~string, V any, Map ~map[K]V, Slice ~[]K](dst Slice, m Map) Slice {
	dst = slices.Grow(dst, len(m))
	start := len(dst)
	dst = slices.AppendSeq(dst, maps.Keys(m))
	slices.SortFunc(dst[start:], func(k1, k2 K) int {
		rank1 := outputKeyRank(string(k1))
		rank2 := outputKeyRank(string(k2))
		switch {
		case rank1 != rank2:
			return cmp.Compare(rank1, rank2)
		case rank1 == 1:
			// Array indices.
			return cmp.Or(
				cmp.Compare(len(k1), len(k2)),
				cmp.Compare(k1, k2),
			)
		default:
			return cmp.Compare(k1, k2)
		}
	})
	return dst
}

func outputKeyRank(s string) int {
	if len(s) == 0 {
		return 0
	}
	if !('1' <= s[0] && s[0] <= '9') {
		return 2
	}
	for _, b := range []byte(s) {
		if !('0' <= b && b <= '9') {
			return 2
		}
	}
	return 1
}
