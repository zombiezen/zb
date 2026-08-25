// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"

	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// DerivationExt is the file extension for a marshalled [Derivation].
const DerivationExt = ".drv"

// A Derivation represents a store derivation:
// a single, specific, constant build action.
type Derivation struct {
	// Dir is the store directory this derivation is a part of.
	Dir Directory

	// Name is the human-readable name of the derivation,
	// i.e. the part after the digest in the store object name.
	Name string
	// System is a string representing the OS and architecture tuple
	// that this derivation is intended to run on.
	System string
	// Builder is the path to the program to run the build.
	Builder string
	// Args is the list of arguments that should be passed to the builder program.
	Args []string
	// Env is the environment variables that should be passed to the builder program.
	Env map[string]string

	// InputSources is the set of source filesystem objects that this derivation depends on.
	InputSources sets.Sorted[Path]
	// InputDerivations is the set of derivations that this derivation depends on.
	// The mapped values are the set of output names that are used.
	InputDerivations map[Path]*sets.Sorted[string]
	// Outputs is the set of outputs that the derivation produces.
	Outputs map[string]*DerivationOutputType
}

// ParseDerivation parses a derivation from ATerm format.
// name should be the derivation's name as returned by [Path.DerivationName].
func ParseDerivation(dir Directory, name string, data []byte) (*Derivation, error) {
	if name == "" {
		return nil, fmt.Errorf("parse derivation: missing name")
	}
	if dir == "" {
		return nil, fmt.Errorf("parse %s derivation: missing directory", name)
	}
	drv := &Derivation{
		Dir:  dir,
		Name: name,
	}
	if err := drv.UnmarshalText(data); err != nil {
		return nil, err
	}
	return drv, nil
}

// ParseDerivationObject loads a ".drv" [Object] into memory
// and parses it as a [*Derivation].
func ParseDerivationObject(ctx context.Context, object Object) (*Derivation, error) {
	drvPath := object.Info().StorePath
	drvName, ok := drvPath.DerivationName()
	if !ok {
		return nil, fmt.Errorf("parse derivation: %s is not a %s file", drvPath, DerivationExt)
	}

	pr, pw := io.Pipe()
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		err := object.WriteNAR(ctx, pw)
		pw.CloseWithError(err)
	}()
	defer func() {
		pr.Close()
		<-writeDone
	}()

	nr := nar.NewReader(pr)
	hdr, err := nr.Next()
	if err != nil {
		return nil, fmt.Errorf("parse %s derivation: %v", drvName, err)
	}
	if !hdr.Mode.IsRegular() {
		return nil, fmt.Errorf("parse %s derivation: not a flat file", drvName)
	}
	drvData := new(bytes.Buffer)
	if _, err := io.Copy(drvData, nr); err != nil {
		return nil, fmt.Errorf("parse %s derivation: %v", drvName, err)
	}
	return ParseDerivation(drvPath.Dir(), drvName, drvData.Bytes())
}

