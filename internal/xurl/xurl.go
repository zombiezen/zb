// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

// Package xurl provides functions for manipulating URIs.
package xurl

import (
	"fmt"
	"net/url"
	"strings"
)

// ResolveReference resolves a URI reference relative to a base URI,
// per [RFC 3986 Section 5.2].
// ResolveReference always returns a new [*url.URL],
// even if the returned URL is identical to either base or reference.
//
// [RFC 3986 Section 5.2]: https://datatracker.ietf.org/doc/html/rfc3986#section-5.2
func ResolveReference(base, ref *url.URL) *url.URL {
	// TODO(https://go.dev/issue/80282): Ideally, this would just be base.ResolveReference(ref).
	// But the standard library does not follow the RFC.

	base = CleanPath(base)
	if ref.Scheme != "" || ref.Host != "" || ref.User != nil {
		t := ref
		if t.Scheme == "" {
			t = new(*ref)
			t.Scheme = base.Scheme
		}
		return CleanPath(t)
	}
	t := &url.URL{
		Scheme:      base.Scheme,
		Host:        base.Host,
		User:        base.User,
		Fragment:    ref.Fragment,
		RawFragment: ref.RawFragment,
	}
	refPath := ref.EscapedPath()
	switch {
	case refPath == "":
		t.Path = base.Path
		t.RawPath = base.RawPath
	case strings.HasPrefix(refPath, "/"):
		t.Path = ref.Path
		t.RawPath = ref.RawPath
		t = CleanPath(t)
	case (base.Host != "" || base.User != nil) && base.Path == "":
		t.Path = "/" + ref.Path
		t.RawPath = "/" + refPath
		t = CleanPath(t)
	default:
		basePath := base.EscapedPath()
		i := strings.LastIndexByte(basePath, '/') + 1
		setRawPath(t, basePath[:i]+refPath)
		t = CleanPath(t)
	}
	if refPath != "" || ref.ForceQuery || ref.RawQuery != "" {
		t.RawQuery = ref.RawQuery
		t.ForceQuery = ref.ForceQuery
	} else {
		t.RawQuery = base.RawQuery
		t.ForceQuery = base.ForceQuery
	}
	return t
}

// CleanPath returns the shortest [*url.URL] equivalent to u purely by lexical processing.
// It applies the following rules iteratively until no further processing can be done:
//
//  1. Eliminate each "." path name element (the current directory).
//  2. Eliminate each inner ".." path name element (the parent directory)
//     along with the non-".." element that precedes it.
//  3. Eliminate ".." elements that begin a rooted path:
//     that is, replace "/.." by "/" at the beginning of a path.
//
// If the result of this process is a URL with an empty path and the path was not originally empty,
// then CleanPath returns a URL whose Path == ".".
// If the result of this process is a URL with an empty path and a non-empty scheme, host, or user,
// then CleanPath returns a URL whose Path == "/".
func CleanPath(u *url.URL) *url.URL {
	if u.Opaque != "" {
		return u
	}

	path := u.EscapedPath()
	rooted := u.Scheme != "" || u.Host != "" || u.User != nil || (len(path) > 0 && path[0] == '/')
	out := lazybuf{s: path}
	r, dotdot := 0, 0
	if rooted {
		out.append('/')
		dotdot = 1
		if len(path) > 0 && path[0] == '/' {
			r = 1
		}
	}
	for r < len(path) {
		switch {
		case path[r] == '.' && r+1 == len(path):
			// . at end
			r++
		case path[r] == '.' && r+1 < len(path) && path[r+1] == '/':
			// ./ element
			r += 2
			if !rooted && out.w == 0 && r < len(path) && path[r] == '/' {
				out.append('.')
				out.append('/')
			}
		case path[r] == '.' && path[r+1] == '.' && (r+2 == len(path) || path[r+2] == '/'):
			// .. element: remove to last /
			r += 2
			if r < len(path) {
				// Consume slash.
				r++
			}

			switch {
			case out.w > dotdot:
				// can backtrack
				out.w--
				for out.w > dotdot && out.index(out.w-1) != '/' {
					out.w--
				}
			case !rooted:
				// cannot backtrack, but not rooted, so append .. element.
				out.append('.')
				out.append('.')
				out.append('/')
				dotdot = out.w
			}
		default:
			for r < len(path) {
				c := path[r]
				out.append(c)
				r++
				if c == '/' {
					break
				}
			}
		}
	}

	if out.w == 0 && u.Path != "" {
		u = new(*u)
		if err := setRawPath(u, "."); err != nil {
			panic(err)
		}
		return u
	}
	newPath := out.string()
	if newPath == "../" || strings.HasSuffix(newPath, "/../") {
		newPath = newPath[:len(newPath)-1]
	}
	u2 := new(*u)
	if err := setRawPath(u2, newPath); err != nil {
		panic(err)
	}
	if u.Path == u2.Path && u.RawPath == u2.RawPath {
		return u
	}
	return u2
}

