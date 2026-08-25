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
		log.Warnf(ctx, "Rejecting %s: not in %s", trailer.StorePath, r.dir)
		return
	}
	storeRefs := zbstore.MakeReferences(trailer.StorePath, &trailer.References)
	if err := zbstore.ValidateContentAddress(trailer.ContentAddress, storeRefs); err != nil {
		log.Warnf(ctx, "Rejecting %s: %v", trailer.StorePath, err)
		return
	}
	unlock, err := r.writing.lock(ctx, trailer.StorePath)
	if err != nil {
		log.Errorf(ctx, "Failed to lock %s: %v", trailer.StorePath, err)
		return
	}
	defer unlock()

	storeDir, err := os.OpenRoot(r.realDir)
	if err != nil {
		log.Errorf(ctx, "Import of %s failed: open store directory: %v", trailer.StorePath, err)
		return
	}
	defer storeDir.Close()
	base := trailer.StorePath.Base()
	realPath := filepath.Join(r.realDir, base)
	if _, err := storeDir.Lstat(base); err == nil {
		log.Debugf(ctx, "Received NAR for %s. Exists in store, skipping...", trailer.StorePath)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Errorf(ctx, "Received NAR for %s. Failed to check for existence: %v", trailer.StorePath, err)
		return
	}

	log.Debugf(ctx, "Extracting %s.nar to %s...", trailer.StorePath, realPath)
	info := &zbstore.ObjectInfo{
		StorePath:      trailer.StorePath,
		NARSize:        r.size,
		NARHash:        r.hasher.SumHash(),
		ContentAddress: trailer.ContentAddress,
		References:     trailer.References,
	}
	pr, pw := io.Pipe()
	obj := &pipeObject{info: *info, narContent: r.tmpFile}
	verifyDone := make(chan error)
	go func() {
		_, err := zbstore.VerifyObject(ctx, pw, obj, &zbstore.ContentAddressOptions{
			CreateTemp: r.caCreateTemp,
			Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
		})
		pw.CloseWithError(err)
		verifyDone <- err
	}()
	extractError := extractNAR(storeDir, base, pr)
	pr.Close()
	verifyError := <-verifyDone
	if extractError != nil || verifyError != nil {
		log.Warnf(ctx, "Import of %s failed: %v", trailer.StorePath, errors.Join(extractError, verifyError))
		if err := storeDir.RemoveAll(base); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", trailer.StorePath, err)
		}
		return
	}

	log.Debugf(ctx, "Recording import of %s...", trailer.StorePath)
	conn, err := r.dbPool.Get(ctx)
	if err != nil {
		log.Warnf(ctx, "Connecting to store database: %v", err)
		if err := storeDir.RemoveAll(base); err != nil {
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
		return insertObject(ctx, conn, info)
	}()
	if err != nil {
		log.Errorf(ctx, "Recording import of %s: %v", trailer.StorePath, err)
		if err := storeDir.RemoveAll(base); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", trailer.StorePath, err)
		}
		return
	}

	freeze(ctx, realPath)

	log.Infof(ctx, "Imported %s", trailer.StorePath)
}

type pipeObject struct {
	info       zbstore.ObjectInfo
	narContent io.Reader
}

func (obj *pipeObject) Info() *zbstore.ObjectInfo {
	return &obj.info
}

func (obj *pipeObject) WriteNAR(ctx context.Context, w io.Writer) error {
	_, err := io.Copy(w, obj.narContent)
	return err
}

// extractNAR extracts a NAR file to the local filesystem at the given path.
func extractNAR(root *os.Root, dst string, r io.Reader) error {
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
			f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
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
			if err := root.Mkdir(p, 0o755); err != nil {
				return err
			}
		case fs.ModeSymlink:
			if err := root.Symlink(hdr.LinkTarget, p); err != nil {
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