// Export marshals the derivation to a NAR containing ATerm format
// and computes the derivation's store metadata using the given hashing algorithm.
//
// At the moment, the only supported algorithm is [nix.SHA256].
func (drv *Derivation) Export(hashType nix.HashType) (*Blob, error) {
	if drv.Name == "" {
		return nil, fmt.Errorf("export derivation: missing name")
	}
	if drv.Dir == "" {
		return nil, fmt.Errorf("export derivation %s: missing store directory", drv.Name)
	}

	drvBytes, err := drv.MarshalText()
	if err != nil {
		return nil, err
	}
	narBuffer := new(bytes.Buffer)
	narHasher := nix.NewHasher(hashType)
	nw := nar.NewWriter(io.MultiWriter(narHasher, narBuffer))
	if err := nw.WriteHeader(&nar.Header{Size: int64(len(drvBytes))}); err != nil {
		return nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	if _, err := nw.Write(drvBytes); err != nil {
		return nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	if err := nw.Close(); err != nil {
		return nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}

	caHasher := nix.NewHasher(hashType)
	caHasher.Write(drvBytes)
	blob := &Blob{
		NAR:     narBuffer.Bytes(),
		NARHash: narHasher.SumHash(),
		ExportTrailer: ExportTrailer{
			ContentAddress: nix.TextContentAddress(caHasher.SumHash()),
			References:     drv.References().Others,
		},
	}
	blob.StorePath, err = FixedCAOutputPath(
		drv.Dir,
		drv.Name+DerivationExt,
		blob.ContentAddress,
		drv.References(),
	)
	if err != nil {
		return nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	return blob, nil
}

// Clone returns a deep copy of drv.
func (drv *Derivation) Clone() *Derivation {
	drvClone := &Derivation{
		Dir:          drv.Dir,
		Name:         drv.Name,
		System:       drv.System,
		Builder:      drv.Builder,
		Args:         slices.Clone(drv.Args),
		Env:          maps.Clone(drv.Env),
		InputSources: *drv.InputSources.Clone(),
		Outputs:      maps.Clone(drv.Outputs),
	}
	if drv.InputDerivations != nil {
		drvClone.InputDerivations = make(map[Path]*sets.Sorted[string], len(drv.InputDerivations))
		for drvPath, outputNames := range drv.InputDerivations {
			drvClone.InputDerivations[drvPath] = outputNames.Clone()
		}
	}
	return drvClone
}

// ReplaceStrings returns a copy of drv
// with r.Replace applied to its builder, builder arguments, and environment variables.
func (drv *Derivation) ReplaceStrings(r Replacer) *Derivation {
	drv = drv.Clone()
	drv.Builder = r.Replace(drv.Builder)
	if len(drv.Args) > 0 {
		for i, arg := range drv.Args {
			drv.Args[i] = r.Replace(arg)
		}
	}
	oldEnv := drv.Env
	drv.Env = make(map[string]string, len(oldEnv))
	for k, v := range oldEnv {
		drv.Env[r.Replace(k)] = r.Replace(v)
	}
	return drv
}

// InputDerivationOutputs returns an iterator over the output references
// this derivation uses as inputs.
// The iterator will produce references in lexicographic order of the derivation path,
// then in lexicographic order of the output name within a derivation path.
func (drv *Derivation) InputDerivationOutputs() iter.Seq[OutputReference] {
	return func(yield func(OutputReference) bool) {
		for inputDrvPath, inputOutputNames := range xmaps.Sorted(drv.InputDerivations) {
			for _, inputOutputName := range inputOutputNames.All() {
				ref := OutputReference{
					DrvPath:    inputDrvPath,
					OutputName: inputOutputName,
				}
				if !yield(ref) {
					return
				}
			}
		}
	}
}

// References returns the set of other store paths that the derivation references.
// Derivations will never have a self-reference.
func (drv *Derivation) References() References {
	refs := References{}
	refs.Others.Grow(drv.InputSources.Len() + len(drv.InputDerivations))
	refs.Others.AddSet(&drv.InputSources)
	for input := range drv.InputDerivations {
		refs.Others.Add(input)
	}
	return refs
}

// OutputPath returns a fixed output's store object path.
// OutputPath returns an error if the output's path cannot be known ahead of realization.
func (drv *Derivation) OutputPath(outputName string) (Path, error) {
	outputType, ok := drv.Outputs[outputName]
	if !ok {
		return "", fmt.Errorf("output path for %s!%s: no such output", drv.Name, outputName)
	}
	return derivationOutputPath(drv.Dir, drv.Name, outputName, outputType)
}

// derivationOutputPath returns a fixed output's store object path
// for the given store (e.g. "/opt/zb/store"),
// derivation name (e.g. "hello"),
// and output name (e.g. "out").
func derivationOutputPath(store Directory, drvName, outputName string, t *DerivationOutputType) (Path, error) {
	if t == nil {
		return "", fmt.Errorf("output path for %s!%s: non-fixed output type", drvName, outputName)
	}
	switch t.typ {
	case fixedCAOutputType:
		name, err := outputPathName(drvName, outputName)
		if err != nil {
			return "", fmt.Errorf("output path for %s!%s: %v", drvName, outputName, err)
		}
		return FixedCAOutputPath(store, name, t.ca, References{})
	default:
		return "", fmt.Errorf("output path for %s!%s: non-fixed output type", drvName, outputName)
	}
}

// outputPathName computes the name part of the store path of the derivation output.
func outputPathName(drvName, outputName string) (string, error) {
	if drvName == "" {
		return "", fmt.Errorf("empty derivation name")
	}
	if !IsValidOutputName(outputName) {
		return "", fmt.Errorf("invalid output name %q", outputName)
	}
	if outputName == DefaultDerivationOutputName {
		return drvName, nil
	}
	return drvName + "-" + outputName, nil
}

// inferDerivationName infers the derivation name based on an output path and an output name.
func inferDerivationName(outputPath Path, outputName string) (string, error) {
	name := outputPath.Name()
	if outputName != DefaultDerivationOutputName {
		var ok bool
		name, ok = strings.CutSuffix(name, "-"+outputName)
		if !ok {
			return "", fmt.Errorf("must end in -%s", outputName)
		}
	}
	if name == "" {
		return "", fmt.Errorf("empty name")
	}
	return name, nil
}

// MarshalText converts the derivation to ATerm format.
func (drv *Derivation) MarshalText() ([]byte, error) {
	if drv.Name == "" {
		return nil, fmt.Errorf("marshal derivation: missing name")
	}
	if drv.Dir == "" {
		return nil, fmt.Errorf("marshal %s derivation: missing store directory", drv.Name)
	}

	var buf []byte
	buf = append(buf, "Derive(["...)
	for i, outName := range xmaps.SortedKeys(drv.Outputs) {
		if i > 0 {
			buf = append(buf, ',')
		}
		if !IsValidOutputName(outName) {
			return nil, fmt.Errorf("marshal %s derivation: invalid output name %q", drv.Name, outName)
		}
		var err error
		buf, err = AppendDerivationOutput(buf, drv.Dir, drv.Name, outName, drv.Outputs[outName])
		if err != nil {
			return nil, fmt.Errorf("marshal %s derivation: %v", drv.Name, err)
		}
	}

	buf = append(buf, "],["...)
	for drvPath := range drv.InputDerivations {
		if got := drvPath.Dir(); got != drv.Dir {
			return nil, fmt.Errorf("marshal %s derivation: inputs: unexpected store directory %s (using %s)",
				drv.Name, got, drv.Dir)
		}
	}
	buf = marshalInputDerivations(buf, drv.InputDerivations)

	buf = append(buf, "],["...)
	for i, src := range drv.InputSources.All() {
		if i > 0 {
			buf = append(buf, ',')
		}
		if got := src.Dir(); got != drv.Dir {
			return nil, fmt.Errorf("marshal %s derivation: inputs: unexpected store directory %s (using %s)",
				drv.Name, got, drv.Dir)
		}
		buf = aterm.AppendString(buf, string(src))
	}

	buf = append(buf, "],"...)
	buf = aterm.AppendString(buf, drv.System)
	buf = append(buf, ","...)
	buf = aterm.AppendString(buf, drv.Builder)

	buf = append(buf, ",["...)
	for i, arg := range drv.Args {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = aterm.AppendString(buf, arg)
	}

	buf = append(buf, "],["...)
	for i, k := range xmaps.SortedKeys(drv.Env) {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '(')
		buf = aterm.AppendString(buf, k)
		buf = append(buf, ',')
		buf = aterm.AppendString(buf, drv.Env[k])
		buf = append(buf, ')')
	}

	buf = append(buf, "])"...)

	return buf, nil
}

func marshalInputDerivations[K ~string](buf []byte, m map[K]*sets.Sorted[string]) []byte {
	for i, k := range xmaps.SortedKeys(m) {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '(')
		buf = aterm.AppendString(buf, string(k))
		buf = append(buf, ",["...)
		outputs := m[k]
		for j, out := range outputs.All() {
			if j > 0 {
				buf = append(buf, ',')
			}
			buf = aterm.AppendString(buf, out)
		}
		buf = append(buf, "])"...)
	}
	return buf
}

// SHA256RealizationHash computes the hash for the given derivation
// based on the realizations of its input derivations.
// This hash is intended for use in [RealizationOutputReference].
// If realization is nil, then SHA256RealizationHash will return an error
// if the derivation requires other realizations to compute its hash.
func (drv *Derivation) SHA256RealizationHash(realization func(ref OutputReference) (Path, error)) (nix.Hash, error) {
	if drv.Outputs[DefaultDerivationOutputName].IsFixed() {
		return hashDrvFixed(drv)
	}

	if realization == nil {
		realization = func(ref OutputReference) (Path, error) {
			return "", fmt.Errorf("missing realization for %v", ref)
		}
	}
	rewrites, err := derivationInputRewrites(drv, realization)
	if err != nil {
		return nix.Hash{}, fmt.Errorf("hash derivation: %v", err)
	}
	expandedDrv := drv.ReplaceStrings(newReplacer(maps.All(rewrites)))
	expandedDrv.InputDerivations = nil
	expandedDrv.InputSources.AddSeq(maps.Values(rewrites))
	return hashDrvFloating(expandedDrv)
}

// UnmarshalText parses a derivation from ATerm format.
// If drv.Dir or drv.Name are empty, they may be inferred from the data.
func (drv *Derivation) UnmarshalText(data []byte) (err error) {
	defer func() {
		if err != nil {
			if drv.Name == "" {
				err = fmt.Errorf("parse derivation: %v", err)
			} else {
				err = fmt.Errorf("parse %s derivation: %v", drv.Name, err)
			}
		}
	}()

	var ok bool
	data, ok = bytes.CutPrefix(data, []byte("Derive"))
	if !ok {
		return fmt.Errorf("'Derive' constructor not found")
	}
	r := bytes.NewReader(data)
	if err := drv.parseTuple(aterm.NewScanner(r)); err != nil {
		return err
	}
	if r.Len() > 0 {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func (drv *Derivation) parseTuple(s *aterm.Scanner) error {
	if _, err := expectToken(s, aterm.LParen); err != nil {
		return err
	}

	// Parse outputs.
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("outputs: %v", err)
	}
	drv.Outputs = xmaps.Init(drv.Outputs)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind == aterm.RBracket {
			break
		}
		s.UnreadToken()

		outName, outPath, outType, err := parseDerivationOutput(s)
		if err != nil {
			return err
		}
		if _, ok := drv.Outputs[outName]; ok {
			return fmt.Errorf("multiple outputs named %q", outName)
		}
		if outPath != "" {
			if drv.Dir == "" {
				drv.Dir = outPath.Dir()
			} else if outPath.Dir() != drv.Dir {
				return fmt.Errorf("parse %s output: %s not in directory %s", outName, outPath, drv.Dir)
			}
			gotName, err := inferDerivationName(outPath, outName)
			if err != nil {
				return fmt.Errorf("parse %s output: path: %s not in directory %s", outName, outPath, drv.Dir)
			}
			if drv.Name == "" {
				drv.Name = gotName
			} else if gotName != drv.Name {
				return fmt.Errorf("parse %s output: path: %s cannot be used for %s", outName, outPath, drv.Name)
			}
			wantPath, err := derivationOutputPath(drv.Dir, drv.Name, outName, outType)
			if err != nil {
				return fmt.Errorf("parse %s output: %v", outName, err)
			}
			if outPath != wantPath {
				return fmt.Errorf("parse %s output: path: %s should be %s", outName, outPath, wantPath)
			}
		}
		drv.Outputs[outName] = outType
	}

	// Parse input derivations.
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("input derivations: %v", err)
	}
	drv.InputDerivations = xmaps.Init(drv.InputDerivations)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind == aterm.RBracket {
			break
		}
		s.UnreadToken()

		drvPath, outputNames, err := parseInputDerivation(s)
		if err != nil {
			return err
		}
		if drv.Dir == "" {
			drv.Dir = drvPath.Dir()
		} else if drvPath.Dir() != drv.Dir {
			return fmt.Errorf("input derivation %s not in directory %s", drvPath, drv.Dir)
		}
		if _, ok := drv.InputDerivations[drvPath]; ok {
			return fmt.Errorf("multiple input derivations for %s", drvPath)
		}
		drv.InputDerivations[drvPath] = outputNames
	}

	// Parse input sources.
	drv.InputSources.Clear()
	err := parseStringList(s, func(val string) error {
		p, err := ParsePath(val)
		if err != nil {
			return err
		}
		if drv.Dir == "" {
			drv.Dir = p.Dir()
		} else if p.Dir() != drv.Dir {
			return fmt.Errorf("input source %s not in directory %s", p, drv.Dir)
		}
		if drv.InputSources.Has(p) {
			return fmt.Errorf("%s occurs in input sources multiple times", p)
		}
		drv.InputSources.Add(p)
		return nil
	})
	if err != nil {
		return fmt.Errorf("input sources: %v", err)
	}

	// Parse system.
	tok, err := expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("system: %v", err)
	}
	drv.System = tok.Value

	// Parse builder.
	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("builder: %v", err)
	}
	drv.Builder = tok.Value

	// Parse builder arguments.
	drv.Args = slices.Delete(drv.Args, 0, len(drv.Args))
	err = parseStringList(s, func(arg string) error {
		drv.Args = append(drv.Args, arg)
		return nil
	})
	if err != nil {
		return fmt.Errorf("builder args: %v", err)
	}

	// Parse environment.
	if err := parseEnv(&drv.Env, s); err != nil {
		return err
	}

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return err
	}
	return nil
}

