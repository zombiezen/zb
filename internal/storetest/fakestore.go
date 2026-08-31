// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package storetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
)

var _ interface {
	zbstore.BatchStore
	zbstore.RealizationFetcher
	zbstore.Importer
} = (*Store)(nil)

// Store is an in-memory implementation of [zbstore.BatchStore] and [zbstore.RealizationFetcher].
// A Store is safe to use from multiple goroutines simultaneously.
// The zero Store value is an empty store: one without objects or realizations.
// Objects can be added with [*Store.StoreImport].
// Realizations can be added with [*Store.AddRealization].
type Store struct {
	mu           sync.RWMutex
	objects      map[zbstore.Path]*zbstore.Blob
	realizations map[string]map[string][]*zbstore.Realization
}

// Object implements [zbstore.Store].
func (store *Store) Object(ctx context.Context, path zbstore.Path) (zbstore.Object, error) {
	var obj *zbstore.Blob
	if store != nil {
		store.mu.RLock()
		obj = store.objects[path]
		store.mu.RUnlock()
	}
	if obj == nil {
		return nil, fmt.Errorf("open %s: %w", path, zbstore.ErrNotFound)
	}
	return obj, nil
}

// ObjectBatch implements [zbstore.BatchStore].
func (store *Store) ObjectBatch(ctx context.Context, storePaths sets.Set[zbstore.Path]) ([]zbstore.Object, error) {
	if storePaths.Len() == 0 || store == nil {
		return nil, nil
	}

	objects := make([]zbstore.Object, 0, storePaths.Len())
	store.mu.RLock()
	defer store.mu.RUnlock()
	for path := range storePaths.All() {
		if obj := store.objects[path]; obj != nil {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

// WriteObject implements [zbstore.ObjectWriter] by adding the object to the store.
func (store *Store) WriteObject(ctx context.Context, obj zbstore.Object) error {
	info := obj.Info()
	content := new(bytes.Buffer)
	var w io.Writer = content
	hash := info.NARHash
	var hasher *nix.Hasher
	if hash.IsZero() {
		hasher = nix.NewHasher(nix.SHA256)
		w = io.MultiWriter(hasher, content)
	}
	if _, err := zbstore.VerifyObject(ctx, w, obj, nil); err != nil {
		return err
	}
	if hasher != nil {
		hash = hasher.SumHash()
	}
	store.addBlobNoVerify(&zbstore.Blob{
		NAR:           content.Bytes(),
		NARHash:       hash,
		ExportTrailer: *info.ExportTrailer().Clone(),
	})
	return nil
}

func (store *Store) addBlob(blob *zbstore.Blob) {
	if _, err := zbstore.VerifyObject(context.Background(), io.Discard, blob, nil); err == nil {
		store.addBlobNoVerify(blob)
	}
}

func (store *Store) addBlobNoVerify(blob *zbstore.Blob) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.objects[blob.StorePath] != nil {
		return
	}
	if store.objects == nil {
		store.objects = make(map[zbstore.Path]*zbstore.Blob)
	}
	store.objects[blob.StorePath] = blob
}

// StoreImport implements [zbstore.Importer] by adding the objects to the store.
func (store *Store) StoreImport(ctx context.Context, r io.Reader) error {
	return addBlobs(ctx, store, r)
}

// FetchRealizations implements [zbstore.RealizationFetcher].
func (store *Store) FetchRealizations(ctx context.Context, derivationHash nix.Hash) (zbstore.RealizationMap, error) {
	result := zbstore.RealizationMap{
		DerivationHash: derivationHash,
	}
	if store != nil {
		store.mu.RLock()
		defer store.mu.RUnlock()
		if m := store.realizations[derivationHash.SRI()]; len(m) > 0 {
			for outputName, realizations := range m {
				if len(realizations) == 0 {
					continue
				}
				if result.Realizations == nil {
					result.Realizations = make(map[string][]*zbstore.Realization, len(m))
				}
				realizationsCopy := make([]*zbstore.Realization, 0, len(realizations))
				for _, r := range realizations {
					realizationsCopy = append(realizationsCopy, r.Clone())
				}
				result.Realizations[outputName] = realizationsCopy
			}
		}
	}
	return result, nil
}

// WriteRealizations adds the given realizations to the store.
func (store *Store) WriteRealizations(ctx context.Context, realizations zbstore.RealizationMap) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.realizations == nil {
		store.realizations = make(map[string]map[string][]*zbstore.Realization)
	}
	key := realizations.DerivationHash.SRI()
	m := store.realizations[key]
	if m == nil {
		m = make(map[string][]*zbstore.Realization)
		store.realizations[key] = m
	}
	return (&zbstore.RealizationMap{
		DerivationHash: realizations.DerivationHash,
		Realizations:   m,
	}).Merge(realizations)
}
