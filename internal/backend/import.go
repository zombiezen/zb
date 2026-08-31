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

type importer struct {
	dir          zbstore.Directory
	realDir      string
	dbPool       connectionGetter
	writing      *mutexMap[zbstore.Path]
	caCreateTemp bytebuffer.Creator
}

func (s *Server) newImporter(getter connectionGetter) *importer {
	// nils are easier to catch at this point on the stack than later.
	if getter == nil {
		panic("nil connectionGetter passed to NewNARReceiver")
	}

	return &importer{
		dir:          s.dir,
		realDir:      s.realDir,
		dbPool:       getter,
		writing:      &s.writing,
		caCreateTemp: s.caCreateTemp,
	}
}

func (imp *importer) WriteObject(ctx context.Context, obj zbstore.Object) error {
	info := obj.Info()
	if info.StorePath.Dir() != imp.dir {
		return fmt.Errorf("import %s: not in %s", info.StorePath, imp.dir)
	}
	storeRefs := zbstore.MakeReferences(info.StorePath, &info.References)
	if err := zbstore.ValidateContentAddress(info.ContentAddress, storeRefs); err != nil {
		return fmt.Errorf("import %s: %v", info.StorePath, err)
	}
	unlock, err := imp.writing.lock(ctx, info.StorePath)
	if err != nil {
		return fmt.Errorf("import %s: %v", info.StorePath, err)
	}
	defer unlock()

	storeDir, err := os.OpenRoot(imp.realDir)
	if err != nil {
		return fmt.Errorf("import %s: open store directory: %v", info.StorePath, err)
	}
	defer storeDir.Close()
	base := info.StorePath.Base()
	realPath := filepath.Join(imp.realDir, base)
	if _, err := storeDir.Lstat(base); err == nil {
		log.Debugf(ctx, "Received NAR for %s. Exists in store, skipping...", info.StorePath)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("import %s: %v", info.StorePath, err)
	}

	log.Debugf(ctx, "Extracting %s.nar to %s...", info.StorePath, realPath)
	info = info.Clone()
	var hasher *nix.Hasher
	if info.NARHash.IsZero() {
		hasher = nix.NewHasher(nix.SHA256)
	}
	pr, pw := io.Pipe()
	verifyDone := make(chan error)
	go func() {
		var verifyWriter io.Writer = pw
		if hasher != nil {
			verifyWriter = io.MultiWriter(hasher, pw)
		}
		var err error
		info.NARSize, err = zbstore.VerifyObject(ctx, verifyWriter, obj, &zbstore.ContentAddressOptions{
			CreateTemp: imp.caCreateTemp,
			Log:        func(msg string) { log.Debugf(ctx, "%s", msg) },
		})
		pw.CloseWithError(err)
		verifyDone <- err
	}()
	extractError := extractNAR(storeDir, base, pr)
	pr.Close()
	verifyError := <-verifyDone
	if extractError != nil || verifyError != nil {
		if err := storeDir.RemoveAll(base); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", info.StorePath, err)
		}
		return fmt.Errorf("import %s: %v", info.StorePath, errors.Join(extractError, verifyError))
	}

	log.Debugf(ctx, "Recording import of %s...", info.StorePath)
	if hasher != nil {
		info.NARHash = hasher.SumHash()
	}
	conn, err := imp.dbPool.Get(ctx)
	if err != nil {
		if err := storeDir.RemoveAll(base); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", info.StorePath, err)
		}
		return fmt.Errorf("import %s: open store database: %v", info.StorePath, err)
	}
	defer imp.dbPool.Put(conn)
	err = func() (err error) {
		endFn, err := sqlitex.ImmediateTransaction(conn)
		if err != nil {
			return err
		}
		defer endFn(&err)
		return insertObject(ctx, conn, info)
	}()
	if err != nil {
		if err := storeDir.RemoveAll(base); err != nil {
			log.Errorf(ctx, "Failed to clean up partial import of %s: %v", info.StorePath, err)
		}
		return fmt.Errorf("import %s: record import: %v", info.StorePath, err)
	}

	freeze(ctx, realPath)

	log.Infof(ctx, "Imported %s", info.StorePath)
	return nil
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

// singleConnectionGetter is a [connectionGetter] that returns a single connection.
type singleConnectionGetter struct {
	conn chan *sqlite.Conn
}

func newSingleConnectionGetter(conn *sqlite.Conn) *singleConnectionGetter {
	g := &singleConnectionGetter{conn: make(chan *sqlite.Conn, 1)}
	g.conn <- conn
	return g
}

func (g *singleConnectionGetter) Get(ctx context.Context) (*sqlite.Conn, error) {
	select {
	case conn := <-g.conn:
		return conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *singleConnectionGetter) Put(conn *sqlite.Conn) {
	select {
	case g.conn <- conn:
	default:
		panic("mismatched connection")
	}
}