func parseInputDerivation(s *aterm.Scanner) (drvPath Path, outputNames *sets.Sorted[string], err error) {
	if _, err := expectToken(s, aterm.LParen); err != nil {
		return "", nil, fmt.Errorf("parse input derivation: %v", err)
	}

	tok, err := expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation: name: %v", err)
	}
	drvPathString := tok.Value

	outputNames = new(sets.Sorted[string])
	err = parseStringList(s, func(val string) error {
		outputNames.Add(val)
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: output names: %v", drvPathString, err)
	}

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: %v", drvPathString, err)
	}

	drvPath, err = ParsePath(drvPathString)
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: %v", drvPathString, err)
	}
	return drvPath, outputNames, nil
}

func parseEnv(dst *map[string]string, s *aterm.Scanner) error {
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("env: %v", err)
	}
	*dst = xmaps.Init(*dst)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return fmt.Errorf("env: %v", err)
		}
		switch tok.Kind {
		case aterm.RBracket:
			return nil
		case aterm.LParen:
			// Carry on.
		default:
			return fmt.Errorf("env: expected ']' or '(', found %v", tok)
		}

		tok, err = expectToken(s, aterm.String)
		if err != nil {
			return fmt.Errorf("env: %v", err)
		}
		k := tok.Value
		if _, exists := (*dst)[k]; exists {
			return fmt.Errorf("env: multiple entries for %s", k)
		}

		tok, err = expectToken(s, aterm.String)
		if err != nil {
			return fmt.Errorf("env: %s: %v", k, err)
		}
		v := tok.Value

		if _, err := expectToken(s, aterm.RParen); err != nil {
			return fmt.Errorf("env: %s: %v", k, err)
		}

		(*dst)[k] = v
	}
}

