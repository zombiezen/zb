// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstorehttp

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
)

func TestNARInfoMarshalText(t *testing.T) {
	tests := []struct {
		name string
		info *NARInfo
		want string
		err  bool
	}{
		{
			name: "Empty",
			info: new(NARInfo),
			err:  true,
		},
		{
			name: "Hello",
			info: &NARInfo{
				StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
				Compression: XZ,
				FileHash:    mustParseHash(t, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
				FileSize:    50088,
				NARHash:     mustParseHash(t, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				NARSize:     226488,
				References: *sets.NewSorted[zbstore.Path](
					"/nix/store/3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8",
					"/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				),
				Deriver: "/nix/store/ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv",
				Sig:     []*nix.Signature{mustParseSignature(t, "cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==")},
			},
			want: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n" +
				"References: 3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8 s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"Deriver: ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv\n" +
				"Sig: cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==\n",
		},
		{
			name: "Minimal",
			info: &NARInfo{
				StorePath: "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				URL:       "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
				FileHash:  mustParseHash(t, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
				FileSize:  50088,
				NARHash:   mustParseHash(t, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				NARSize:   226488,
			},
			want: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: bzip2\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.info.MarshalText()
			if test.err {
				if len(got) > 0 || err == nil {
					t.Errorf("MarshalText() = %q, %v; want \"\", <error>", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal("MarshalText():", err)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
}

type narInfoUnmarshalTest struct {
	name string
	data string
	want *NARInfo
	err  bool
}

func makeNARInfoUnmarshalTests(tb testing.TB) []narInfoUnmarshalTest {
	return []narInfoUnmarshalTest{
		{
			name: "Empty",
			data: "",
			err:  true,
		},
		{
			name: "Hello",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n" +
				"References: 3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8 s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"Deriver: ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv\n" +
				"Sig: cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==\n",
			want: &NARInfo{
				StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
				Compression: XZ,
				FileHash:    mustParseHash(tb, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
				FileSize:    50088,
				NARHash:     mustParseHash(tb, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				NARSize:     226488,
				References: *sets.NewSorted[zbstore.Path](
					"/nix/store/3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8",
					"/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				),
				Deriver: "/nix/store/ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv",
				Sig:     []*nix.Signature{mustParseSignature(tb, "cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==")},
			},
		},
		{
			name: "MinimalCompression",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n",
			want: &NARInfo{
				StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
				Compression: Bzip2,
				FileHash:    mustParseHash(tb, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
				FileSize:    50088,
				NARHash:     mustParseHash(tb, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				NARSize:     226488,
			},
		},
		{
			name: "MinimalNoCompression",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: none\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n",
			want: &NARInfo{
				StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
				URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
				Compression: NoCompression,
				FileHash:    mustParseHash(tb, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				FileSize:    226488,
				NARHash:     mustParseHash(tb, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
				NARSize:     226488,
			},
		},
		{
			name: "NegativeNARSize",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: -226488\n",
			err: true,
		},
		{
			name: "ZeroNARSize",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: 50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 0\n",
			err: true,
		},
		{
			name: "NegativeFileSize",
			data: "StorePath: /nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1\n" +
				"URL: nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz\n" +
				"Compression: xz\n" +
				"FileHash: sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq\n" +
				"FileSize: -50088\n" +
				"NarHash: sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80\n" +
				"NarSize: 226488\n",
			err: true,
		},
	}
}

func TestNARInfoUnmarshalText(t *testing.T) {
	for _, test := range makeNARInfoUnmarshalTests(t) {
		t.Run(test.name, func(t *testing.T) {
			got := new(NARInfo)
			err := got.UnmarshalText([]byte(test.data))
			if test.err {
				if err == nil {
					t.Error("UnmarshalText(...) = <nil>; want error")
				}
				return
			}
			if err != nil {
				t.Fatal("UnmarshalText(...):", err)
			}
			if diff := cmp.Diff(test.want, got, storetest.TransformSortedSet[zbstore.Path](), cmp.Comparer(compareSignatures)); diff != "" {
				t.Errorf("after re-marshaling (-want +got):\n%s", diff)
			}
		})
	}
}

func FuzzNARInfo(f *testing.F) {
	for _, test := range makeNARInfoUnmarshalTests(f) {
		f.Add([]byte(test.data))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		info := new(NARInfo)
		if err := info.UnmarshalText(data); err != nil {
			// Many inputs will be invalid during fuzzing. That's fine.
			return
		}

		// So we have a valid input. We should be able to round-trip.
		intermediate, err := info.MarshalText()
		if err != nil {
			t.Fatal("Could not re-marshal unmarshaled input:", err)
		}
		got := new(NARInfo)
		if err := got.UnmarshalText(intermediate); err != nil {
			t.Logf("Remarshaled text:\n%s", intermediate)
			t.Fatal("Could not unmarshal re-marshaled input:", err)
		}
		opts := cmp.Options{
			cmp.Comparer(compareSignatures),
			cmp.Transformer("String", func(h nix.Hash) string {
				return h.String()
			}),
			storetest.TransformSortedSet[zbstore.Path](),
		}
		if diff := cmp.Diff(info, got, opts); diff != "" {
			t.Errorf("after re-marshaling (-want +got):\n%s", diff)
		}
	})
}

func TestNARInfoClone(t *testing.T) {
	want := &NARInfo{
		StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
		URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
		Compression: XZ,
		FileHash:    mustParseHash(t, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
		FileSize:    50088,
		NARHash:     mustParseHash(t, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
		NARSize:     226488,
		References: *sets.NewSorted[zbstore.Path](
			"/nix/store/3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8",
			"/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
		),
		Deriver: "/nix/store/ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv",
		Sig:     []*nix.Signature{mustParseSignature(t, "cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==")},
	}

	// Copy+pasting the fields in code to protect against Clone() modifying the fields
	// or having a buggy oracle.
	original := &NARInfo{
		StorePath:   "/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
		URL:         "nar/1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq.nar.xz",
		Compression: XZ,
		FileHash:    mustParseHash(t, "sha256:1nhgq6wcggx0plpy4991h3ginj6hipsdslv4fd4zml1n707j26yq"),
		FileSize:    50088,
		NARHash:     mustParseHash(t, "sha256:0yzhigwjl6bws649vcs2asa4lbs8hg93hyix187gc7s7a74w5h80"),
		NARSize:     226488,
		References: *sets.NewSorted[zbstore.Path](
			"/nix/store/3n58xw4373jp0ljirf06d8077j15pc4j-glibc-2.37-8",
			"/nix/store/s66mzxpvicwk07gjbjfw9izjfa797vsw-hello-2.12.1",
		),
		Deriver: "/nix/store/ib3sh3pcz10wsmavxvkdbayhqivbghlq-hello-2.12.1.drv",
		Sig:     []*nix.Signature{mustParseSignature(t, "cache.nixos.org-1:8ijECciSFzWHwwGVOIVYdp2fOIOJAfmzGHPQVwpktfTQJF6kMPPDre7UtFw3o+VqenC5P8RikKOAAfN7CvPEAg==")},
	}
	sig0 := &original.Sig[0]
	got := original.Clone()

	if original == got {
		t.Fatal("original.Clone() == original")
	}
	opts := cmp.Options{
		cmp.Comparer(compareSignatures),
		storetest.TransformSortedSet[zbstore.Path](),
		cmpopts.EquateEmpty(),
	}
	if diff := cmp.Diff(want, original, opts); diff != "" {
		t.Errorf("original -want +got:\n%s", diff)
	} else {
		if sig0 != &original.Sig[0] {
			t.Error("original.Sig modified")
		}
	}
	if diff := cmp.Diff(want, got, opts); diff != "" {
		t.Errorf("original.Clone() -want +got:\n%s", diff)
	} else {
		if &original.Sig[0] == &got.Sig[0] {
			t.Error("Same backing array used for Sig")
		}
	}
}

func compareSignatures(a, b *nix.Signature) bool {
	return (a == nil) == (b == nil) && (a == nil || a.String() == b.String())
}

func mustParseHash(tb testing.TB, s string) nix.Hash {
	tb.Helper()
	h, err := nix.ParseHash(s)
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

func mustParseSignature(tb testing.TB, s string) *nix.Signature {
	tb.Helper()
	sig, err := nix.ParseSignature(s)
	if err != nil {
		tb.Fatal(err)
	}
	return sig
}
