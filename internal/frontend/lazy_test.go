// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"strings"
	"testing"

	"zb.256lights.llc/pkg/internal/backendtest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/testcontext"
)

func TestLazy(t *testing.T) {
	ctx := testcontext.New(t)
	storeDir := backendtest.NewStoreDirectory(t)

	store, err := backendtest.NewServer(ctx, t, storeDir, &backendtest.Options{
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	eval, err := NewEval(&Options{
		Store:          &testRPCStore{client: store},
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

	t.Run("Fibonacci", func(t *testing.T) {
		expr := `lazy(function(fib, i) if math.type(i) ~= "integer" or i < 3 then return nil end; return fib[i-2] + fib[i-1]; end, {0, 1})[10]`
		got, err := eval.Expression(ctx, expr, system.Current())
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if want := "34"; got.Default().String() != want {
			t.Errorf("%s = %q; want %q", expr, got, want)
		}
	})

	t.Run("Error", func(t *testing.T) {
		expr := `lazy(function() error("B".."O".."R".."K"); end)["foo"]`
		if _, err := eval.Expression(ctx, expr, system.Current()); err == nil {
			t.Errorf("%s did not raise error", expr)
		} else if got := err.Error(); !strings.Contains(got, "BORK") {
			t.Errorf("%s: %s; want to contain BORK", expr, got)
		} else {
			t.Logf("%s: %s", expr, got)
		}
	})
}
