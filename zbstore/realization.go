// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"iter"
	"slices"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/internal/xiter"
	"zb.256lights.llc/pkg/internal/xslices"
	"zombiezen.com/go/nix"
)

// A RealizationFetcher lists known [Realization] values for a derivation.
// The argument to FetchRealizations is a [derivation hash].
// FetchRealizations may return a non-empty [RealizationMap] in addition to an error.
//
// FetchRealizations must be safe to call concurrently from multiple goroutines simultaneously.
//
// [derivation hash]: https://zb.256lights.llc/binary-cache/realizations#derivation-hashes
type RealizationFetcher interface {
	FetchRealizations(ctx context.Context, derivationHash nix.Hash) (RealizationMap, error)
}

// A RealizationMap is a multi-map of [RealizationOutputReference] to [*Realization].
// The zero value is an empty map.
// RealizationMap is equivalent to a [realization document].
//
// [realization document]: https://zb.256lights.llc/binary-cache/realizations
type RealizationMap struct {
	DerivationHash nix.Hash                  `json:"derivationHash"`
	Realizations   map[string][]*Realization `json:"realizations"`
}

// IsEmpty reports whether m is an empty map.
func (m RealizationMap) IsEmpty() bool {
	for _, slice := range m.Realizations {
		for _, r := range slice {
			if r != nil {
				return false
			}
		}
	}
	return true
}

// Clone returns a copy of m.
// Nil [*Realization] pointers are removed,
// and empty slices are not added to the resulting Realizations map.
func (m RealizationMap) Clone() RealizationMap {
	keyCount := 0
	realizationCount := 0
	for _, slice := range m.Realizations {
		n := realizationCount
		for _, r := range slice {
			if r != nil {
				realizationCount++
			}
		}
		if realizationCount > n {
			keyCount++
		}
	}

	m2 := RealizationMap{
		DerivationHash: m.DerivationHash,
	}
	if keyCount > 0 {
		m2.Realizations = make(map[string][]*Realization, keyCount)
		pool := make([]Realization, realizationCount)
		slicePool := make([]*Realization, realizationCount)
		for outputName, slice := range m.Realizations {
			newSlice := slicePool[:0]
			for _, original := range slice {
				if original == nil {
					continue
				}
				r := &pool[0]
				pool = pool[1:]
				*r = *original.Clone()
				newSlice = append(newSlice, r)
			}
			if len(newSlice) > 0 {
				slicePool = slicePool[len(newSlice):]
				m2.Realizations[outputName] = slices.Clip(newSlice)
			}
		}
	}
	return m2
}

// All returns an iterator over all the realizations in the map.
func (m RealizationMap) All() iter.Seq2[RealizationOutputReference, *Realization] {
	return func(yield func(RealizationOutputReference, *Realization) bool) {
		for outputName, slice := range m.Realizations {
			ref := RealizationOutputReference{
				DerivationHash: m.DerivationHash,
				OutputName:     outputName,
			}
			for _, v := range slice {
				if v != nil {
					if !yield(ref, v) {
						return
					}
				}
			}
		}
	}
}

// Compact deduplicates [*Realization] entries in m.
func (m RealizationMap) Compact() {
	if m.Realizations == nil {
		return
	}
	for outputName, slice := range m.Realizations {
		newRealizations := slice[:0]
		for _, r := range slice {
			newRealizations = appendRealization(newRealizations, r, false)
		}
		clear(slice[len(newRealizations):])
		m.Realizations[outputName] = newRealizations
	}
}

// Merge updates m with realizations from src.
func (m *RealizationMap) Merge(src RealizationMap) error {
	if src.IsEmpty() {
		return nil
	}
	if !src.DerivationHash.Equal(m.DerivationHash) {
		return fmt.Errorf("mismatched hash %v", src.DerivationHash)
	}
	if m.Realizations == nil {
		m.Realizations = src.Realizations
		return nil
	}
	for outputName, slice := range src.Realizations {
		if xiter.All(slices.Values(slice), func(r *Realization) bool { return r == nil }) {
			continue
		}
		if m.Realizations == nil {
			m.Realizations = make(map[string][]*Realization)
		}
		newRealizations := m.Realizations[outputName]
		for _, r := range slice {
			newRealizations = appendRealization(newRealizations, r, true)
		}
		m.Realizations[outputName] = newRealizations
	}
	return nil
}

