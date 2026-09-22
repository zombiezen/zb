// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package deque

import (
	"slices"
	"testing"
)

func TestDeque(t *testing.T) {
	check := func(t *testing.T, d *Deque[int], want []int) {
		t.Helper()
		if got, want := d.Len(), len(want); got != want {
			t.Errorf("new(Deque[int]).Len() = %d; want %d", got, want)
		}
		if got, want := d.Cap(), len(want); got < want {
			t.Errorf("new(Deque[int]).Cap() = %d; want >=%d", got, want)
		}
		if got := slices.Collect(d.Values()); !slices.Equal(got, want) {
			t.Errorf("slices.Collect(d.Values()) = %v; want %v", got, want)
		}
		{
			var got []int
			for i, x := range d.All() {
				if want := len(got); i != want {
					t.Errorf("d.All()[%d] i = %d", want, i)
				}
				got = append(got, x)
			}
			if !slices.Equal(got, want) {
				t.Errorf("d.All() = %v; want %v", got, want)
			}
		}

		if len(want) == 0 {
			if got, ok := d.Front(); got != 0 || ok {
				t.Errorf("new(Deque[int]).Front() = %d, %t; want 0, false", got, ok)
			}
			if got, ok := d.Back(); got != 0 || ok {
				t.Errorf("new(Deque[int]).Back() = %d, %t; want 0, false", got, ok)
			}
		} else {
			if got, ok := d.Front(); got != want[0] || !ok {
				t.Errorf("new(Deque[int]).Front() = %d, %t; want %d, true", got, ok, want[0])
			}
			if got, ok := d.Back(); got != want[len(want)-1] || !ok {
				t.Errorf("new(Deque[int]).Back() = %d, %t; want %d, true", got, ok, want[len(want)-1])
			}
		}
	}

	t.Run("Nil", func(t *testing.T) {
		check(t, nil, []int{})
	})
	t.Run("Empty", func(t *testing.T) {
		check(t, new(Deque[int]), []int{})
	})
	t.Run("PushFront1", func(t *testing.T) {
		d := new(Deque[int])
		d.PushFront(42)
		check(t, d, []int{42})
	})
	t.Run("PushFront3", func(t *testing.T) {
		d := new(Deque[int])
		d.PushFront(1, 2, 3)
		check(t, d, []int{1, 2, 3})
	})
	t.Run("PushBack1", func(t *testing.T) {
		d := new(Deque[int])
		d.PushBack(42)
		check(t, d, []int{42})
	})
	t.Run("PushBack3", func(t *testing.T) {
		d := new(Deque[int])
		d.PushBack(1, 2, 3)
		check(t, d, []int{1, 2, 3})
	})
	t.Run("PushFontAndBack", func(t *testing.T) {
		d := new(Deque[int])
		d.PushBack(2, 3)
		d.PushFront(1)
		d.PushBack(4)
		check(t, d, []int{1, 2, 3, 4})
	})
	t.Run("AtArrayEdge", func(t *testing.T) {
		d := new(Deque[int])
		d.PushFront(1, 2, 10, 10)
		d.PopFront(2)
		for end := 4; end < d.Cap(); end++ {
			d.PushBack(10)
			d.PopFront(10)
		}
		d.PushBack(20, 20)
		check(t, d, []int{10, 10, 20, 20})
	})
	t.Run("PopBack1", func(t *testing.T) {
		d := new(Deque[int])
		d.PushBack(1, 2, 3)
		d.PopBack(1)
		check(t, d, []int{1, 2})
	})
	t.Run("SplitAndGrow", func(t *testing.T) {
		d := new(Deque[int])
		d.PushFront(1, 2)
		d.Grow(8)
		newCap := d.Cap() - d.Len()
		for range newCap {
			d.PushBack(999)
		}
		d.PopFront(1)
		d.PushBack(3)
		d.PushBack(4)

		var want []int
		want = append(want, 2)
		for range newCap {
			want = append(want, 999)
		}
		want = append(want, 3, 4)
		check(t, d, want)
	})
	t.Run("PopFrontEndOfCap", func(t *testing.T) {
		d := new(Deque[int])
		d.Grow(5)
		n := d.Cap() - d.Len()
		for range n {
			d.PushBack(999)
		}
		d.PopFront(n - 1)
		d.PushBack(42)
		d.PopFront(1)
		check(t, d, []int{42})
	})
}
