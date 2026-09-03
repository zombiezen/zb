// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package storetest

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"strings"

	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// TxtarStore is a collection of objects parsed by [TxtarObjects].
type TxtarStore struct {
	BlobSlice
	// Rewrites is a map of store object names as they appear in the txtar file
	// to their resulting store path.
	Rewrites map[string]zbstore.Path
	// Labels is a set of optional strings attached to an object.
	// They must appear before the first file in an object enclosed in brackets
	// (e.g. "[foo]").
	Labels map[zbstore.Path][]string
}

// OriginalObjectName returns the placeholder store object name in the txtar file
// for a [zbstore.Path].
func (store *TxtarStore) OriginalObjectName(path zbstore.Path) (filename string, ok bool) {
	if store == nil {
		return "", false
	}
	for k, v := range store.Rewrites {
		if v == path {
			return k, true
		}
	}
	return "", false
}

// TxtarObjects converts the source files and .drv files in a txtar archive to a [BlobSlice],
// rewriting their paths to the named directory.
func TxtarObjects(dir zbstore.Directory, files []txtar.File) (*TxtarStore, error) {
	result := &TxtarStore{
		BlobSlice: make(BlobSlice, 0, len(files)),
		Rewrites:  make(map[string]zbstore.Path),
		Labels:    make(map[zbstore.Path][]string),
	}

	for objectFiles := range groupFilesByObject(files) {
		labels, firstPath, fixedHash, err := parseFilename(objectFiles[0].Name)
		if err != nil {
			return result, err
		}
		if digest, ok := firstPath.digest(); ok {
			for name := range result.Rewrites {
				if other, _ := txtarPath(name).digest(); other == digest {
					currObjectName := firstPath.objectName()
					if name == currObjectName {
						return result, fmt.Errorf("duplicate object %s", name)
					}
					return result, fmt.Errorf("%s and %s have the same digest", name, currObjectName)
				}
			}
		}
		firstFileData := cleanFileData(objectFiles[0].Data)

		switch {
		case !fixedHash.IsZero():
			// Special case: fixed-output file.
			if !firstPath.isSingleFile() || len(objectFiles) != 1 {
				return result, fmt.Errorf("%s: fixed output objects must be a single file", firstPath)
			}
			h := nix.NewHasher(fixedHash.Type())
			h.Write(firstFileData)
			if got := h.SumHash(); !got.Equal(fixedHash) {
				return result, fmt.Errorf("%s: computed hash is %v instead of %v", firstPath, got, fixedHash)
			}

			buf := new(bytes.Buffer)
			nw := nar.NewWriter(buf)
			var emptyRewrites iter.Seq2[string, zbstore.Path] = func(yield func(string, zbstore.Path) bool) {}
			if err := copyTxtarToNAR(nw, firstPath, firstFileData, nil, emptyRewrites); err != nil {
				return result, err
			}
			ca := nix.FlatFileContentAddress(fixedHash)
			path, err := zbstore.FixedCAOutputPath(dir, firstPath.name(), ca, zbstore.References{})
			if err != nil {
				return result, fmt.Errorf("%s: %v", firstPath, err)
			}
			if err := nw.Close(); err != nil {
				return result, fmt.Errorf("%s: %v", firstPath, err)
			}
			obj := &zbstore.Blob{
				NAR: buf.Bytes(),
				ExportTrailer: zbstore.ExportTrailer{
					StorePath:      path,
					ContentAddress: ca,
				},
			}
			if err := addToTxtarStore(result, firstPath.objectName(), obj, labels); err != nil {
				return result, err
			}
			continue

		case firstPath.isSingleFile() && len(objectFiles) == 1 && strings.HasSuffix(firstPath.objectName(), zbstore.DerivationExt):
			// Special case: derivations.
			drvName := strings.TrimSuffix(firstPath.name(), zbstore.DerivationExt)
			drv, err := rewriteTxtarDerivation(dir, drvName, firstFileData, maps.All(result.Rewrites))
			if err != nil {
				return result, fmt.Errorf("rewrite %s: %v", firstPath, err)
			}
			obj, err := drv.Export(nix.SHA256)
			if err != nil {
				return result, fmt.Errorf("rewrite %s: %v", firstPath, err)
			}
			if err := addToTxtarStore(result, firstPath.objectName(), obj, labels); err != nil {
				return result, err
			}
			continue
		}

		buf := new(bytes.Buffer)
		nw := nar.NewWriter(buf)
		refs := new(sets.Sorted[zbstore.Path])
		if err := copyTxtarToNAR(nw, firstPath, firstFileData, refs, maps.All(result.Rewrites)); err != nil {
			return result, err
		}
		for _, other := range objectFiles[1:] {
			otherLabels, otherFilename, otherFixedHash, err := parseFilename(other.Name)
			if err != nil {
				return result, err
			}
			if len(otherLabels) > 0 {
				return result, fmt.Errorf("unexpected labels on %s", otherFilename)
			}
			if !otherFixedHash.IsZero() {
				return result, fmt.Errorf("unexpected hash on %s", otherFilename)
			}
			otherData := cleanFileData(other.Data)
			err = copyTxtarToNAR(nw, otherFilename, otherData, refs, maps.All(result.Rewrites))
			if err != nil {
				return result, err
			}
		}
		if err := nw.Close(); err != nil {
			return result, fmt.Errorf("%s: %v", firstPath.objectName(), err)
		}

		obj := &zbstore.Blob{NAR: buf.Bytes()}
		caOptions := new(zbstore.ContentAddressOptions)
		if digest, ok := firstPath.digest(); ok {
			caOptions.Digest = digest
		}
		ca, analysis, err := zbstore.SourceSHA256ContentAddress(bytes.NewReader(obj.NAR), caOptions)
		if err != nil {
			return result, fmt.Errorf("%s: %v", firstPath.objectName(), err)
		}
		obj.ContentAddress = ca
		storeRefs := zbstore.References{
			Self:   analysis.HasSelfReferences(),
			Others: *refs,
		}
		obj.StorePath, err = zbstore.FixedCAOutputPath(dir, firstPath.name(), obj.ContentAddress, storeRefs)
		if err != nil {
			return result, err
		}
		obj.References = *storeRefs.ToSet(obj.StorePath)
		newDigest := obj.StorePath.Digest()
		for _, rewrite := range analysis.Rewrites {
			readStart, readEnd := rewrite.ReadRange()
			replacement, err := rewrite.Rewrite(newDigest, bytes.NewReader(obj.NAR[readStart:readEnd]))
			if err != nil {
				return result, fmt.Errorf("%s: %v", firstPath.objectName(), err)
			}
			copy(obj.NAR[rewrite.WriteOffset():], replacement)
		}
		if err := addToTxtarStore(result, firstPath.objectName(), obj, labels); err != nil {
			return result, err
		}
	}
	return result, nil
}

