// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package storetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"zb.256lights.llc/pkg/internal/multierror"
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

// StoreImport implements [zbstore.Importer] by adding the objects to the store.
func (store *Store) StoreImport(ctx context.Context, r io.Reader) error {
	recv := &storeReceiver{store: store}
	err := zbstore.ReceiveExport(recv, r)
	recv.errors.Add(err)
	return recv.errors.Error()
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
					realizationsCopy = append(realizationsCopy, cloneRealization(r))
				}
				result.Realizations[outputName] = realizationsCopy
			}
		}
	}
	return result, nil
}

// AddRealization adds the given realization to the store.
func (store *Store) AddRealization(ref zbstore.RealizationOutputReference, r *zbstore.Realization) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.realizations == nil {
		store.realizations = make(map[string]map[string][]*zbstore.Realization)
	}
	key := ref.DerivationHash.SRI()
	m := store.realizations[key]
	if m == nil {
		m = make(map[string][]*zbstore.Realization)
		store.realizations[key] = m
	}
	m[ref.OutputName] = append(m[ref.OutputName], cloneRealization(r))
}

type storeReceiver struct {
	store  *Store
	buf    bytes.Buffer
	errors multierror.Collector
}

func (s *storeReceiver) Write(p []byte) (n int, err error) {
	return s.buf.Write(p)
}

func (s *storeReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	obj := &zbstore.Blob{
		NAR:           s.buf.Bytes(),
		ExportTrailer: *trailer,
	}
	if _, err := zbstore.VerifyObject(context.Background(), io.Discard, obj, nil); err != nil {
		s.errors.Add(err)
		return
	}

	s.store.mu.Lock()
	if s.store.objects[obj.StorePath] == nil {
		if s.store.objects == nil {
			s.store.objects = make(map[zbstore.Path]*zbstore.Blob)
		}
		obj.NAR = bytes.Clone(obj.NAR)
		obj.ExportTrailer = *cloneExportTrailer(&obj.ExportTrailer)
		s.store.objects[obj.StorePath] = obj
	}
	s.store.mu.Unlock()

	s.buf.Reset()
}

func cloneExportTrailer(trailer *zbstore.ExportTrailer) *zbstore.ExportTrailer {
	trailer = new(*trailer)
	trailer.References = *trailer.References.Clone()
	return trailer
}

func cloneRealization(r *zbstore.Realization) *zbstore.Realization {
	rcopy := &zbstore.Realization{
		OutputPath: r.OutputPath,
		Signatures: make([]*zbstore.RealizationSignature, 0, len(r.Signatures)),
	}
	if len(r.ReferenceClasses) > 0 {
		rcopy.ReferenceClasses = make([]*zbstore.ReferenceClass, 0, len(r.ReferenceClasses))
		for _, rc := range r.ReferenceClasses {
			rcopy.ReferenceClasses = append(rcopy.ReferenceClasses, new(*rc))
		}
	}
	if len(r.Signatures) > 0 {
		rcopy.Signatures = make([]*zbstore.RealizationSignature, 0, len(r.Signatures))
		for _, sig := range r.Signatures {
			rcopy.Signatures = append(rcopy.Signatures, &zbstore.RealizationSignature{
				PublicKey: zbstore.RealizationPublicKey{
					Format: sig.PublicKey.Format,
					Data:   bytes.Clone(sig.PublicKey.Data),
				},
				Signature: bytes.Clone(sig.Signature),
			})
		}
	}
	return rcopy
}