// DefaultDerivationOutputName is the name of the primary output of a derivation.
// It is omitted in a number of contexts.
const DefaultDerivationOutputName = "out"

// IsValidOutputName reports whether the given string is valid as a derivation output name.
func IsValidOutputName(name string) bool {
	// TODO(someday): This should be an allow list of characters.
	return name != "" && !strings.ContainsAny(name, "^!")
}

type derivationOutputType int8

const (
	fixedCAOutputType derivationOutputType = 1 + iota
	floatingCAOutputType
)

// A DerivationOutputType describes the content addressing scheme of an output of a [Derivation].
type DerivationOutputType struct {
	typ      derivationOutputType
	ca       nix.ContentAddress
	method   contentAddressMethod
	hashAlgo nix.HashType
}

// FixedCAOutput returns a [DerivationOutputType]
// that must match the given content address assertion.
func FixedCAOutput(ca nix.ContentAddress) *DerivationOutputType {
	return &DerivationOutputType{
		typ: fixedCAOutputType,
		ca:  ca,
	}
}

// FlatFileFloatingCAOutput returns a [DerivationOutputType]
// that must be a single file
// and will be hashed with the given algorithm.
// The hash will not be known until the derivation is realized.
func FlatFileFloatingCAOutput(hashAlgo nix.HashType) *DerivationOutputType {
	return &DerivationOutputType{
		typ:      floatingCAOutputType,
		method:   flatFileIngestionMethod,
		hashAlgo: hashAlgo,
	}
}

