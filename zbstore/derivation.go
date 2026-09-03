// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"context"
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
	Outputs DerivationOutputs
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
	blob, err := NewTextBlob(drv.Dir, drv.Name+DerivationExt, drvBytes, new(drv.References().Others))
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
		Outputs:      drv.Outputs,
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

// FixedOutputPath returns the derivation's expected output path.
// FixedOutputPath returns an error if [*DerivationOutputs.IsFixed] reports false.
func (drv *Derivation) FixedOutputPath() (Path, error) {
	ca, ok := drv.Outputs.FixedContentAddress()
	if !ok {
		if drv.Name == "" {
			return "", fmt.Errorf("not a fixed-output derivation")
		}
		return "", fmt.Errorf("%s.drv is not a fixed-output derivation", drv.Name)
	}
	if drv.Name == "" {
		return "", fmt.Errorf("fixed-output derivation missing name")
	}
	path, err := FixedCAOutputPath(drv.Dir, drv.Name, ca, References{})
	if err != nil {
		return "", fmt.Errorf("compute output path for %s.drv: %v", drv.Name, err)
	}
	return path, nil
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
	var err error
	buf = append(buf, "Derive("...)
	buf, err = drv.AppendOutputs(buf)
	if err != nil {
		return nil, err
	}

	buf = append(buf, ",["...)
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

// AppendOutputs appends the [*DerivationOutputs] in ATerm format
// to the byte slice and returns the modified slice.
func (drv *Derivation) AppendOutputs(dst []byte) ([]byte, error) {
	if drv.Name == "" {
		return nil, fmt.Errorf("marshal derivation: missing name")
	}
	if drv.Dir == "" {
		return nil, fmt.Errorf("marshal %s derivation: missing store directory", drv.Name)
	}
	if drv.Outputs == nil {
		return nil, fmt.Errorf("marshal %s derivation: outputs not set", drv.Name)
	}

	dst = append(dst, '[')
	if !drv.Outputs.fixed.IsZero() {
		p, err := drv.FixedOutputPath()
		if err != nil {
			return dst, fmt.Errorf("marshal %s derivation: %v", drv.Name, err)
		}
		dst = append(dst, '(')
		dst = aterm.AppendString(dst, DefaultDerivationOutputName)
		dst = append(dst, ',')
		dst = aterm.AppendString(dst, string(p))
		dst = append(dst, ',')
		h := drv.Outputs.fixed.Hash()
		dst = aterm.AppendString(dst, methodOfContentAddress(drv.Outputs.fixed).prefix()+h.Type().String())
		dst = append(dst, ',')
		dst = aterm.AppendString(dst, h.RawBase16())
		dst = append(dst, ')')
	} else {
		caInfo := recursiveFileIngestionMethod.prefix() + floatingCAHashType.String()
		for i, outputName := range drv.Outputs.names {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '(')
			dst = aterm.AppendString(dst, outputName)
			dst = append(dst, `,"",`...)
			dst = aterm.AppendString(dst, caInfo)
			dst = append(dst, `,"")`...)
		}
	}
	dst = append(dst, ']')

	return dst, nil
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
	newOutputs := new(DerivationOutputs)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind == aterm.RBracket {
			break
		}
		s.UnreadToken()

		outName, outPath, outMethod, outHash, err := parseDerivationOutput(s)
		if err != nil {
			return err
		}
		if outHash.IsZero() {
			newOutputs.names = append(newOutputs.names, outName)
			continue
		}
		if !newOutputs.fixed.IsZero() {
			return fmt.Errorf("parse %s output: cannot have more that one fixed output", outName)
		}
		if outName != DefaultDerivationOutputName {
			return fmt.Errorf("parse %s output: fixed output must be named %s", outName, DefaultDerivationOutputName)
		}
		switch outMethod {
		case textIngestionMethod:
			newOutputs.fixed = new(nix.TextContentAddress(outHash))
		case flatFileIngestionMethod:
			newOutputs.fixed = new(nix.FlatFileContentAddress(outHash))
		case recursiveFileIngestionMethod:
			newOutputs.fixed = new(nix.RecursiveFileContentAddress(outHash))
		default:
			return fmt.Errorf("parse %s output: internal error: unknown content address method %d", outName, outMethod)
		}

		if outPath != "" {
			if drv.Dir == "" {
				drv.Dir = outPath.Dir()
			} else if outPath.Dir() != drv.Dir {
				return fmt.Errorf("parse %s output: %s not in directory %s", outName, outPath, drv.Dir)
			}
			gotName, err := inferDerivationName(outPath, outName)
			if err != nil {
				return fmt.Errorf("parse %s output: path: %v", outName, err)
			}
			if drv.Name == "" {
				drv.Name = gotName
			} else if gotName != drv.Name {
				return fmt.Errorf("parse %s output: path: %s cannot be used for %s", outName, outPath, drv.Name)
			}
			wantPath, err := FixedCAOutputPath(drv.Dir, drv.Name, newOutputs.fixed, References{})
			if err != nil {
				return fmt.Errorf("parse %s output: %v", outName, err)
			}
			if outPath != wantPath {
				return fmt.Errorf("parse %s output: path: %s should be %s", outName, outPath, wantPath)
			}
		}
	}
	if err := newOutputs.init(); err != nil {
		return err
	}
	drv.Outputs = newOutputs

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
