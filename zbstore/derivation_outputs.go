// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
)

// DefaultOutputName is the name of the primary [*Output] in an [Outputs] set.
// It is omitted in a number of contexts.
const DefaultOutputName = "out"

// Outputs holds a set of [*Output] values for a [Derivation].
// The zero value is an empty set.
type Outputs struct {
	names []string
	fixed *nix.ContentAddress
}

// FixedOutput returns a new [Outputs] value
// for a single [*Output] named [DefaultOutputName]
// that must match the given content address assertion.
// FixedOutput panics if the [ContentAddress] is the zero value.
func FixedOutput(ca nix.ContentAddress) Outputs {
	if ca.IsZero() {
		panic("zero content address")
	}
	return Outputs{fixed: new(ca)}
}

// DefaultFloatingOutput returns a [Outputs] value
// with a single output named [DefaultOutputName].
func DefaultFloatingOutput() Outputs {
	return defaultFloatingOutput
}

var defaultFloatingOutput = Outputs{
	names: []string{DefaultOutputName},
}

// FloatingOutputs returns a new [Outputs] value with a set of output names.
// Each output will be SHA-256-hashed as a NAR.
// The hash will not be known until the [Derivation] is realized.
func FloatingOutputs(names sets.Set[string]) (Outputs, error) {
	outputs := Outputs{names: make([]string, 0, names.Len())}
	for name := range names {
		outputs.names = append(outputs.names, name)
	}
	if err := outputs.init(); err != nil {
		return Outputs{}, err
	}
	return outputs, nil
}

func (outputs Outputs) init() error {
	switch {
	case outputs.Len() == 0:
		return errZeroOutputs
	case outputs.fixed != nil && len(outputs.names) > 0:
		return fmt.Errorf("cannot mix fixed output with floating outputs")
	}
	for i, name := range outputs.names {
		if !IsValidOutputName(name) {
			return fmt.Errorf("invalid output name %+q", name)
		}
		if slices.Contains(outputs.names[:i], name) {
			return fmt.Errorf("duplicate output name %+q", name)
		}
	}
	slices.Sort(outputs.names)
	return nil
}

// All returns an iterator over the [*Output].
// If the outputs were created from [FixedOutput]
// and the directory and derivation name are valid,
// then the Path field will be set.
//
// The [*Output] values are ordered lexicographically by name.
func (outputs Outputs) All(dir Directory, drvName string) iter.Seq[*Output] {
	seq, _ := outputs.all(dir, drvName)
	return seq
}

func (outputs Outputs) all(dir Directory, drvName string) (iter.Seq[*Output], error) {
	if outputs.fixed != nil {
		output := &Output{
			Name:           DefaultOutputName,
			ContentAddress: *outputs.fixed,
		}
		var err error
		if dir != "" || drvName != "" {
			output.Path, err = FixedCAOutputPath(dir, drvName, *outputs.fixed, References{})
		}
		return func(yield func(*Output) bool) {
			yield(output)
		}, err
	}

	var slice []Output
	var err error
	if len(outputs.names) == 0 {
		err = errZeroOutputs
	} else {
		slice = make([]Output, len(outputs.names))
		for i, name := range outputs.names {
			slice[i] = Output{Name: name}
		}
	}
	return func(yield func(*Output) bool) {
		for i := range slice {
			if !yield(&slice[i]) {
				return
			}
		}
	}, err
}

// Names returns an iterator over the [*Output] names.
// The names are ordered lexicographically.
func (outputs Outputs) Names() iter.Seq[string] {
	return func(yield func(string) bool) {
		if outputs.fixed != nil {
			yield(DefaultOutputName)
			return
		}
		for _, name := range outputs.names {
			if !yield(name) {
				return
			}
		}
	}
}

// Has reports whether there exists an [*Output] with the given name.
func (outputs Outputs) Has(name string) bool {
	switch {
	case outputs.IsFixed():
		return name == DefaultOutputName
	default:
		return slices.Contains(outputs.names, name)
	}
}

// Len returns the number of [*Output] values contained in the set.
func (outputs Outputs) Len() int {
	if outputs.fixed != nil {
		return 1
	}
	return len(outputs.names)
}

// IsFixed reports whether the set was created with [FixedOutput].
func (outputs Outputs) IsFixed() bool {
	return outputs.fixed != nil
}