// RecursiveFileFloatingCAOutput returns a [DerivationOutputType]
// that is hashed as a NAR with the given algorithm.
// The hash will not be known until the derivation is realized.
func RecursiveFileFloatingCAOutput(hashAlgo nix.HashType) *DerivationOutputType {
	return &DerivationOutputType{
		typ:      floatingCAOutputType,
		method:   recursiveFileIngestionMethod,
		hashAlgo: hashAlgo,
	}
}

// IsFixed reports whether the output was created by [FixedCAOutput].
func (t *DerivationOutputType) IsFixed() bool {
	if t == nil {
		return false
	}
	return t.typ == fixedCAOutputType
}

// IsFloating reports whether the output's content hash cannot be known
// until the derivation is realized.
// This is true for outputs returned by
// [FlatFileFloatingCAOutput] and [RecursiveFileFloatingCAOutput].
func (t *DerivationOutputType) IsFloating() bool {
	if t == nil {
		return false
	}
	return t.typ == floatingCAOutputType
}

// HashType returns the hash type of the derivation output, if present.
func (t *DerivationOutputType) HashType() (_ nix.HashType, ok bool) {
	switch {
	case t.IsFixed():
		return t.ca.Hash().Type(), true
	case t.IsFloating():
		return t.hashAlgo, true
	default:
		return 0, false
	}
}

