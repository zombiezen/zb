// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"

	"zb.256lights.llc/pkg/internal/osutil"
	"zb.256lights.llc/pkg/internal/useragent"
	"zb.256lights.llc/pkg/internal/xhttp"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

const (
	builtinSystem        = "builtin"
	builtinBuilderPrefix = "builtin:"
)

// runBuiltin runs a pre-defined builder function.
// It satisfies the [runnerFunc] signature.
func runBuiltin(ctx context.Context, invocation *builderInvocation) error {
	switch invocation.derivation.Builder {
	case builtinBuilderPrefix + "fetchurl":
		if err := fetchURL(ctx, invocation.derivation, invocation.realStoreDir); err != nil {
			fmt.Fprintf(invocation.logWriter, "%s: %v\n", invocation.derivation.Builder, err)
			return builderFailure{fmt.Errorf("%s failed", invocation.derivation.Builder)}
		}
		return nil
	case builtinBuilderPrefix + "extract":
		if err := extract(ctx, invocation.derivation, invocation.realStoreDir); err != nil {
			fmt.Fprintf(invocation.logWriter, "%s: %v\n", invocation.derivation.Builder, err)
			return builderFailure{fmt.Errorf("%s failed", invocation.derivation.Builder)}
		}
		return nil
	default:
		return builderFailure{fmt.Errorf("builtin %q not found", invocation.derivation.Builder)}
	}
}

func fetchURL(ctx context.Context, drv *zbstore.Derivation, realStoreDir string) error {
	href := drv.Env["url"]
	if href == "" {
		return fmt.Errorf("missing url environment variable")
	}
	outputPath := drv.Env[zbstore.DefaultDerivationOutputName]
	if outputPath == "" {
		return fmt.Errorf("missing %s environment variable", zbstore.DefaultDerivationOutputName)
	}
	outputPath = strings.ReplaceAll(outputPath, string(drv.Dir), realStoreDir)
	if !drv.Outputs.IsFixed() {
		return fmt.Errorf("output is not fixed")
	}
	executable := drv.Env["executable"] != ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", useragent.String)
	req.Header.Set("Accept", "*/*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("get %s: %v", href, xhttp.ErrorFromResponse(resp))
	}
	perm := os.FileMode(0o644)
	if executable {
		perm |= 0o111
	}
	f, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err1 := io.Copy(f, resp.Body)
	err2 := f.Close()
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return nil
}

func extract(ctx context.Context, drv *zbstore.Derivation, realStoreDir string) error {
	src := strings.ReplaceAll(drv.Env["src"], string(drv.Dir), realStoreDir)
	if !filepath.IsAbs(src) {
		return fmt.Errorf("source %s is not absolute", src)
	}
	outputPath := drv.Env[zbstore.DefaultDerivationOutputName]
	if outputPath == "" {
		return fmt.Errorf("missing %s environment variable", zbstore.DefaultDerivationOutputName)
	}
	stripFirstComponent := drv.Env["stripFirstComponent"] == "1"

	srcObject, subpath, err := drv.Dir.ParsePath(src)
	if err != nil {
		return err
	}
	archiveFile, err := os.OpenInRoot(realStoreDir, filepath.Join(srcObject.Base(), filepath.FromSlash(subpath)))
	if err != nil {
		return err
	}
	defer func() {
		if err := archiveFile.Close(); err != nil {
			log.Debugf(ctx, "Closing archive file %s: %v", src, err)
		}
	}()

	header := make([]byte, 4)
	n, err := io.ReadFull(archiveFile, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read %s: %v", src, err)
	}
	header = header[:n]

	switch {
	case hasTarMagic(header):
		r := io.MultiReader(bytes.NewReader(header), archiveFile)
		if err := extractTar(outputPath, r, stripFirstComponent); err != nil {
			return fmt.Errorf("extract %s: %v", src, err)
		}
	case hasBzip2Magic(header):
		r := bzip2.NewReader(io.MultiReader(bytes.NewReader(header), archiveFile))
		if err := extractTar(outputPath, r, stripFirstComponent); err != nil {
			return fmt.Errorf("extract %s: %v", src, err)
		}
	case hasGzipMagic(header):
		r, err := gzip.NewReader(io.MultiReader(bytes.NewReader(header), archiveFile))
		if err != nil {
			return fmt.Errorf("extract %s: %v", src, err)
		}
		if err := extractTar(outputPath, r, stripFirstComponent); err != nil {
			return fmt.Errorf("extract %s: %v", src, err)
		}
	case hasZipMagic(header):
		size, err := archiveFile.Seek(0, io.SeekEnd)
		if err != nil {
			return fmt.Errorf("read %s: %v", src, err)
		}
		if err := extractZip(outputPath, archiveFile, size, stripFirstComponent); err != nil {
			return fmt.Errorf("extract %s: %v", src, err)
		}
	case hasXZMagic(header):
		return fmt.Errorf("extract %s: xz not supported", err)
	default:
		return fmt.Errorf("extract %s: unknown format (must be .tar, .tar.gz, .tar.bz2, or .zip)", err)
	}

	return nil
}