// FixedContentAddress reports whether the output was created with [FixedOutput]
// and returns the [ContentAddress] argument passed.
func (outputs Outputs) FixedContentAddress() (_ ContentAddress, isFixed bool) {
	if outputs.fixed == nil {
		return ContentAddress{}, false
	}
	return *outputs.fixed, true
}

// IsFloating reports whether the outputs' content hashes cannot be known
// until the derivation is realized.
// This is true for [Outputs] returned by
// [DefaultFloatingOutput] and [FloatingOutputs].
func (outputs Outputs) IsFloating() bool {
	return len(outputs.names) > 0
}

// String converts the [Outputs] to pseudo-ATerm format.
func (outputs Outputs) String() string {
	var dst []byte
	dst = append(dst, '[')
	i := 0
	for output := range outputs.All("", "") {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = output.appendText(dst)
		i++
	}
	dst = append(dst, ']')
	return string(dst)
}

const floatingCAHashType = nix.SHA256

// A Output is a single entry in an [Outputs] set.
type Output struct {
	Name           string
	ContentAddress nix.ContentAddress
	Path           Path
}

// IsFloating reports whether the output's content hash cannot be known
// until the derivation is realized.
func (output *Output) IsFloating() bool {
	return output != nil && output.ContentAddress.IsZero() && output.Path == ""
}

// IsFixed reports whether the output's content address is nonzero.
func (output *Output) IsFixed() bool {
	return output != nil && !output.ContentAddress.IsZero()
}

// HashAlgorithm returns the output's hashing algorithm string
// as it appears in [*Output.MarshalText].
func (output *Output) HashAlgorithm() (_ string, ok bool) {
	switch {
	case output.IsFixed():
		return methodOfContentAddress(output.ContentAddress).prefix() + output.ContentAddress.Hash().Type().String(), true
	case output.IsFloating():
		return recursiveFileIngestionMethod.prefix() + floatingCAHashType.String(), true
	default:
		return "", false
	}
}

// String converts the [*Output] to pseudo-ATerm format.
func (output *Output) String() string {
	return string(output.appendText(nil))
}

// MarshalText marshals the [*Output] to ATerm format.
func (output *Output) MarshalText() ([]byte, error) {
	return output.AppendText(nil)
}

// AppendText marshals the [*Output] to ATerm format
// and appends it to a byte slice, returning the resulting slice.
func (output *Output) AppendText(dst []byte) ([]byte, error) {
	if output.Name == "" {
		return dst, fmt.Errorf("marshal derivation output: missing name")
	}
	if !IsValidOutputName(output.Name) {
		return dst, fmt.Errorf("marshal derivation output %s: invalid name", output.Name)
	}
	if output.Path != "" {
		if !output.IsFixed() {
			return dst, fmt.Errorf("marshal derivation output %s: cannot use path %s for floating output", output.Name, output.Path)
		}
		if _, err := inferDerivationName(output.Path, output.Name); err != nil {
			return dst, fmt.Errorf("marshal derivation output %s: %v", output.Name, err)
		}
	}
	return output.appendText(dst), nil
}

func (output *Output) appendText(dst []byte) []byte {
	if output == nil {
		output = new(Output)
	}
	dst = append(dst, '(')
	dst = aterm.AppendString(dst, output.Name)
	dst = append(dst, ',')
	dst = aterm.AppendString(dst, string(output.Path))
	dst = append(dst, ',')
	algo, _ := output.HashAlgorithm()
	dst = aterm.AppendString(dst, algo)
	dst = append(dst, ',')
	dst = aterm.AppendString(dst, output.ContentAddress.Hash().RawBase16())
	dst = append(dst, ')')
	return dst
}

// UnmarshalText unmarshals an [*Output] from ATerm format.
func (output *Output) UnmarshalText(text []byte) error {
	r := bytes.NewReader(text)
	if err := output.parse(aterm.NewScanner(r)); err != nil {
		return fmt.Errorf("unmarshal derivation output: %v", err)
	}
	if r.Len() > 0 {
		return fmt.Errorf("unmarshal derivation output: %s: trailing data", output.Name)
	}
	return nil
}