// FixedCA returns a fixed hash output's content address.
// ok is true only if the output was created by [FixedCAOutput].
func (t *DerivationOutputType) FixedCA() (_ ContentAddress, ok bool) {
	if !t.IsFixed() {
		return ContentAddress{}, false
	}
	return t.ca, true
}

// IsRecursiveFile reports whether the derivation output
// uses recursive (NAR) hashing.
func (t *DerivationOutputType) IsRecursiveFile() bool {
	switch {
	case t.IsFixed():
		return t.ca.IsRecursiveFile()
	case t.IsFloating():
		return t.method == recursiveFileIngestionMethod
	default:
		return false
	}
}

// AppendDerivationOutput appends a [Derivation] output in ATerm format to the byte slice
// and returns the modified slice.
func AppendDerivationOutput(dst []byte, storeDir Directory, drvName, outName string, t *DerivationOutputType) ([]byte, error) {
	dst = append(dst, '(')
	dst = aterm.AppendString(dst, outName)
	if t == nil {
		dst = append(dst, `,"","","")`...)
		return dst, nil
	}
	switch t.typ {
	case fixedCAOutputType:
		dst = append(dst, ',')
		p, err := derivationOutputPath(storeDir, drvName, outName, t)
		if err != nil {
			return dst, fmt.Errorf("marshal %s output: %v", outName, err)
		}
		dst = aterm.AppendString(dst, string(p))
		dst = append(dst, ',')
		h := t.ca.Hash()
		dst = aterm.AppendString(dst, methodOfContentAddress(t.ca).prefix()+h.Type().String())
		dst = append(dst, ',')
		dst = aterm.AppendString(dst, h.RawBase16())
	case floatingCAOutputType:
		dst = append(dst, `,"",`...)
		dst = aterm.AppendString(dst, t.method.prefix()+t.hashAlgo.String())
		dst = append(dst, `,""`...)
	default:
		return dst, fmt.Errorf("marshal %s output: invalid type %v", outName, t.typ)
	}
	dst = append(dst, ')')
	return dst, nil
}

