// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"fmt"
	"slices"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	jsonv1 "github.com/go-json-experiment/json/v1"
	"zombiezen.com/go/nix"
)

// A ReferenceClass is a mapping of referenced path to optional realization.
type ReferenceClass struct {
	Path        Path                       `json:"path"`
	Realization RealizationOutputReference `json:"realization"`
}

// compareReferenceClasses returns
//
//   - -1 if rc1 is less than rc2
//   - 0 if rc1 equals rc2
//   - 1 if rc1 is greater than rc2
//
// according to the rules in https://zb.256lights.llc/binary-cache/realizations#signatures.
func compareReferenceClasses(rc1, rc2 *ReferenceClass) int {
	if result := cmp.Compare(rc1.Path, rc2.Path); result != 0 {
		return result
	}
	return compareRealizationOutputReferences(rc1.Realization, rc2.Realization)
}

// RealizationOutputReference is a reference to an output of an equivalence class of derivations.
// It is similar to an [OutputReference], but can refers to many derivations.
type RealizationOutputReference struct {
	DerivationHash nix.Hash
	OutputName     string
}

// IsZero reports whether ref is the zero value.
func (ref RealizationOutputReference) IsZero() bool {
	return ref.DerivationHash.IsZero() && ref.OutputName == ""
}

// String returns the hash and the output name separated by "!".
// If ref is the zero value, then String returns "ε".
func (ref RealizationOutputReference) String() string {
	if ref.IsZero() {
		return "ε"
	}
	return ref.DerivationHash.Base32() + "!" + ref.OutputName
}

// MarshalJSON encodes the output reference to JSON.
// If ref.IsZero() reports true, then MarshalJSON returns "null".
func (ref RealizationOutputReference) MarshalJSON() ([]byte, error) {
	return jsonv2.Marshal(ref, jsonv1.DefaultOptionsV1())
}

// MarshalJSONTo encodes the output reference to a [*jsontext.Encoder].
// If ref.IsZero() reports true, then MarshalJSONTo writes a null token.
func (ref RealizationOutputReference) MarshalJSONTo(enc *jsontext.Encoder) error {
	if ref.IsZero() {
		return enc.WriteToken(jsontext.Null)
	}
	if ref.DerivationHash.IsZero() {
		return fmt.Errorf("marshal realization output reference: hash not set")
	}
	if !IsValidOutputName(ref.OutputName) {
		return fmt.Errorf("marshal realization output reference: invalid output name %+q", ref.OutputName)
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	if err := enc.WriteToken(jsontext.String("derivationHash")); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	if err := MarshalHashJSONTo(enc, ref.DerivationHash); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	if err := enc.WriteToken(jsontext.String("outputName")); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	if err := enc.WriteToken(jsontext.String(ref.OutputName)); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return fmt.Errorf("marshal realization output reference: %w", err)
	}
	return nil
}

// UnmarshalJSON decodes an output reference from a JSON byte slice.
// If the byte slice holds "null", then ref is set to the zero [RealizationOutputReference].
func (ref *RealizationOutputReference) UnmarshalJSON(data []byte) error {
	return jsonv2.Unmarshal(data, ref, jsonv1.DefaultOptionsV1())
}

// UnmarshalJSONFrom decodes an output reference from a [*jsontext.Decoder].
// If the next token is null, then ref is set to the zero [RealizationOutputReference].
func (ref *RealizationOutputReference) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch kind := dec.PeekKind(); kind {
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return fmt.Errorf("unmarshal realization output reference: %w", err)
		}
		*ref = RealizationOutputReference{}
		return nil
	case '{':
		var parsed struct {
			DerivationHash nix.Hash `json:"derivationHash"`
			OutputName     string   `json:"outputName"`
		}
		unmarshalersOption := jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(UnmarshalHashJSONFrom))
		if err := jsonv2.UnmarshalDecode(dec, &parsed, unmarshalersOption); err != nil {
			return fmt.Errorf("unmarshal realization output reference: %w", err)
		}
		if parsed.DerivationHash.IsZero() {
			return fmt.Errorf("unmarshal realization output reference: hash not set")
		}
		if !IsValidOutputName(parsed.OutputName) {
			return fmt.Errorf("unmarshal realization output reference: invalid output name %+q", parsed.OutputName)
		}
		*ref = RealizationOutputReference(parsed)
		return nil
	default:
		return fmt.Errorf("unmarshal realization output reference: must be object or null (got %v)", kind)
	}
}

// compareRealizationOutputReferences returns
//
//   - -1 if ref1 is less than ref2
//   - 0 if ref1 equals ref2
//   - 1 if ref1 is greater than ref2
//
// according to the rules in https://zb.256lights.llc/binary-cache/realizations#signatures.
func compareRealizationOutputReferences(ref1, ref2 RealizationOutputReference) int {
	switch {
	case ref1.IsZero() && ref2.IsZero():
		return 0
	case ref1.IsZero() && !ref2.IsZero():
		return -1
	case !ref1.IsZero() && ref2.IsZero():
		return 1
	}

	htype1 := ref1.DerivationHash.Type()
	htype2 := ref2.DerivationHash.Type()
	if result := cmp.Compare(htype1.String(), htype2.String()); result != 0 {
		return result
	}

	decodedLen := htype1.Size()
	encodedLen := base64.StdEncoding.EncodedLen(decodedLen)
	buf := make([]byte, decodedLen+encodedLen*2)
	hashBytes := slices.Clip(buf[:decodedLen])
	digest1 := slices.Clip(buf[decodedLen : decodedLen+encodedLen])
	digest2 := slices.Clip(buf[decodedLen+encodedLen:])
	hashBytes = ref1.DerivationHash.Bytes(hashBytes[:0])
	base64.StdEncoding.Encode(digest1, hashBytes)
	hashBytes = ref2.DerivationHash.Bytes(hashBytes[:0])
	base64.StdEncoding.Encode(digest2, hashBytes)
	if result := bytes.Compare(digest1, digest2); result != 0 {
		return result
	}

	return cmp.Compare(ref1.OutputName, ref2.OutputName)
}