// appendRealization joins r with realizations and returns the resulting slice.
// If another realization in the slice has the same output path and reference classes as r,
// unique r.Signatures will be appended to the first such realization in the slice.
// If clone is true, the parts of r that are added to dst are cloned.
func appendRealization(dst []*Realization, r *Realization, clone bool) []*Realization {
	if r == nil {
		return dst
	}
	i := slices.IndexFunc(dst, func(r2 *Realization) bool {
		return realizationKeysEqual(r, r2)
	})
	if i == -1 {
		if clone {
			r = r.Clone()
		}
		return append(dst, r)
	}
	for _, sig := range r.Signatures {
		found := slices.ContainsFunc(dst[i].Signatures, func(other *RealizationSignature) bool {
			return realizationSignaturesEqual(sig, other)
		})
		if !found {
			if clone {
				sig = sig.Clone()
			}
			dst[i].Signatures = append(dst[i].Signatures, sig)
		}
	}
	return dst
}

// A Realization is a known output path for a particular [RealizationOutputReference].
type Realization struct {
	OutputPath       Path                    `json:"outputPath"`
	ReferenceClasses []*ReferenceClass       `json:"referenceClasses"`
	Signatures       []*RealizationSignature `json:"signatures,omitempty"`
}

// Clone returns a copy of r or nil if r is nil.
func (r *Realization) Clone() *Realization {
	r2 := &Realization{
		OutputPath: r.OutputPath,
		Signatures: cloneSignatures(nil, r.Signatures...),
	}
	if len(r.ReferenceClasses) > 0 {
		r2.ReferenceClasses = xslices.ClonePointers(r.ReferenceClasses)
	}
	return r2
}

func realizationKeysEqual(r1, r2 *Realization) bool {
	if r1.OutputPath != r2.OutputPath || len(r1.ReferenceClasses) != len(r2.ReferenceClasses) {
		return false
	}
	rc1 := slices.Clone(r1.ReferenceClasses)
	slices.SortFunc(rc1, compareReferenceClasses)
	rc2 := slices.Clone(r2.ReferenceClasses)
	slices.SortFunc(rc2, compareReferenceClasses)
	for i := range rc1 {
		if compareReferenceClasses(rc1[i], rc2[i]) != 0 {
			return false
		}
	}
	return true
}

// RealizationSignatureFormat is an enumeration of formats for [RealizationSignature].
type RealizationSignatureFormat string

// Known signature formats.
const (
	Ed25519SignatureFormat RealizationSignatureFormat = "ed25519"
)

// RealizationPublicKey stores a public key used for a [RealizationSignature].
type RealizationPublicKey struct {
	Format RealizationSignatureFormat `json:"format"`
	Data   []byte                     `json:"publicKey,format:base64"`
}

// Equal reports whether pub and other are equal.
func (pub *RealizationPublicKey) Equal(other *RealizationPublicKey) bool {
	switch {
	case (pub != nil) != (other != nil):
		return false
	case pub == nil && other == nil:
		return true
	default:
		return pub.Format == other.Format && bytes.Equal(pub.Data, other.Data)
	}
}

// Clone returns a copy of pub or nil if pub is nil.
func (pub *RealizationPublicKey) Clone() *RealizationPublicKey {
	if pub == nil {
		return nil
	}
	return &RealizationPublicKey{
		Format: pub.Format,
		Data:   bytes.Clone(pub.Data),
	}
}

// String returns the public key in JSON format.
func (pub *RealizationPublicKey) String() string {
	data, _ := jsonv2.Marshal(pub, jsontext.SpaceAfterColon(false), jsontext.SpaceAfterComma(false))
	return string(data)
}

// A RealizationSignature is a cryptographic signature of a [RealizationOutputReference], [Realization] tuple.
type RealizationSignature struct {
	PublicKey RealizationPublicKey `json:",inline"`
	Signature []byte               `json:"signature,format:base64"`
}

// Clone returns a copy of sig or nil if sig is nil.
func (sig *RealizationSignature) Clone() *RealizationSignature {
	return cloneSignatures(nil, sig)[0]
}