// Rel returns a [*url.URL] relative to baseURL that is equivalent to targetURL.
// If targetURL is absolute, Rel will not return an error.
func Rel(baseURL, targetURL *url.URL) (*url.URL, error) {
	if baseURL.Scheme != targetURL.Scheme ||
		baseURL.Opaque != "" ||
		targetURL.Opaque != "" ||
		baseURL.Host != targetURL.Host ||
		baseURL.User.String() != targetURL.User.String() {
		if !targetURL.IsAbs() {
			return targetURL, fmt.Errorf("cannot make %s relative to %s", targetURL.Redacted(), baseURL.Redacted())
		}
		return targetURL, nil
	}

	basePath := CleanPath(baseURL).EscapedPath()
	targetPath := CleanPath(targetURL).EscapedPath()
	if basePath == targetPath {
		ref := &url.URL{
			RawQuery:    targetURL.RawQuery,
			ForceQuery:  targetURL.ForceQuery && targetURL.RawQuery == "",
			Fragment:    targetURL.Fragment,
			RawFragment: targetURL.RawFragment,
		}
		if !ref.ForceQuery && ref.RawQuery == "" && baseURL.RawQuery != "" ||
			baseURL.Fragment != "" && ref.Fragment == "" {
			var refPath string
			if i := strings.LastIndexByte(targetPath, '/'); i == len(targetPath)-1 {
				refPath = "."
			} else {
				// If no slashes, i == -1 and we want to include the entire string in that case.
				refPath = targetPath[i+1:]
			}
			if err := setRawPath(ref, refPath); err != nil {
				return targetURL, fmt.Errorf("cannot make %s relative to %s: %v", targetURL.Redacted(), baseURL.Redacted(), err)
			}
		}
		return ref, nil
	}
	if isAbs(basePath) != isAbs(targetPath) {
		// If one of the URLs is an absolute-path reference
		// and the other is a relative-path reference,
		// then impossible to make relative.
		return targetURL, fmt.Errorf("cannot make %s relative to %s", targetURL.Redacted(), baseURL.Redacted())
	}

	// Position basePath[b0:bi] and targetPath[t0:ti] at the first differing elements.
	bl := len(basePath)
	tl := len(targetPath)
	var b0, bi, t0, ti int
	for {
		for bi < bl {
			c := basePath[bi]
			bi++
			if c == '/' {
				break
			}
		}
		for ti < tl {
			c := targetPath[ti]
			ti++
			if c == '/' {
				break
			}
		}
		if targetPath[t0:ti] != basePath[b0:bi] {
			break
		}
		b0 = bi
		t0 = ti
	}

	if basePath[b0:bi] == ".." {
		return targetURL, fmt.Errorf("cannot make %s relative to %s", targetURL.Redacted(), baseURL.Redacted())
	}

	var newPath string
	if b0 == bl {
		newPath = targetPath[t0:]
	} else {
		// Base elements left. Must go up before going down.
		seps := strings.Count(basePath[b0:bl], "/")
		size := seps * len("../")
		size += tl - t0

		buf := new(strings.Builder)
		buf.Grow(size)
		for range seps {
			buf.WriteString("../")
		}
		if t0 != tl {
			buf.WriteString(targetPath[t0:])
		}
		newPath = buf.String()
		if newPath == "" {
			newPath = "."
		}
	}
	if strings.HasPrefix(newPath, "/") {
		newPath = "./" + newPath
	}

	ref := &url.URL{
		RawQuery:    targetURL.RawQuery,
		ForceQuery:  targetURL.ForceQuery && targetURL.RawQuery == "",
		Fragment:    targetURL.Fragment,
		RawFragment: targetURL.RawFragment,
	}
	if err := setRawPath(ref, newPath); err != nil {
		return targetURL, fmt.Errorf("cannot make %s relative to %s: %v", targetURL.Redacted(), baseURL.Redacted(), err)
	}
	return ref, nil
}

func isAbs(path string) bool {
	return strings.HasPrefix(path, "/")
}

func setRawPath(u *url.URL, rawPath string) error {
	var err error
	u.Path, err = url.PathUnescape(rawPath)
	if err != nil {
		return err
	}
	if defaultEncoding := url.PathEscape(u.Path); rawPath == defaultEncoding {
		u.RawPath = ""
	} else {
		u.RawPath = rawPath
	}
	return nil
}

// A lazybuf is a lazily constructed path buffer.
// It supports append, reading previously appended bytes,
// and retrieving the final string. It does not allocate a buffer
// to hold the output until that output diverges from s.
type lazybuf struct {
	s   string
	buf []byte
	w   int
}

func (b *lazybuf) index(i int) byte {
	if b.buf != nil {
		return b.buf[i]
	}
	return b.s[i]
}

func (b *lazybuf) append(c byte) {
	if b.buf == nil {
		if b.w < len(b.s) && b.s[b.w] == c {
			b.w++
			return
		}
		b.buf = make([]byte, len(b.s)+1)
		copy(b.buf, b.s[:b.w])
	}
	b.buf[b.w] = c
	b.w++
}

func (b *lazybuf) string() string {
	if b.buf == nil {
		return b.s[:b.w]
	}
	return string(b.buf[:b.w])
}