func (output *Output) parse(s *aterm.Scanner) error {
	*output = Output{}

	tok, err := expectToken(s, aterm.LParen)
	if err != nil {
		return err
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("name: %v", err)
	}
	output.Name = tok.Value
	if !IsValidOutputName(output.Name) {
		return fmt.Errorf("name: invalid name %+q", output.Name)
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("%s: path: %v", output.Name, err)
	}
	rawOutputPath := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("%s: hash algorithm: %v", output.Name, err)
	}
	caInfo := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("%s: hash: %v", output.Name, err)
	}
	hashHex := tok.Value

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return fmt.Errorf("%s: %v", output.Name, err)
	}

	method, hashAlgo, err := parseHashAlgorithm(caInfo)
	if err != nil {
		return fmt.Errorf("%s: hash algorithm: %v", output.Name, err)
	}
	if rawOutputPath != "" {
		var err error
		output.Path, err = ParsePath(rawOutputPath)
		if err != nil {
			return fmt.Errorf("%s: %v", output.Name, err)
		}
		if _, err := inferDerivationName(output.Path, output.Name); err != nil {
			return fmt.Errorf("%s: path %s: %v", output.Name, output.Path, err)
		}
	}
	hashBits, err := hex.DecodeString(hashHex)
	if err != nil {
		return fmt.Errorf("%s: hash: %v", output.Name, err)
	}
	switch {
	case hashHex != "":
		if got, want := len(hashBits), hashAlgo.Size(); got != want {
			err = fmt.Errorf("%s: hash: incorrect size (got %d bytes but %v uses %d)",
				output.Name, got, hashAlgo, want)
			return err
		}
		switch h := nix.NewHash(hashAlgo, hashBits); method {
		case textIngestionMethod:
			output.ContentAddress = nix.TextContentAddress(h)
		case flatFileIngestionMethod:
			output.ContentAddress = nix.FlatFileContentAddress(h)
		case recursiveFileIngestionMethod:
			output.ContentAddress = nix.RecursiveFileContentAddress(h)
		default:
			return fmt.Errorf("%s: internal error: unknown content address method %d", output.Name, method)
		}
	case output.Path == "" && (method != recursiveFileIngestionMethod || hashAlgo != floatingCAHashType):
		return fmt.Errorf("%s: hash algorithm = %s (must be %s%v)", output.Name, caInfo, recursiveFileIngestionMethod.prefix(), floatingCAHashType)
	case output.Path != "":
		return fmt.Errorf("%s: unknown type", output.Name)
	}
	return nil
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

// OutputReference is a reference to an [*Output].
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
	if drvName == "" {
		return "", fmt.Errorf("output path for %v: empty derivation name", ref)
	}
	if !IsValidOutputName(ref.OutputName) {
		return "", fmt.Errorf("output path for %v: invalid output name %q", ref, ref.OutputName)
	}
	if ref.OutputName == DefaultOutputName {
		return drvName, nil
	}
	return drvName + "-" + ref.OutputName, nil
}

// Placeholder returns the string used in leiu of the final output path for the reference.
//
// During a [Derivation]'s realization, the backend replaces any occurrences of the placeholder
// in the derivation's environment variables
// with the temporary output path (used until the content address stabilizes).
func (ref OutputReference) Placeholder() string {
	// We accept non-".drv" paths here for simplicity,
	// so we don't use [Path.DerivationName].
	drvName := strings.TrimSuffix(ref.DrvPath.Name(), DerivationExt)

	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-upstream-output:")
	h.WriteString(ref.DrvPath.Digest())
	h.WriteString(":")
	h.WriteString(drvName)
	if ref.OutputName != DefaultOutputName {
		h.WriteString("-")
		h.WriteString(ref.OutputName)
	}
	return "/" + h.SumHash().RawBase32()
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

// IsValidOutputName reports whether the given string is valid as a derivation output name.
func IsValidOutputName(name string) bool {
	// TODO(someday): This should be an allow list of characters.
	return name != "" && !strings.ContainsAny(name, "^!")
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

// OutputPlaceholder returns the placeholder string used in leiu of the current [Derivation]'s output path.
//
// During a [Derivation]'s realization, the backend replaces any occurrences of the placeholder
// in the derivation's environment variables
// with the temporary output path (used until the content address stabilizes).
func OutputPlaceholder(outputName string) string {
	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-output:")
	h.WriteString(outputName)
	return "/" + h.SumHash().RawBase32()
}

// inferDerivationName infers the derivation name based on an output path and an output name.
func inferDerivationName(outputPath Path, outputName string) (string, error) {
	name := outputPath.Name()
	if outputName != DefaultOutputName {
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

var errZeroOutputs = errors.New("derivation must have at least one output")
