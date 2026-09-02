// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package xurl

import (
	"net/url"
	"strings"
	"testing"
)

func TestResolveReference(t *testing.T) {
	tests := []struct {
		base string
		ref  string
		want string
	}{
		{"", "", ""},
		{".", "", "."},
		{"", ".", "."},
		{".", ".", "."},
		{"./", ".", "."},
		{"a", "x/y", "x/y"},
		{"a/b", "x/y", "a/x/y"},
		{"a/b/", "x/y", "a/b/x/y"},
		{"a/b", "/x/y", "/x/y"},
		{".", "http://www.example.com", "http://www.example.com/"},
		{"http://www.example.com/a/b", "x/y", "http://www.example.com/a/x/y"},
		{"http://www.example.com", "x/y", "http://www.example.com/x/y"},
		{"http://www.example.com/a/b", "//other.example.com", "http://other.example.com/"},
		{"a/b?q=foo", "#id", "a/b?q=foo#id"},
		{"a/b?q=foo", "?#id", "a/b?#id"},
		{"a/b?q=foo", "?w=42#id", "a/b?w=42#id"},
		{"a/b?q=foo", "x#id", "a/x#id"},
		{"/a//..", "../", "/"},
		{"/a/b/..", "../", "/"},
	}

	for _, test := range tests {
		base, err := url.Parse(test.base)
		if err != nil {
			t.Error(err)
			continue
		}
		ref, err := url.Parse(test.ref)
		if err != nil {
			t.Error(err)
			continue
		}
		got := ResolveReference(base, ref).String()
		if got != test.want {
			t.Errorf("ResolveReference(%s, %s) = %s; want %s", test.base, test.ref, got, test.want)
		}
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		urlstr string
		want   string
	}{
		{"", ""},
		{".", "."},
		{"./", "."},
		{".//", ".//"},
		{".///", ".///"},
		{"./.", "."},
		{"..", ".."},
		{"../", ".."},
		{"..//", "..//"},
		{"../..", "../.."},
		{"../../", "../.."},
		{"../../././.", "../.."},
		{"../.././././", "../.."},
		{".././..", "../.."},
		{"foo.html", "foo.html"},
		{"foo/..", "."},
		{"../foo.html", "../foo.html"},
		{".././foo.html", "../foo.html"},
		{"./foo.html", "foo.html"},
		{"./this:that", "./this:that"},
		{"mid/content=5/../6", "mid/6"},
		{"/foo.html", "/foo.html"},
		{"/foo/bar/baz.html", "/foo/bar/baz.html"},
		{"/foo/bar/../baz.html", "/foo/baz.html"},
		{"/foo/bar/../baz.html", "/foo/baz.html"},
		{"/foo/..", "/"},
		{"/a/b/c/./../../g", "/a/g"},
		{"/", "/"},
		{"/..//", "//"},
		{"a:/..//", "a://"},
		{"a://foo/..//", "a://foo//"},
		{"a:/0//..", "a:/0/"},
		{"a:/0//../..", "a:/"},
		{"//foo", "//foo/"},
		{"//foo/../bar.txt", "//foo/bar.txt"},
		{"http://www.example.com", "http://www.example.com/"},
		{"http://www.example.com/", "http://www.example.com/"},
		{"http://www.example.com//", "http://www.example.com//"},
		{"http://www.example.com///", "http://www.example.com///"},
		{"http://www.example.com/foo.html", "http://www.example.com/foo.html"},
		{"http://www.example.com/foo/..", "http://www.example.com/"},
		{"http://www.example.com//foo.html", "http://www.example.com//foo.html"},
		{"http://www.example.com//./foo.html", "http://www.example.com//foo.html"},
		{"http://www.example.com//../foo.html", "http://www.example.com/foo.html"},
		{"http://www.example.com/./foo.html", "http://www.example.com/foo.html"},
		{"http://www.example.com/../foo.html", "http://www.example.com/foo.html"},
		{"http://www.example.com/../foo.html", "http://www.example.com/foo.html"},
		{"?spam=eggs", "?spam=eggs"},
		{"foo/..?spam=eggs", ".?spam=eggs"},
		{"#", ""},
		{"#foo", "#foo"},
		{"foo/..#foo", ".#foo"},
	}

	for _, test := range tests {
		u, err := url.Parse(test.urlstr)
		if err != nil {
			t.Error(err)
			continue
		}
		got := CleanPath(u).String()
		if got != test.want {
			t.Errorf("CleanPath(%s) = %s; want %s", test.urlstr, got, test.want)
		}
	}
}

