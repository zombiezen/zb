// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
	"zb.256lights.llc/pkg/zbstore"
)

func TestURLs(t *testing.T) {
	dir := filepath.Join("testdata", "TestURLs")
	listing, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range listing {
		fileName := entry.Name()
		if strings.HasPrefix(fileName, ".") {
			continue
		}
		testName, isTXTAR := strings.CutSuffix(fileName, ".txt")
		if !isTXTAR {
			continue
		}
		fileName = filepath.Join(dir, fileName)

		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			ctx := testcontext.New(t)
			storeDir := backendtest.NewStoreDirectory(t)
			archive, err := txtar.ParseFile(fileName)
			if err != nil {
				t.Fatal(err)
			}

			lines := strings.Split(string(archive.Comment), "\n")
			lines = slices.DeleteFunc(lines, func(line string) bool {
				return strings.TrimSpace(line) == ""
			})
			if len(lines) == 0 {
				t.Fatal("missing zb eval line")
			}
			evalArgv := strings.Fields(lines[0])
			if len(evalArgv) < 2 || evalArgv[0] != "zb" || evalArgv[1] != "eval" {
				t.Fatalf("first line of test = %s; must start with zb eval", lines[0])
			}
			urls := evalArgv[2:]
			sys := system.Current()
			if len(urls) > 0 {
				sysString, isSystemFlag := strings.CutPrefix(urls[0], "--system=")
				if isSystemFlag {
					var err error
					sys, err = system.Parse(sysString)
					if err != nil {
						t.Fatal(err)
					}
					urls = urls[1:]
				}
			}

			objects, storePaths, err := storetest.TxtarObjects(storeDir, archive.Files)
			if err != nil {
				t.Fatal(err)
			}
			exportBuffer := new(bytes.Buffer)
			exportWriter := zbstore.NewExportWriter(exportBuffer)
			for i, arg := range urls {
				u, err := ParseURL(arg)
				if err != nil {
					t.Fatal(err)
				}
				objectBase, subpath, _ := strings.Cut(u.Path, "/")
				objectIndex := slices.IndexFunc(objects, func(obj *zbstore.Blob) bool {
					return obj.StorePath == storePaths[objectBase]
				})
				if objectIndex == -1 {
					t.Fatalf("unknown object %s in zb eval arguments", objectBase)
				}

				urlstr := string(objects[objectIndex].StorePath)
				if subpath != "" {
					urlstr += "/" + subpath
				}
				if u.Fragment != "" {
					urlstr += "#" + u.Fragment
				}
				urls[i] = urlstr
				if err := exportWriter.WriteObject(ctx, objects[objectIndex]); err != nil {
					t.Fatal(err)
				}
			}
			if err := exportWriter.Close(); err != nil {
				t.Fatal(err)
			}
			replacements := make([]string, 0, len(storePaths)*2)
			for originalName, path := range storePaths {
				replacements = append(replacements, originalName, string(path))
			}
			replacer := strings.NewReplacer(replacements...)

			_, store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
				TempDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			evalStore := &testRPCStore{Client: store}
			if err := evalStore.StoreImport(ctx, exportBuffer); err != nil {
				t.Fatal(err)
			}
			eval, err := NewEval(&Options{
				Store:          evalStore,
				StoreDirectory: storeDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := eval.Close(); err != nil {
					t.Error("eval.Close:", err)
				}
			}()

			outputMap, err := eval.URLs(ctx, urls, sys)
			if err != nil {
				t.Fatal(err)
			}

			i := 1
			for key, out := range outputMap.All() {
				fullKey := "result"
				if key != "" {
					fullKey += "-" + key
				}
				if i >= len(lines) {
					t.Errorf("unexpected output %s %v", fullKey, out)
					continue
				}

				line := strings.TrimLeft(lines[i], " \t")
				wantKeyEnd := strings.IndexAny(line, " \t")
				if i == -1 {
					t.Errorf("invalid line %s", lines[i])
				} else {
					wantKey := line[:wantKeyEnd]
					wantOutput := replacer.Replace(strings.Trim(line[wantKeyEnd:], " \t\r"))
					failed := false
					if strings.HasPrefix(wantOutput, "^") {
						if re, err := regexp.Compile("(?m)" + wantOutput); err != nil {
							t.Errorf("invalid pattern for %s: %v", wantKey, err)
						} else if fullKey != wantKey || !re.MatchString(out.String()) {
							t.Errorf("%s %v does not match %s %s", fullKey, out, wantKey, wantOutput)
							failed = true
						}
					} else if fullKey != wantKey || out.String() != wantOutput {
						t.Errorf("%s %v != %s %s", fullKey, out, wantKey, wantOutput)
						failed = true
					}
					if failed {
						for ref := range out.OutputReferences() {
							got, err := os.ReadFile(string(ref.DrvPath))
							if err == nil {
								t.Errorf("%s = %s", ref.DrvPath, got)
							}
						}
					}
				}
				i++
			}
			if i < len(lines) {
				t.Errorf("missing outputs:\n%s", strings.Join(lines[i:], "\n"))
			}
		})
	}
}