func groupFilesByObject(files []txtar.File) iter.Seq[[]txtar.File] {
	return func(yield func([]txtar.File) bool) {
		for i := 0; i < len(files); {
			firstFileIndex := i
			_, firstPath, _, err := parseFilename(files[firstFileIndex].Name)
			i++
			if err == nil && !firstPath.isSingleFile() {
				for i < len(files) {
					_, curr, _, err := parseFilename(files[i].Name)
					if err != nil || curr.objectName() != firstPath.objectName() {
						break
					}
					i++
				}
			}
			if !yield(files[firstFileIndex:i]) {
				return
			}
		}
	}
}

func parseFilename(name string) (labels []string, path txtarPath, fixedHash nix.Hash, err error) {
	name = strings.TrimSpace(name)
	for {
		var hasLabel bool
		name, hasLabel = strings.CutPrefix(name, "[")
		if !hasLabel {
			break
		}
		var label string
		var labelEnds bool
		label, name, labelEnds = strings.Cut(name, "]")
		if !labelEnds {
			return nil, "", nix.Hash{}, fmt.Errorf("unclosed label %s", label)
		}
		labels = append(labels, label)
		name = strings.TrimLeft(name, " \t")
	}

	originalName := name
	name, hasFixedHash := strings.CutSuffix(name, "]")
	if hasFixedHash {
		nameEnd := strings.LastIndex(name, "[")
		if nameEnd == -1 {
			return nil, "", nix.Hash{}, fmt.Errorf("invalid file name %s", originalName)
		}
		hashString := name[nameEnd+len("["):]
		name = name[:nameEnd]
		var err error
		fixedHash, err = nix.ParseHash(hashString)
		if err != nil {
			return nil, "", nix.Hash{}, err
		}
		name = strings.TrimRight(name, " \t")
	}

	return labels, txtarPath(name), fixedHash, nil
}

func addToTxtarStore(store *TxtarStore, originalName string, object *zbstore.Blob, labels []string) error {
	if otherName, exists := store.OriginalObjectName(object.StorePath); exists {
		if otherName == originalName {
			return fmt.Errorf("duplicate object %s", originalName)
		}
		return fmt.Errorf("%s and %s are identical objects", otherName, originalName)
	}

	store.BlobSlice = append(store.BlobSlice, object)
	store.Rewrites[originalName] = object.StorePath
	if len(labels) > 0 {
		store.Labels[object.StorePath] = labels
	}
	return nil
}

type txtarPath string

// digest returns the digest part of the first element of the path, if present.
func (path txtarPath) digest() (_ string, ok bool) {
	const n = 32
	name := path.objectName()
	if len(name) < n+len("-x") || strings.IndexByte(name, '-') != n {
		return "", false
	}
	return name[:n], true
}

// name returns the part of the first element of the path after the digest,
// excluding the separating dash.
// If there is no digest, returns the first element of the path.
func (path txtarPath) name() string {
	if digest, ok := path.digest(); ok {
		return path.objectName()[len(digest)+len("-"):]
	}
	return path.objectName()
}

// objectName returns the first element of the path.
func (path txtarPath) objectName() string {
	objectName, _, _ := strings.Cut(string(path), "/")
	return objectName
}