func FuzzRelURL(f *testing.F) {
	f.Add("/foo", "")
	f.Add("foo", "")
	f.Add("/foo", "foo")
	f.Add("/foo", "/bar")
	f.Add("/foo/", "/bar")
	f.Add("/bar", "/foo/")
	f.Add("/foo/../bar", "/baz")
	f.Add("/foo/%2e%2e/bar", "/baz")
	f.Add("http://www.example.com/", "http://www.example.com/")
	f.Add("http://www.example.com", "http://www.example.com/")
	f.Add("http://www.example.com/", "http://www.example.com")
	f.Add("http://www.example.com/", "http://www.example.com/foo/bar")
	f.Add("http://www.example.com/foo/bar", "http://www.example.com/")
	f.Add("http://www.example.com/foo", "http://www.example.com/foo/")
	f.Add("http://www.example.com/foo/", "http://www.example.com/foo")
	f.Add("http://www.example.com/foo/bar", "http://www.example.com/foo/quux")
	f.Add("http://www.example.com/foo/%2e%2e/bar", "http://www.example.com/bar")
	f.Add("http://www.example.com/foo/bar/", "http://www.example.com/foo/quux")
	f.Add("http://www.example.com/foo/bar/", "http://www.example.com/foo/../quux")
	f.Add("http://www.example.com/foo/bar/", "http://www.example.com/foo/bar")
	f.Add("http://www.example.com/foo", "http://www.example.com/foo?spam=eggs")
	f.Add("http://www.example.com/foo?spam=eggs", "http://www.example.com/foo")
	f.Add("http://www.example.com/foo", "http://www.example.com/foo#spam")
	f.Add("http://www.example.com/foo#spam", "http://www.example.com/foo")
	f.Add("http://www.example.com/foo#spam", "http://www.example.com/foo#eggs")
	f.Add("http://www.example.com/foo/#spam", "http://www.example.com/foo/")
	f.Add("http://www.example.com/foo/", "http://www.example.com/foo/#spam")
	f.Add("http://www.example.com/foo/#spam", "http://www.example.com/foo/#eggs")
	f.Add("http://www.example.com/foo", "ftp://www.example.com/foo")
	f.Add("http://www.example.com/foo", "http://www.example.com/foo?")
	f.Add("http://www.example.com/foo", "myuri:blabber")
	f.Add("http://www.example.com/foo", "foo/bar/baz")

	f.Fuzz(func(t *testing.T, baseURLString string, targetURLString string) {
		baseURL, baseError := url.Parse(baseURLString)
		if baseError != nil {
			t.Log(baseURL)
		}
		targetURL, targetError := url.Parse(targetURLString)
		if targetError != nil {
			t.Log(targetError)
		}
		if baseError != nil || targetError != nil {
			t.SkipNow()
		}
		if !targetURL.IsAbs() && (!isAbsolutePathReference(baseURL) || !isAbsolutePathReference(targetURL)) {
			// TODO(someday): Some relative-path references are possible, but not important for zb.
			t.Skipf("Impossible to make a reference to %q from %q", targetURLString, baseURLString)
		}

		ref, err := Rel(baseURL, targetURL)
		if err != nil {
			t.Fatalf("Rel(%v, %v) = _, %v", baseURL, targetURL, err)
		}
		if got := ResolveReference(baseURL, ref); !urlsEqual(got, targetURL) {
			t.Errorf("Rel(%v, %v) = %v (resolves to %v)", baseURL, targetURL, ref, got)
		}
	})
}

func isAbsolutePathReference(u *url.URL) bool {
	return u.Scheme == "" && u.Opaque == "" && u.Host == "" && u.User == nil && strings.HasPrefix(u.EscapedPath(), "/")
}

func urlsEqual(u1, u2 *url.URL) bool {
	path1 := CleanPath(u1).EscapedPath()
	path2 := CleanPath(u2).EscapedPath()
	return u1.Scheme == u2.Scheme &&
		u1.Opaque == u2.Opaque &&
		u1.Host == u2.Host &&
		userinfosEqual(u1.User, u2.User) &&
		path1 == path2 &&
		u1.RawQuery == u2.RawQuery &&
		(u1.ForceQuery == u2.ForceQuery || u1.RawQuery != "") &&
		u1.EscapedFragment() == u2.EscapedFragment()
}

func userinfosEqual(u1, u2 *url.Userinfo) bool {
	p1, hasp1 := u1.Password()
	p2, hasp2 := u2.Password()
	return u1.Username() == u2.Username() && p1 == p2 && hasp1 == hasp2
}