func parseDerivationOutput(s *aterm.Scanner) (outName string, outPath Path, out *DerivationOutputType, err error) {
	tok, err := expectToken(s, aterm.LParen)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse output: %v", err)
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse output: name: %v", err)
	}
	outName = tok.Value
	if !IsValidOutputName(outName) {
		return "", "", nil, fmt.Errorf("parse output: name: invalid name %+q", outName)
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse %s output: path: %v", outName, err)
	}
	rawOutputPath := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse %s output: hash algorithm: %v", outName, err)
	}
	caInfo := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse %s output: hash: %v", outName, err)
	}
	hashHex := tok.Value

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return "", "", nil, fmt.Errorf("parse %s output: %v", outName, err)
	}

	method, hashAlgo, err := parseHashAlgorithm(caInfo)
	if err != nil {
		return outName, "", nil, fmt.Errorf("parse %s output: hash algorithm: %v", outName, err)
	}
	if rawOutputPath != "" {
		var err error
		outPath, err = ParsePath(rawOutputPath)
		if err != nil {
			return "", "", nil, fmt.Errorf("parse %s output: %v", outName, err)
		}
		if _, err := inferDerivationName(outPath, outName); err != nil {
			return "", "", nil, fmt.Errorf("parse %s output: path %s: %v", outName, outPath, err)
		}
	}
	hashBits, err := hex.DecodeString(hashHex)
	if err != nil {
		return outName, "", nil, fmt.Errorf("parse %s output: hash: %v", outName, err)
	}
	switch {
	case outPath == "" && hashHex == "":
		out = &DerivationOutputType{
			typ:      floatingCAOutputType,
			method:   method,
			hashAlgo: hashAlgo,
		}
	case hashHex != "":
		if got, want := len(hashBits), hashAlgo.Size(); got != want {
			err = fmt.Errorf("parse %s output: hash: incorrect size (got %d bytes but %v uses %d)",
				outName, got, hashAlgo, want)
			return outName, outPath, nil, err
		}
		h := nix.NewHash(hashAlgo, hashBits)
		switch method {
		case flatFileIngestionMethod:
			out = FixedCAOutput(nix.FlatFileContentAddress(h))
		case recursiveFileIngestionMethod:
			out = FixedCAOutput(nix.RecursiveFileContentAddress(h))
		case textIngestionMethod:
			out = FixedCAOutput(nix.TextContentAddress(h))
		default:
			return outName, outPath, nil, fmt.Errorf("parse %s output: unhandled hash algorithm %q", outName, caInfo)
		}
	default:
		return outName, outPath, nil, fmt.Errorf("parse %s output: unknown type", outName)
	}
	return outName, outPath, out, nil
}

func parseHashAlgorithm(s string) (contentAddressMethod, nix.HashType, error) {
	method := flatFileIngestionMethod
	s, ok := strings.CutPrefix(s, "r:")
	if ok {
		method = recursiveFileIngestionMethod
	} else {
		s, ok = strings.CutPrefix(s, "text:")
		if ok {
			method = textIngestionMethod
		}
	}

	typ, err := nix.ParseHashType(s)
	if err != nil {
		return method, 0, err
	}
	return method, typ, nil
}

// OutputReference is a reference to an output of a derivation.
type OutputReference struct {
	DrvPath    Path
	OutputName string
}

// ParseOutputReference parses the result of [OutputReference.String]
// back into an OutputReference.
func ParseOutputReference(s string) (OutputReference, error) {
	i := strings.LastIndexByte(s, '!')
	if i < 0 {
		return OutputReference{}, fmt.Errorf("parse output reference %q: missing '!' separator", s)
	}
	result := OutputReference{OutputName: s[i+1:]}
	if !IsValidOutputName(result.OutputName) {
		return OutputReference{}, fmt.Errorf("parse output reference %q: invalid output name %q", s, result.OutputName)
	}
	var err error
	result.DrvPath, err = ParsePath(s[:i])
	if err != nil {
		return OutputReference{}, fmt.Errorf("parse output reference %q: %v", s, err)
	}
	if _, isDrv := result.DrvPath.DerivationName(); !isDrv {
		return OutputReference{}, fmt.Errorf("parse output reference %q: not a derivation", s)
	}
	return result, nil
}