// extractTar extracts the tar archive from the given stream to the file system at dst.
// If stripFirstComponent is true,
// then extractTar assumes that the archive contains a single top-level file or directory
// and extracts that to dst.
// Otherwise, extractTar creates a new directory at dst and extracts the archive's contents into it.
func extractTar(dst string, src io.Reader, stripFirstComponent bool) error {
	if !stripFirstComponent {
		if err := os.Mkdir(dst, 0o777); err != nil {
			return err
		}
	}

	// Peek at first file header.
	r := tar.NewReader(src)
	hdr, err := nextSupportedTarHeader(r)
	if err != nil {
		if err == io.EOF {
			if stripFirstComponent {
				err = errors.New("empty archive")
			} else {
				err = nil
			}
		}
		return err
	}
	var strip string
	if stripFirstComponent {
		var onlyComponent bool
		var err error
		strip, onlyComponent, err = firstPathComponent(hdr.Name)
		if err != nil {
			return err
		}
		if onlyComponent {
			// Extract the root and advance the reader to the next entry.
			if err := extractTarFile(nil, dst, r, hdr); err != nil {
				return err
			}
			var err error
			hdr, err = nextSupportedTarHeader(r)
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return err
			}
		} else {
			// For a nested file, create the top-level directory, then process as normal.
			if err := os.Mkdir(dst, 0o777); err != nil {
				return err
			}
		}
	}

	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()

	for {
		name, hasPrefix := strings.CutPrefix(hdr.Name, strip)
		if !hasPrefix {
			return errors.New("zip archive contains multiple top-level files")
		}
		subdst, err := filepath.Localize(slashpath.Clean(name))
		if err != nil {
			return err
		}
		if dir := filepath.Dir(subdst); root == nil {
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return err
			}
		} else {
			if err := osutil.MkdirAllInRoot(root, dir, 0o777); err != nil {
				return err
			}
		}
		if err := extractTarFile(root, subdst, r, hdr); err != nil {
			return err
		}

		hdr, err = nextSupportedTarHeader(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func nextSupportedTarHeader(r *tar.Reader) (*tar.Header, error) {
	for {
		hdr, err := r.Next()
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			// Ignore.
		//lint:ignore SA1019 TypeRegA shouldn't be returned, but handled just in case.
		case tar.TypeReg, tar.TypeRegA, tar.TypeSymlink, tar.TypeDir:
			return hdr, nil
		default:
			return hdr, fmt.Errorf("unsupported tar entry type %q", hdr.Typeflag)
		}
	}
}

func extractTarFile(root *os.Root, dst string, r *tar.Reader, hdr *tar.Header) error {
	mode := hdr.FileInfo().Mode()
	var open func() (io.ReadCloser, error)
	if mode.Type() == fs.ModeSymlink {
		open = func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(hdr.Linkname)), nil
		}
	} else {
		open = func() (io.ReadCloser, error) {
			return io.NopCloser(r), nil
		}
	}
	return extractFile(root, dst, mode, open)
}

// extractZip extracts the Zip archive from the given stream to the file system at dst.
// If stripFirstComponent is true,
// then extractZip assumes that the archive contains a single top-level file or directory
// and extracts that to dst.
// Otherwise, extractZip creates a new directory at dst and extracts the archive's contents into it.
func extractZip(dst string, src io.ReaderAt, srcSize int64, stripFirstComponent bool) error {
	r, err := zip.NewReader(src, srcSize)
	if err != nil {
		return err
	}

	top := "."
	if stripFirstComponent {
		if len(r.File) == 0 {
			return errors.New("empty archive")
		}
		strip, onlyComponent, err := firstPathComponent(r.File[0].Name)
		if err != nil {
			return err
		}
		var isDir bool
		top, isDir = strings.CutSuffix(strip, "/")
		if onlyComponent && !isDir {
			// Only a single file?
			if len(r.File) > 1 {
				return errors.New("zip archive contains multiple top-level files")
			}
			return extractFile(nil, dst, r.File[0].Mode(), r.File[0].Open)
		}
		for _, f := range r.File[1:] {
			if !strings.HasPrefix(f.Name, strip) {
				return errors.New("zip archive contains multiple top-level files")
			}
		}
	}

	if err := os.Mkdir(dst, 0o777); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()

	return fs.WalkDir(r, top, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == top {
			return nil
		}
		relpath := path
		if top != "." {
			relpath = path[len(top)+len("/"):]
		}
		subdst, err := filepath.Localize(relpath)
		if err != nil {
			return err
		}
		mode := entry.Type()
		if mode.IsRegular() {
			// Regular files need to check for permission bits.
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode = info.Mode()
		}
		return extractFile(root, subdst, mode, func() (io.ReadCloser, error) {
			return r.Open(path)
		})
	})
}