func TestParseFragment(t *testing.T) {
	tests := []struct {
		s           string
		archivePath string
		keyPath     string
		err         bool
	}{
		{
			s:           "",
			archivePath: "",
			keyPath:     "",
		},
		{
			s:           "foo",
			archivePath: "",
			keyPath:     "foo",
		},
		{
			s:           "1.2.3",
			archivePath: "",
			keyPath:     "1.2.3",
		},
		{
			s:           "/",
			archivePath: "",
			keyPath:     "/",
			err:         true,
		},
		{
			s:           "foo/bar",
			archivePath: "",
			keyPath:     "foo/bar",
		},
		{
			s:           "foo//bar",
			archivePath: "",
			keyPath:     "foo//bar",
		},
		{
			s:           "/foo/bar",
			archivePath: "",
			keyPath:     "/foo/bar",
			err:         true,
		},
		{
			s:           "foo/bar/",
			archivePath: "",
			keyPath:     "foo/bar/",
			err:         true,
		},
		{
			s:           "foo.lua:bar",
			archivePath: "foo.lua",
			keyPath:     "bar",
		},
		{
			s:           "foo.lua:",
			archivePath: "foo.lua",
			keyPath:     "",
		},
		{
			s:           "foo/bar.lua:baz",
			archivePath: "foo/bar.lua",
			keyPath:     "baz",
		},
		{
			s:           "foo/bar.lua:baz",
			archivePath: "foo/bar.lua",
			keyPath:     "baz",
		},
		{
			s:           "foo/bar:baz.lua:quux",
			archivePath: "foo/bar:baz.lua",
			keyPath:     "quux",
		},
	}

	for _, test := range tests {
		archivePath, keyPath, err := parseFragment(test.s)
		if archivePath != test.archivePath || keyPath != test.keyPath || (err != nil) != test.err {
			errString := "<nil>"
			if test.err {
				errString = "<error>"
			}
			t.Errorf("parseFragment(%q) = %q, %q, %v; want %q, %q, %s",
				test.s, archivePath, keyPath, err, test.archivePath, test.keyPath, errString)
		}
	}
}

func TestSplitKeyPath(t *testing.T) {
	tests := []struct {
		s    string
		want []string
	}{
		{"", []string{}},
		{"/", []string{"", ""}},
		{"foo", []string{"foo"}},
		{"foo.bar", []string{"foo.bar"}},
		{"foo/bar", []string{"foo", "bar"}},
		{"foo//bar", []string{"foo", "bar"}},
		{"/foo", []string{"", "foo"}},
		{"foo/", []string{"foo", ""}},
		{"/foo/", []string{"", "foo", ""}},
		{"//foo", []string{"", "foo"}},
		{"foo//", []string{"foo", ""}},
		{"//foo//", []string{"", "foo", ""}},
	}

	for _, test := range tests {
		got := slices.Collect(splitKeyPath(test.s))
		if !slices.Equal(test.want, got) {
			t.Errorf("slices.Collect(splitKeyPath(%q)) = %q; want %q", test.s, got, test.want)
		}
	}
}
