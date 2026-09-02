// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

func TestDefaultGlobalConfig(t *testing.T) {
	t.Run("Unix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skipf("Not running on %s", runtime.GOOS)
		}
		got := defaultGlobalConfig(func(key string) (string, bool) {
			if key == "HOME" {
				return "/home/foo", true
			}
			return "", false
		})
		want := &globalConfig{
			Directory:   zbstore.DefaultUnixDirectory,
			StoreSocket: "/opt/zb/var/zb/server.sock",
			NetrcPath:   "/home/foo/.netrc",
			CacheDB:     "/home/foo/.cache/zb/cache.db",
			HTTPCacheDB: "/home/foo/.cache/zb/http-cache.db",
		}
		if diff := cmp.Diff(want, got, globalConfigCompareOptions); diff != "" {
			t.Errorf("defaultGlobalConfig(HOME=/home/foo) (-want +got):\n%s", diff)
		}
	})

	t.Run("Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skipf("Not running on %s", runtime.GOOS)
		}
		got := defaultGlobalConfig(func(key string) (string, bool) {
			switch strings.ToUpper(key) {
			case "USERPROFILE":
				return `C:\Users\foo`, true
			case "APPDATA":
				return `C:\Users\foo\AppData\Roaming`, true
			case "LOCALAPPDATA":
				return `C:\Users\foo\AppData\Local`, true
			default:
				return "", false
			}
		})
		want := &globalConfig{
			Directory:   zbstore.DefaultWindowsDirectory,
			StoreSocket: `C:\zb\var\zb\server.sock`,
			NetrcPath:   `C:\Users\foo\.netrc`,
			CacheDB:     `C:\Users\foo\AppData\Local\zb\cache.db`,
			HTTPCacheDB: `C:\Users\foo\AppData\Local\zb\http-cache.db`,
		}
		if diff := cmp.Diff(want, got, globalConfigCompareOptions); diff != "" {
			t.Errorf("defaultGlobalConfig(...) (-want +got):\n%s", diff)
		}
	})
}