// subpath strips the first element of the path.
func (path txtarPath) subpath() string {
	_, subpath, _ := strings.Cut(string(path), "/")
	subpath = strings.TrimLeft(subpath, "/")
	return subpath
}

// isDirectory reports whether the path ends in a slash.
func (path txtarPath) isDirectory() bool {
	return strings.HasSuffix(string(path), "/")
}

// isSingleFile reports whether the path represents a store object that is a single file.
func (path txtarPath) isSingleFile() bool {
	return !strings.Contains(string(path), "/")
}

func copyTxtarToNAR(nw *nar.Writer, path txtarPath, data []byte, refs *sets.Sorted[zbstore.Path], rewrites iter.Seq2[string, zbstore.Path]) error {
	h := &nar.Header{Path: path.subpath()}
	if path.isDirectory() {
		h.Mode = fs.ModeDir
	} else {
		h.Size = int64(len(data))
	}
	if err := nw.WriteHeader(h); err != nil {
		return fmt.Errorf("serialize %s to nar: %v", path, err)
	}
	if !path.isDirectory() {
		for oldName, newPath := range rewrites {
			if oldDigest, ok := txtarPath(oldName).digest(); ok {
				if bytes.Contains(data, []byte(oldDigest)) {
					refs.Add(newPath)
					data = bytes.ReplaceAll(data, []byte(oldDigest), []byte(newPath.Digest()))
				}
			}
		}
		if _, err := nw.Write(data); err != nil {
			return fmt.Errorf("serialize %s to nar: %v", path, err)
		}
	}
	return nil
}

func cleanFileData(data []byte) []byte {
	// Convert CRLF to LF.
	data = bytes.ReplaceAll(data, []byte("\r"), nil)
	return data
}

func rewriteTxtarDerivation(dir zbstore.Directory, drvName string, data []byte, rewrites iter.Seq2[string, zbstore.Path]) (*zbstore.Derivation, error) {
	data, err := MinimizeDerivation(data)
	if err != nil {
		return nil, err
	}
	drv := &zbstore.Derivation{Name: drvName}
	if err := drv.UnmarshalText(data); err != nil {
		return nil, err
	}

	if drv.Dir != "" {
		var replacements []string
		for oldBase, newPath := range rewrites {
			oldPath, err := drv.Dir.Object(oldBase)
			if err != nil {
				continue
			}
			replacements = append(replacements, string(oldPath), string(newPath))
			if drv.InputSources.Has(oldPath) {
				drv.InputSources.Delete(oldPath)
				drv.InputSources.Add(newPath)
			}
			for outputName := range drv.InputDerivations[oldPath].Values() {
				oldPlaceholder := zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
					DrvPath:    oldPath,
					OutputName: outputName,
				})
				newPlaceholder := zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
					DrvPath:    newPath,
					OutputName: outputName,
				})
				replacements = append(replacements, oldPlaceholder, newPlaceholder)
			}
			if outputNames, ok := drv.InputDerivations[oldPath]; ok {
				drv.InputDerivations[newPath] = outputNames
				delete(drv.InputDerivations, oldPath)
			}
		}
		drv = drv.ReplaceStrings(strings.NewReplacer(replacements...))
	}
	drv.Dir = dir

	return drv, nil
}

// MinimizeDerivation removes all whitespace between tokens in the derivation data.
// If an error is encountered, then data is returned as-is along with the error.
func MinimizeDerivation(data []byte) ([]byte, error) {
	const prefix = "Derive"
	atermData, ok := bytes.CutPrefix(data, []byte(prefix))
	if !ok {
		return data, fmt.Errorf("minimize derivation: line 1: data does not start with %q", prefix)
	}
	noWhitespace := make([]byte, 0, len(data))
	noWhitespace = append(noWhitespace, prefix...)
	atermReader := bytes.NewReader(atermData)
	atermScanner := aterm.NewScanner(atermReader)
	atermScanner.AllowWhitespace()
	for first := true; ; {
		tok, err := atermScanner.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return data, fmt.Errorf("minimize derivation: line %d: %v", readerLineNumber(atermReader), err)
		}
		if !first && tok.Kind != aterm.RParen && tok.Kind != aterm.RBracket {
			noWhitespace = append(noWhitespace, ',')
		}
		first = tok.Kind == aterm.LParen || tok.Kind == aterm.LBracket
		noWhitespace, err = tok.AppendText(noWhitespace)
		if err != nil {
			return data, fmt.Errorf("minimize derivation: line %d: %v", readerLineNumber(atermReader), err)
		}
	}
	readEnd := len(data) - atermReader.Len()
	if !isBlank(data[readEnd:]) {
		return data, fmt.Errorf("minimize derivation: line %d: trailing data", readerLineNumber(atermReader))
	}
	return noWhitespace, nil
}

func readerLineNumber(r *bytes.Reader) int {
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		panic(err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}
	lineno := 1
	for range pos {
		b, err := r.ReadByte()
		if err != nil {
			panic(err)
		}
		if b == '\n' {
			lineno++
		}
	}
	return lineno
}

func isBlank(s []byte) bool {
	for _, b := range s {
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return false
		}
	}
	return true
}