func extractFile(root *os.Root, dst string, mode fs.FileMode, open func() (io.ReadCloser, error)) error {
	switch mode.Type() {
	case 0:
		perm := os.FileMode(0o666)
		if mode&0o111 != 0 {
			perm |= 0o111
		}
		r, err := open()
		if err != nil {
			return err
		}
		defer r.Close()
		var w *os.File
		if root == nil {
			w, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|osutil.O_NOFOLLOW, perm)
		} else {
			w, err = root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|osutil.O_NOFOLLOW, perm)
		}
		if err != nil {
			return err
		}
		_, err1 := io.Copy(w, r)
		err2 := w.Close()
		if err1 != nil {
			return fmt.Errorf("write %s: %v", dst, err1)
		}
		if err2 != nil {
			return fmt.Errorf("write %s: %v", dst, err2)
		}
	case fs.ModeDir:
		if root == nil {
			return os.Mkdir(dst, 0o777)
		}
		return root.Mkdir(dst, 0o777)
	case fs.ModeSymlink:
		r, err := open()
		if err != nil {
			return err
		}
		sb := new(strings.Builder)
		_, err = io.Copy(sb, r)
		r.Close()
		if err != nil {
			return fmt.Errorf("read %s: %v", dst, err)
		}
		if root == nil || filepath.IsAbs(dst) {
			return os.Symlink(sb.String(), dst)
		}
		// TODO(someday): https://go.dev/issue/67002 tracks adding *os.Root.Symlink.
		return os.Symlink(sb.String(), filepath.Join(root.Name(), dst))
	default:
		return fmt.Errorf("unsupported archive member with mode %v", mode)
	}

	return nil
}

// firstPathComponent returns the first component of the slash-separated relative path,
// including the trailing slash if present.
func firstPathComponent(path string) (_ string, only bool, err error) {
	end := strings.IndexByte(path, '/')
	if end == 0 {
		return "", false, fmt.Errorf("invalid file name %s", path)
	}
	var afterSlash int
	if end == -1 {
		end = len(path)
		afterSlash = end
	} else {
		afterSlash = end + 1
	}
	name := path[:end]
	if name == "." || name == ".." {
		return "", false, fmt.Errorf("invalid file name %s", path)
	}
	cleanedFirst, _, cleanedHasSlash := strings.Cut(slashpath.Clean(path), "/")
	if cleanedFirst != name {
		return "", false, fmt.Errorf("invalid file name %s", path)
	}
	return path[:afterSlash], !cleanedHasSlash, nil
}

func hasBzip2Magic(header []byte) bool {
	return len(header) >= 3 && header[0] == 'B' && header[1] == 'Z' && header[2] == 'h'
}

func hasZipMagic(header []byte) bool {
	return len(header) >= 4 &&
		header[0] == 'P' &&
		header[1] == 'K' &&
		(header[2] == 0x03 && header[3] == 0x04 ||
			header[2] == 0x05 && header[3] == 0x06 ||
			header[2] == 0x07 && header[3] == 0x08)
}

func hasGzipMagic(header []byte) bool {
	return len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b
}

func hasXZMagic(header []byte) bool {
	return len(header) >= 6 &&
		header[0] == 0xfd &&
		header[1] == '7' &&
		header[2] == 'z' &&
		header[3] == 'X' &&
		header[4] == 'Z' &&
		header[5] == 0
}

func hasTarMagic(header []byte) bool {
	return len(header) >= 8 &&
		header[0] == 'u' &&
		header[1] == 's' &&
		header[2] == 't' &&
		header[3] == 'a' &&
		header[4] == 'r' &&
		(header[5] == 0 && header[6] == '0' && header[7] == '0' ||
			header[5] == ' ' && header[6] == ' ' && header[7] == 0)
}
