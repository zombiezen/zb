// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zb.256lights.llc/pkg/bytebuffer"
	"zb.256lights.llc/pkg/internal/osutil"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type connectionGetter interface {
	Get(ctx context.Context) (*sqlite.Conn, error)
	Put(conn *sqlite.Conn)
}

// NARReceiver is a per-connection [zbstore.NARReceiver].
type NARReceiver struct {
	ctx     context.Context
	dir     zbstore.Directory
	realDir string
	dbPool  connectionGetter
	writing *mutexMap[zbstore.Path]

	tmpFileCreator bytebuffer.Creator
	tmpFile        bytebuffer.ReadWriteSeekCloser

	hasher       nix.Hasher
	size         int64
	caCreateTemp bytebuffer.Creator
}

// NewNARReceiver returns a new [NARReceiver] that is attached to the server.
// Callers are responsible for calling [NARReceiver.Cleanup] after the receiver is no longer in use.
func (s *Server) NewNARReceiver(ctx context.Context, bufCreator bytebuffer.Creator) *NARReceiver {
	return s.newNARReceiver(ctx, bufCreator, s.db)
}

func (s *Server) newNARReceiver(ctx context.Context, bufCreator bytebuffer.Creator, getter connectionGetter) *NARReceiver {
	// nils are easier to catch at this point on the stack than later.
	if ctx == nil {
		panic("nil context passed to NewNARReceiver")
	}
	if bufCreator == nil {
		panic("nil bytebuffer.Creator passed to NewNARReceiver")
	}

	return &NARReceiver{
		ctx:            ctx,
		dir:            s.dir,
		realDir:        s.realDir,
		dbPool:         getter,
		writing:        &s.writing,
		tmpFileCreator: bufCreator,
		hasher:         *nix.NewHasher(nix.SHA256),
	}
}

func (r *NARReceiver) Write(p []byte) (n int, err error) {
	if r.tmpFile == nil {
		r.tmpFile, err = r.tmpFileCreator.CreateBuffer(-1)
		if err != nil {
			return 0, err
		}
	}
	n, err = r.tmpFile.Write(p)
	r.hasher.Write(p[:n])
	r.size += int64(n)
	return n, err
}

func (r *NARReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	ctx := r.ctx
	if r.tmpFile == nil {
		// No bytes written? Not a valid NAR.
		return
	}
	if _, err := r.tmpFile.Seek(0, io.SeekStart); err != nil {
		log.Errorf(ctx, "Unable to seek in store temp file: %v", err)
		r.Cleanup(ctx)
		return
	}
	defer func() {
		if err := truncateIfPossible(r.tmpFile, 0); err != nil {
			log.Warnf(ctx, "Unable to truncate store temp file: %v", err)
		}
		if _, err := r.tmpFile.Seek(0, io.SeekStart); err != nil {
			log.Errorf(ctx, "Unable to seek in store temp file: %v", err)
			r.Cleanup(ctx)
			return
		}
		r.hasher.Reset()
		r.size = 0
	}()

	if trailer.StorePath.Dir() != r.dir {
		log.Warnf(ctx, "Rejecting %s (not in %s)", trailer.StorePath, r.dir)
		return
	}
	ca, err := verifyContentAddress(r.ctx, trailer.StorePath, io.LimitReader(r.tmpFile, r.size), &trailer.References, trailer.ContentAddress, r.caCreateTemp)
	if err != nil {
		log.Warnf(ctx, "%v", err)
		return
	}
	if _, err := r.tmpFile.Seek(0, io.SeekStart); err != nil {
		log.Errorf(ctx, "Unable to seek in store temp file: %v", err)
		return
	}

	unlock, err := r.writing.lock(ctx, trailer.StorePath)
	if err != nil {
		log.Errorf(ctx, "Failed to lock %s: %v", trailer.StorePath, err)
		return
	}
	defer unlock()

	realPath := filepath.Join(r.realDir, trailer.StorePath.Base())
	if _, err := os.Lstat(realPath); err == nil {
		log.Debugf(ctx, "Received NAR for %s. Exists in store, skipping...", trailer.StorePath)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Errorf(ctx, "Received NAR for %s. Failed to check for existence: %v", trailer.StorePath, err)
		return
	}

	log.Debugf(ctx, "Extracting %s.nar to %s...", trailer.StorePath, realPath)
	if err := extractNAR(realPath, io.LimitReader(r.tmpFile, r.size)); err != nil {
		log.Warnf(ctx, "Import of %s failed: %v", trailer.StorePath, err)
		if err := os.RemoveAll(realPath); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", trailer.StorePath, err)
		}
		return
	}

	log.Debugf(ctx, "Recording import of %s...", trailer.StorePath)
	conn, err := r.dbPool.Get(ctx)
	if err != nil {
		log.Warnf(ctx, "Connecting to store database: %v", err)
		if err := os.RemoveAll(realPath); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", trailer.StorePath, err)
		}
		return
	}
	defer r.dbPool.Put(conn)
	err = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)

		return insertObject(ctx, conn, &zbstore.ObjectInfo{
			StorePath:      trailer.StorePath,
			NARSize:        r.size,
			NARHash:        r.hasher.SumHash(),
			ContentAddress: ca,
			References:     trailer.References,
		})
	}()
	if err != nil {
		log.Errorf(ctx, "Recording import of %s: %v", trailer.StorePath, err)
		if err := os.RemoveAll(realPath); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", trailer.StorePath, err)
		}
		return
	}

	freeze(ctx, realPath)

	log.Infof(ctx, "Imported %s", trailer.StorePath)
}

