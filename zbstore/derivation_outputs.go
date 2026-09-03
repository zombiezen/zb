// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"iter"
	"slices"
	"strings"

	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
)

// DefaultDerivationOutputName is the name of the primary output of a derivation.
// It is omitted in a number of contexts.
const DefaultDerivationOutputName = "out"

// DerivationOutputs describes the outputs of a [Derivation].
// The zero value is an empty set of outputs.
type DerivationOutputs struct {
	names []string
	fixed *nix.ContentAddress
}

// FixedOutput returns a new [*DerivationOutputs] value
// for a single output that must match the given content address assertion.
// FixedOutput panics if the [ContentAddress] is the zero value.
func FixedOutput(ca nix.ContentAddress) DerivationOutputs {
	if ca.IsZero() {
		panic("zero content address")
	}
	return DerivationOutputs{fixed: new(ca)}
}

// DefaultFloatingOutput returns a [*DerivationOutputs] value
// with a single output named [DefaultDerivationOutputName].
func DefaultFloatingOutput() DerivationOutputs {
	return defaultFloatingOutput
}

var defaultFloatingOutput = DerivationOutputs{
	names: []string{DefaultDerivationOutputName},
}

// FloatingOutputs returns a new [*DerivationOutputs] value with a set of output names.
// Each output will be SHA-256-hashed as a NAR.
// The hash will not be known until the derivation is realized.
func FloatingOutputs(names sets.Set[string]) (DerivationOutputs, error) {
	outputs := DerivationOutputs{names: make([]string, 0, names.Len())}
	for name := range names {
		outputs.names = append(outputs.names, name)
	}
	if err := outputs.init(); err != nil {
		return DerivationOutputs{}, err
	}
	return outputs, nil
}

func (outputs DerivationOutputs) init() error {
	switch {
	case outputs.fixed == nil && len(outputs.names) == 0:
		return fmt.Errorf("empty outputs")
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

// Names returns an iterator over the output names.
func (outputs DerivationOutputs) Names() iter.Seq[string] {
	return func(yield func(string) bool) {
		if outputs.fixed != nil {
			yield(DefaultDerivationOutputName)
			return
		}
		for _, name := range outputs.names {
			if !yield(name) {
				return
			}
		}
	}
}

// Has reports whether there exists an output with the given name.
func (outputs DerivationOutputs) Has(name string) bool {
	switch {
	case outputs.IsFixed():
		return name == DefaultDerivationOutputName
	default:
		return slices.Contains(outputs.names, name)
	}
}

// IsZero reports whether outputs is the zero value.
func (outputs DerivationOutputs) IsZero() bool {
	return len(outputs.names) == 0 && outputs.fixed == nil
}

// IsFixed reports whether the output was returned from [FixedOutput]
func (outputs DerivationOutputs) IsFixed() bool {
	return outputs.fixed != nil
}

// FixedContentAddress reports whether the output was returned from [FixedOutput]
// and returns the [ContentAddress] argument passed.
func (outputs DerivationOutputs) FixedContentAddress() (_ ContentAddress, isFixed bool) {
	if outputs.fixed == nil {
		return ContentAddress{}, false
	}
	return *outputs.fixed, true
}

// IsFloating reports whether the output's content hash cannot be known
// until the derivation is realized.
// This is true for outputs returned by
// [FlatFileFloatingCAOutput] and [FloatingOutputs].
func (outputs DerivationOutputs) IsFloating() bool {
	return len(outputs.names) > 0
}

const floatingCAHashType = nix.SHA256

// HashType returns the hash type of the outputs.
func (outputs DerivationOutputs) HashType() (_ nix.HashType, ok bool) {
	if ca, ok := outputs.FixedContentAddress(); ok {
		return ca.Hash().Type(), true
	} else if outputs.IsFloating() {
		return floatingCAHashType, true
	} else {
		return 0, false
	}
}

// IsRecursiveFile reports whether the outputs use recursive (NAR) hashing.
func (outputs DerivationOutputs) IsRecursiveFile() bool {
	if ca, ok := outputs.FixedContentAddress(); ok {
		return ca.IsRecursiveFile()
	} else {
		return outputs.IsFloating()
	}
}

type DerivationOutput struct {
	Name           string
	ContentAddress nix.ContentAddress
	Path           Path
}

func (output *DerivationOutput) IsFloating() bool {
	return output.ContentAddress.IsZero() && output.Path == ""
}

func (output *DerivationOutput) IsFixed() bool {
	return !output.ContentAddress.IsZero()
}

func (output *DerivationOutput) AppendText(dst []byte) ([]byte, error) {
	if output.Name == "" {
		return dst, fmt.Errorf("marshal derivation output: missing name")
	}
	if !IsValidOutputName(output.Name) {
		// TODO(now): Check for fixed and non-default name.
		return dst, fmt.Errorf("marshal derivation output %s: invalid name")
	}
	if output.IsFixed() {
		// TODO(now): Is path valid?
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
}

func (output *DerivationOutput) UnmarshalText(text []byte) error {
	r := bytes.NewReader(text)
	if err := output.parse(aterm.NewScanner(r)); err != nil {
		return fmt.Errorf("unmarshal derivation output: %v", err)
	}
	if r.Len() > 0 {
		return fmt.Errorf("unmarshal derivation output: %s: trailing data", output.Name)
	}
	return nil
}

func (output *DerivationOutput) parse(s *aterm.Scanner) error {
	*output = DerivationOutput{}

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
		output.Hash = nix.NewHash(hashAlgo, hashBits)
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