// IsZero reports whether the reference is the zero value.
func (ref OutputReference) IsZero() bool {
	return ref == OutputReference{}
}

// String returns the path and the output name separated by "!".
func (ref OutputReference) String() string {
	return string(ref.DrvPath) + "!" + ref.OutputName
}

// Suffix returns the name part (as would be returned by [Path.Name])
// of the store path of the referenced output.
// Suffix returns an error if ref.DrvPath does not end in [DerivationExt]
// or ref.OutputName is not valid.
func (ref OutputReference) Suffix() (string, error) {
	drvName, ok := ref.DrvPath.DerivationName()
	if !ok {
		return "", fmt.Errorf("output path for %v: not a derivation", ref)
	}
	name, err := outputPathName(drvName, ref.OutputName)
	if err != nil {
		return "", fmt.Errorf("output path for %v: %v", ref, err)
	}
	return name, nil
}

// MarshalText returns the output reference in the same format as [OutputReference.String].
func (ref OutputReference) MarshalText() ([]byte, error) {
	if ref.DrvPath == "" {
		return nil, fmt.Errorf("marshal output reference: empty path")
	}
	if !IsValidOutputName(ref.OutputName) {
		return nil, fmt.Errorf("marshal output reference: invalid output name %q", ref.OutputName)
	}
	return []byte(ref.String()), nil
}

// UnmarshalText parses the output reference like [ParseOutputReference] into ref.
func (ref *OutputReference) UnmarshalText(text []byte) error {
	var err error
	*ref, err = ParseOutputReference(string(text))
	return err
}

// IsValidOutputPath reports whether path can be used for the given derivation output.
func IsValidOutputPath(ref OutputReference, path Path) bool {
	if path.Dir() != ref.DrvPath.Dir() {
		return false
	}
	suffix, err := ref.Suffix()
	if err != nil {
		return false
	}
	return path.Name() == suffix
}

// HashPlaceholder returns the placeholder string used in leiu of a derivation's output path.
// During a derivation's realization, the backend replaces any occurrences of the placeholder
// in the derivation's environment variables
// with the temporary output path (used until the content address stabilizes).
func HashPlaceholder(outputName string) string {
	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-output:")
	h.WriteString(outputName)
	return "/" + h.SumHash().RawBase32()
}

// UnknownCAOutputPlaceholder returns the placeholder
// for an unknown output of a content-addressed derivation.
func UnknownCAOutputPlaceholder(ref OutputReference) string {
	// We accept non-".drv" paths here for simplicity,
	// so we don't use [Path.DerivationName].
	drvName := strings.TrimSuffix(ref.DrvPath.Name(), DerivationExt)

	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-upstream-output:")
	h.WriteString(ref.DrvPath.Digest())
	h.WriteString(":")
	h.WriteString(drvName)
	if ref.OutputName != DefaultDerivationOutputName {
		h.WriteString("-")
		h.WriteString(ref.OutputName)
	}
	return "/" + h.SumHash().RawBase32()
}

func parseStringList(s *aterm.Scanner, f func(string) error) error {
	tok, err := expectToken(s, aterm.LBracket)
	if err != nil {
		return err
	}
	for {
		tok, err = s.ReadToken()
		if err != nil {
			return err
		}
		switch tok.Kind {
		case aterm.String:
			if err := f(tok.Value); err != nil {
				return err
			}
		case aterm.RBracket:
			return nil
		default:
			return fmt.Errorf("expected string or ']', found %v", tok)
		}
	}
}

func expectToken(s *aterm.Scanner, kind aterm.TokenKind) (aterm.Token, error) {
	tok, err := s.ReadToken()
	if err != nil {
		return aterm.Token{}, err
	}
	if tok.Kind != kind {
		var want string
		if kind == aterm.String {
			want = "string"
		} else {
			want = `'` + string(kind) + `'`
		}
		return tok, fmt.Errorf("expected %s, found %v", want, tok)
	}
	return tok, nil
}