// verifyContentAddress validates that the content matches the given content address.
// If the content address is the zero value,
// then the content address is computed as a "source" store object.
func verifyContentAddress(ctx context.Context, path zbstore.Path, narContent io.Reader, refs *sets.Sorted[zbstore.Path], ca nix.ContentAddress, createTemp bytebuffer.Creator) (nix.ContentAddress, error) {
	if !ca.IsZero() {
		obj := &fakeObject{
			narContent: narContent,
			trailer: zbstore.ExportTrailer{
				StorePath:      path,
				ContentAddress: ca,
			},
		}
		if refs != nil {
			obj.trailer.References = *refs
		}
		opts := &zbstore.ContentAddressOptions{
			CreateTemp: createTemp,
			Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
		}
		if _, err := zbstore.VerifyObject(ctx, io.Discard, obj, opts); err != nil {
			return nix.ContentAddress{}, err
		}
		return ca, nil
	}

	storeRefs := zbstore.MakeReferences(path, refs)
	var digest string
	if storeRefs.Self {
		digest = path.Digest()
	}
	computed, _, err := zbstore.SourceSHA256ContentAddress(narContent, &zbstore.ContentAddressOptions{
		Digest:     digest,
		CreateTemp: createTemp,
		Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
	})
	if err != nil {
		return nix.ContentAddress{}, fmt.Errorf("verify %s content address: %v", path, err)
	}

	computedPath, err := zbstore.FixedCAOutputPath(path.Dir(), path.Name(), computed, storeRefs)
	if err != nil {
		return nix.ContentAddress{}, fmt.Errorf("verify %s content address: %v", path, err)
	}
	if path != computedPath {
		return nix.ContentAddress{}, fmt.Errorf("verify %s content address: does not match computed path %s", path, computedPath)
	}

	return computed, nil
}

type fakeObject struct {
	trailer zbstore.ExportTrailer

	contentWrite sync.Once
	narContent   io.Reader
}

func (obj *fakeObject) Info() *zbstore.ObjectInfo {
	return &zbstore.ObjectInfo{
		StorePath:      obj.trailer.StorePath,
		References:     obj.trailer.References,
		ContentAddress: obj.trailer.ContentAddress,
	}
}

func (obj *fakeObject) WriteNAR(ctx context.Context, w io.Writer) error {
	err := errMultipleReads
	obj.contentWrite.Do(func() {
		_, err = io.Copy(w, obj.narContent)
	})
	return err
}

var errMultipleReads = errors.New("object cannot be read more than once")

// extractNAR extracts a NAR file to the local filesystem at the given path.
func extractNAR(dst string, r io.Reader) error {
	nr := nar.NewReader(r)
	for {
		hdr, err := nr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dst, filepath.FromSlash(hdr.Path))
		switch typ := hdr.Mode.Type(); typ {
		case 0:
			perm := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				perm = 0o755
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, nr)
			err2 := f.Close()
			if err != nil {
				return err
			}
			if err2 != nil {
				return err2
			}
		case fs.ModeDir:
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
		case fs.ModeSymlink:
			if err := os.Symlink(hdr.LinkTarget, p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unhandled type %v", typ)
		}
	}
}

// Cleanup releases any resources associated with the receiver.
func (r *NARReceiver) Cleanup(ctx context.Context) {
	if r.tmpFile == nil {
		return
	}
	if err := r.tmpFile.Close(); err != nil {
		log.Warnf(ctx, "Unable to close store temp file: %v", err)
	}
	r.tmpFile = nil
}

func truncateIfPossible(f io.ReadWriteSeeker, size int64) error {
	t, ok := f.(interface{ Truncate(size int64) error })
	if !ok {
		return nil
	}
	return t.Truncate(size)
}

// freeze calls [osutil.Freeze]
// and logs any errors instead of causing them to stop the operation.
func freeze(ctx context.Context, path string) {
	log.Debugf(ctx, "Marking %s read-only...", path)
	osutil.Freeze(path, time.Unix(0, 0), func(err error) error {
		// Log errors, but don't abort the chmod attempt.
		// Subsequent use of this store object can still succeed,
		// and we want to mark as many files read-only as possible.
		log.Warnf(ctx, "%v", err)
		return nil
	})
}

type pathRecorderReceiver struct {
	paths    sets.Set[zbstore.Path]
	receiver zbstore.NARReceiver
}

func (prr *pathRecorderReceiver) ReceiveNAR(trailer *zbstore.ExportTrailer) {
	if prr.paths == nil {
		prr.paths = make(sets.Set[zbstore.Path])
	}
	prr.paths.Add(trailer.StorePath)

	prr.receiver.ReceiveNAR(trailer)
}

func (prr *pathRecorderReceiver) Write(p []byte) (int, error) {
	return prr.receiver.Write(p)
}

// singleConnectionGetter is a [connectionGetter] that returns a single connection.
type singleConnectionGetter struct {
	conn *sqlite.Conn
}

func (g *singleConnectionGetter) Get(ctx context.Context) (*sqlite.Conn, error) {
	return g.conn, nil
}

func (g *singleConnectionGetter) Put(conn *sqlite.Conn) {
	if conn != g.conn {
		panic("mismatched connection")
	}
}
