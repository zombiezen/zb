// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

// Package osutil provides convenience functions for working with the local filesystem.
package osutil

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// MkdirPerm creates a new directory with the given permission bits (after umask).
func MkdirPerm(name string, perm os.FileMode) error {
	if err := os.Mkdir(name, perm); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return nil
}

// WriteFilePerm writes data to the named file, creating it if necessary,
// and ensuring it has the given permissions (after umask).
func WriteFilePerm(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm|0o200)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %v", name, err)
	}
	err = f.Chmod(perm)
	err2 := f.Close()
	if err == nil {
		err = err2
	}
	if err != nil {
		return fmt.Errorf("write %s: %v", name, err)
	}
	return nil
}

// FirstPresentFile returns the first path in the sequence that exists in the filesystem,
// or an error if no path could be found.
func FirstPresentFile(paths iter.Seq[string]) (string, error) {
	var firstError, firstUnexpectedError error
	for path := range paths {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			return path, nil
		case !errors.Is(err, os.ErrNotExist):
			if firstUnexpectedError == nil {
				firstUnexpectedError = err
			}
		default:
			if firstError == nil {
				firstError = err
			}
		}
	}
	if firstUnexpectedError != nil {
		return "", firstUnexpectedError
	}
	if firstError == nil {
		firstError = errors.New("no files searched")
	}
	return "", firstError
}

// Freeze removes any write permissions on the filesystem object at the given path
// and adds read permissions for all users.
// If the path names a directory,
// then this applies recursively to any filesystem objects in the directory.
// If epoch is non-zero, the modification time of all files is set to epoch.
//
// If onError is not nil, it will be used to handle any errors encountered.
// Its return value is handled in the same manner as in [io/fs.WalkDirFunc].
func Freeze(path string, epoch time.Time, onError func(error) error) error {
	if onError == nil {
		onError = func(err error) error { return err }
	}
	return filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return onError(err)
		}

		existingMode := os.FileMode(0o666)
		if runtime.GOOS != "windows" {
			info, err := entry.Info()
			if err != nil {
				return onError(err)
			}
			const permMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky | os.ModeAppend | os.ModeExclusive | os.ModeTemporary
			existingMode = info.Mode() & permMask
		}

		newMode := (existingMode | 0o444) &^ (0o222 | os.ModeSetuid | os.ModeSetgid) // +r-sw
		if entry.IsDir() || existingMode&0o111 != 0 {
			newMode |= 0o111 // +x
		}
		if err := os.Chmod(path, newMode); err != nil {
			if err = onError(err); err != nil {
				return err
			}
		}

		if !epoch.IsZero() {
			if err := os.Chtimes(path, time.Time{}, epoch); err != nil {
				if err = onError(err); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// MkdirAllInRoot creates a directory inside root named path,
// along with any necessary parents, and returns nil,
// or else returns an error.
// The permission bits perm (before umask) are used for all
// directories that MkdirAllInRoot creates.
// If path is already a directory, MkdirAllInRoot does nothing
// and returns nil.
func MkdirAllInRoot(root *os.Root, path string, perm os.FileMode) error {
	// Fast path: if we can tell whether path is a directory or file, stop with success or error.
	dir, err := root.Stat(path)
	if err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}

	// Slow path: make sure parent exists and then call Mkdir for path.

	// Extract the parent folder from path by first removing any trailing
	// path separator and then scanning backward until finding a path
	// separator or reaching the beginning of the string.
	i := len(path) - 1
	for i >= 0 && os.IsPathSeparator(path[i]) {
		i--
	}
	for i >= 0 && !os.IsPathSeparator(path[i]) {
		i--
	}
	if i < 0 {
		i = 0
	}

	// If there is a parent directory, and it is not the volume name,
	// recurse to ensure parent directory exists.
	if parent := path[:i]; len(parent) > len(filepath.VolumeName(path)) {
		err = MkdirAllInRoot(root, parent, perm)
		if err != nil {
			return err
		}
	}

	// Parent now exists; invoke Mkdir and use its result.
	err = root.Mkdir(path, perm)
	if err != nil {
		// Handle arguments like "foo/." by
		// double-checking that directory doesn't exist.
		dir, err1 := root.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// UnmountAndRemoveAll removes path and any children it contains,
// unmounting any mount points encountered.
// It removes everything it can but returns the first error it encounters.
// If the path does not exist, RemoveAll returns nil (no error).
// If there is an error, it will be of type [*os.PathError].
//
// Generally this requires root privileges to run.
func UnmountAndRemoveAll(path string) error {
	return removeAll(path)
}

// ReadFileString reads the entire content of name into a string.
func ReadFileString(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sb := new(strings.Builder)
	if info, err := f.Stat(); err == nil {
		sb.Grow(int(info.Size()))
	}
	_, err = io.Copy(sb, f)
	return sb.String(), err
}