func TestGlobalConfigMergeFiles(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  func(dir string) *globalConfig
	}{
		{
			name: "MergeScalar",
			files: []string{
				`{"debug": true, "storeDirectory": "/foo"}` + "\n",
				`{"storeDirectory": "/bar"}` + "\n",
			},
			want: func(dir string) *globalConfig {
				return &globalConfig{
					Debug:     true,
					Directory: "/bar",
				}
			},
		},
		{
			name: "DontMergeAllowEnvironment",
			files: []string{
				`{"allowEnvironment": ["FOO"]}` + "\n",
				`{"allowEnvironment": ["BAR"]}` + "\n",
			},
			want: func(dir string) *globalConfig {
				return &globalConfig{
					AllowEnv: stringAllowList{
						set: sets.New("BAR"),
					},
				}
			},
		},
		{
			name: "BooleanClearsSet",
			files: []string{
				`{"allowEnvironment": ["FOO"]}` + "\n",
				`{"allowEnvironment": true}` + "\n",
			},
			want: func(dir string) *globalConfig {
				return &globalConfig{
					AllowEnv: stringAllowList{all: true},
				}
			},
		},
		{
			name: "MergePublicKeys",
			files: []string{
				`{"trustedPublicKeys": [{"format": "ed25519", "publicKey": "+NMDNfvjCmdT9mLr9zadYQXwF/mPLsToMw36yX7w6HCVCSK9J2WsMGPCAT9U2Y959NFgAfdiSWGRvWbXYlGUcA=="}]}` + "\n",
				`{"trustedPublicKeys": [{"format": "foo", "publicKey": "YmFy"}]}` + "\n",
			},
			want: func(dir string) *globalConfig {
				return &globalConfig{
					TrustedPublicKeys: []*zbstore.RealizationPublicKey{
						{
							Format: "ed25519",
							Data: []byte{
								0xf8, 0xd3, 0x03, 0x35, 0xfb, 0xe3, 0x0a, 0x67,
								0x53, 0xf6, 0x62, 0xeb, 0xf7, 0x36, 0x9d, 0x61,
								0x05, 0xf0, 0x17, 0xf9, 0x8f, 0x2e, 0xc4, 0xe8,
								0x33, 0x0d, 0xfa, 0xc9, 0x7e, 0xf0, 0xe8, 0x70,
								0x95, 0x09, 0x22, 0xbd, 0x27, 0x65, 0xac, 0x30,
								0x63, 0xc2, 0x01, 0x3f, 0x54, 0xd9, 0x8f, 0x79,
								0xf4, 0xd1, 0x60, 0x01, 0xf7, 0x62, 0x49, 0x61,
								0x91, 0xbd, 0x66, 0xd7, 0x62, 0x51, 0x94, 0x70,
							},
						},
						{
							Format: "foo",
							Data:   []byte{0x62, 0x61, 0x72},
						},
					},
				}
			},
		},
		{
			name: "MergeServerSigningKeyFiles",
			files: []string{
				`{"server": {"signingKeyFiles": ["foo.json", "bar.json"]}}` + "\n",
				`{"server": {"signingKeyFiles": ["baz.json"]}}` + "\n",
			},
			want: func(dir string) *globalConfig {
				return &globalConfig{
					Server: serverConfig{
						KeyFiles: []string{
							filepath.Join(dir, "foo.json"),
							filepath.Join(dir, "bar.json"),
							filepath.Join(dir, "baz.json"),
						},
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := make([]string, len(test.files))
			for i, content := range test.files {
				path := filepath.Join(dir, fmt.Sprintf("config%d.jwcc", i+1))
				if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
					t.Fatal(err)
				}
				paths[i] = path
			}

			got := new(globalConfig)
			err := got.mergeFiles(slices.Values(paths))
			if err != nil {
				t.Error("mergeFiles:", err)
			}
			if diff := cmp.Diff(test.want(dir), got, globalConfigCompareOptions); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
}

func FuzzConfigMarshal(f *testing.F) {
	f.Add([]byte(`{"debug": true, "storeDirectory": "/foo"}` + "\n"))
	f.Add([]byte(`{"storeDirectory": "/bar"}` + "\n"))
	f.Add([]byte(`{"storeSocket": "/var/foo.socket"}` + "\n"))
	f.Add([]byte(`{"cacheDB": "/var/cache.db"}` + "\n"))
	f.Add([]byte(`{"trustedPublicKeys": []}` + "\n"))
	f.Add([]byte(`{"trustedPublicKeys": [{"format": "ed25519", "publicKey": "+NMDNfvjCmdT9mLr9zadYQXwF/mPLsToMw36yX7w6HCVCSK9J2WsMGPCAT9U2Y959NFgAfdiSWGRvWbXYlGUcA=="}]}` + "\n"))
	f.Add([]byte(`{"trustedPublicKeys": [{"format": "foo", "publicKey": "YmFy"}]}`))
	f.Add([]byte(`{"netrcFile": "/etc/netrc"}` + "\n"))
	f.Add([]byte(`{"server": {"signingKeyFiles": ["secret-key.json"]}}` + "\n"))

	f.Fuzz(func(t *testing.T, in []byte) {
		init := defaultGlobalConfig(func(key string) (string, bool) {
			return "", false
		})
		if err := jsonv2.Unmarshal(in, &init); err != nil {
			t.Skip(err)
		}
		marshalled, err := jsonv2.Marshal(init)
		if err != nil {
			t.Fatal(err)
		}
		got := new(globalConfig)
		if err := jsonv2.Unmarshal(marshalled, got, jsonv2.RejectUnknownMembers(true)); err != nil {
			t.Error("Unmarshal:", err)
		}
		if diff := cmp.Diff(init, got, globalConfigCompareOptions); diff != "" {
			t.Errorf("Marshal result -want +got:\n%s", diff)
		}
	})
}

var globalConfigCompareOptions = cmp.Options{
	cmp.AllowUnexported(stringAllowList{}),
	cmpopts.EquateEmpty(),
}
