// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"fmt"
	"maps"

	"zombiezen.com/go/nix"
)

// SHA256RealizationHash computes the hash for the given derivation
// based on the realizations of its input derivations.
// This hash is intended for use in [RealizationOutputReference].
// If realization is nil, then SHA256RealizationHash will return an error
// if the derivation requires other realizations to compute its hash.
func (drv *Derivation) SHA256RealizationHash(realization func(ref OutputReference) (Path, error)) (nix.Hash, error) {
	if drv.Outputs.IsFixed() {
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

// derivationInputRewrites returns a substitution map
// of output placeholders to realization paths.
func derivationInputRewrites(drv *Derivation, realization func(ref OutputReference) (Path, error)) (map[string]Path, error) {
	// TODO(maybe): Also rewrite transitive derivation hashes?
	result := make(map[string]Path)
	for ref := range drv.InputDerivationOutputs() {
		placeholder := UnknownCAOutputPlaceholder(ref)
		rpath, err := realization(ref)
		if err != nil {
			return nil, fmt.Errorf("compute input rewrites: %v", err)
		}
		result[placeholder] = rpath
	}
	return result, nil
}

// hashDrvFixed computes the equivalence class for a fixed-output derivation.
func hashDrvFixed(drv *Derivation) (nix.Hash, error) {
	ca, isFixed := drv.Outputs.FixedContentAddress()
	if !isFixed {
		return nix.Hash{}, fmt.Errorf("hash derivation: not fixed")
	}
	outputPath, err := drv.FixedOutputPath()
	if err != nil {
		return nix.Hash{}, fmt.Errorf("hash derivation: %v", err)
	}
	h2 := nix.NewHasher(nix.SHA256)
	h2.WriteString("fixed:out:")
	switch {
	case ca.IsText():
		h2.WriteString("text:")
	case ca.IsRecursiveFile():
		h2.WriteString("r:")
	}
	h2.WriteString(ca.Hash().Base16())
	h2.WriteString(":")
	h2.WriteString(string(outputPath))
	return h2.SumHash(), nil
}

func hashDrvFloating(expandedDrv *Derivation) (nix.Hash, error) {
	atermData, err := expandedDrv.MarshalText()
	if err != nil {
		return nix.Hash{}, fmt.Errorf("hash derivation: %v", err)
	}
	h := nix.NewHasher(nix.SHA256)
	h.WriteString("floating:")
	h.WriteString(expandedDrv.Name)
	h.WriteString(":") // ':' guaranteed not to appear in a store object name.
	h.Write(atermData)
	return h.SumHash(), nil
}