func cloneSignatures(dst []*RealizationSignature, sigs ...*RealizationSignature) []*RealizationSignature {
	if len(sigs) == 0 {
		return dst
	}
	n := 0
	byteBufSize := 0
	for _, sig := range sigs {
		if sig != nil {
			n++
			byteBufSize += len(sig.PublicKey.Data) + len(sig.Signature)
		}
	}
	byteBuf := make([]byte, 0, byteBufSize)
	sigBuf := make([]RealizationSignature, n)
	dst = slices.Grow(dst, len(sigs))
	newLength := len(dst) + len(sigs)
	result := dst[len(dst):newLength]
	for i, sig := range sigs {
		if sig == nil {
			continue
		}

		sig2 := &sigBuf[0]
		sigBuf = sigBuf[1:]
		*sig2 = RealizationSignature{
			PublicKey: RealizationPublicKey{Format: sig.PublicKey.Format},
		}
		byteBuf = append(byteBuf, sig.PublicKey.Data...)
		sig2.PublicKey.Data = slices.Clip(byteBuf)
		byteBuf = append(byteBuf[len(byteBuf):], sig.Signature...)
		sig2.Signature = slices.Clip(byteBuf)
		byteBuf = byteBuf[len(byteBuf):]

		result[i] = sig2
	}
	return dst[:newLength]
}

func realizationSignaturesEqual(sig1, sig2 *RealizationSignature) bool {
	return sig1.PublicKey.Equal(&sig2.PublicKey) && bytes.Equal(sig1.Signature, sig2.Signature)
}

// SignRealizationWithEd25519 creates a signature for the realization
// using the Ed25519 signature algorithm.
func SignRealizationWithEd25519(ref RealizationOutputReference, r *Realization, key ed25519.PrivateKey) (*RealizationSignature, error) {
	v, err := marshalRealizationForSignature(ref, r)
	if err != nil {
		return nil, fmt.Errorf("sign realization %v: %v", ref, err)
	}
	sig := ed25519.Sign(key, v)
	return &RealizationSignature{
		PublicKey: RealizationPublicKey{
			Format: Ed25519SignatureFormat,
			Data:   key.Public().(ed25519.PublicKey),
		},
		Signature: sig,
	}, nil
}

// VerifyRealizationSignature verifies that the signature for the realization is valid.
func VerifyRealizationSignature(ref RealizationOutputReference, r *Realization, sig *RealizationSignature) error {
	switch sig.PublicKey.Format {
	case Ed25519SignatureFormat:
		if got, want := len(sig.PublicKey.Data), ed25519.PublicKeySize; got != want {
			return fmt.Errorf("verify realization signature: ed25519 public key is the wrong size (%d instead of %d bytes)", got, want)
		}
		v, err := marshalRealizationForSignature(ref, r)
		if err != nil {
			return fmt.Errorf("verify realization signature: %v", err)
		}
		if !ed25519.Verify(ed25519.PublicKey(sig.PublicKey.Data), v, sig.Signature) {
			return fmt.Errorf("verify realization signature: ed25519 signature does not match")
		}
		return nil
	default:
		return fmt.Errorf("verify realization signature: unsupported format %q", sig.PublicKey.Format)
	}
}

type realizationForSignature struct {
	DerivationHash   nix.Hash          `json:"derivationHash"`
	OutputName       string            `json:"outputName"`
	OutputPath       Path              `json:"outputPath"`
	ReferenceClasses []*ReferenceClass `json:"referenceClasses"`
}

// marshalRealizationForSignature marshals a realization using the [JSON Canonicalization Scheme].
//
// [JSON Canonicalization Scheme]: https://datatracker.ietf.org/doc/html/rfc8785
func marshalRealizationForSignature(ref RealizationOutputReference, r *Realization) (jsontext.Value, error) {
	rsig := &realizationForSignature{
		DerivationHash:   ref.DerivationHash,
		OutputName:       ref.OutputName,
		OutputPath:       r.OutputPath,
		ReferenceClasses: r.ReferenceClasses,
	}
	if !slices.IsSortedFunc(rsig.ReferenceClasses, compareReferenceClasses) {
		rsig.ReferenceClasses = slices.Clone(rsig.ReferenceClasses)
		slices.SortFunc(rsig.ReferenceClasses, compareReferenceClasses)
	}
	firstPass, err := jsonv2.Marshal(
		rsig,
		jsonv2.WithMarshalers(jsonv2.MarshalToFunc(MarshalHashJSONTo)),
	)
	if err != nil {
		return nil, fmt.Errorf("marshal realization: %v", err)
	}
	canonicalOutput := jsontext.Value(firstPass)
	if err := canonicalOutput.Canonicalize(); err != nil {
		return nil, fmt.Errorf("marshal realization: %v", err)
	}
	return canonicalOutput, nil
}
