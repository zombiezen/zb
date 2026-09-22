// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

// Package deque provides a double-ended queue type.
package deque

import (
	"iter"
	"slices"
)

// A Deque is a queue with two ends: the front and the back.
// The zero value is an empty deque.
type Deque[T any] struct {
	slice []T
	start int
	n     int
}

// Collect collects values from seq into a new deque and returns it.
func Collect[T any](seq iter.Seq[T]) *Deque[T] {
	d := new(Deque[T])
	for x := range seq {
		d.PushBack(x)
	}
	return d
}

// Len returns the number of elements in the deque.
func (d *Deque[T]) Len() int {
	if d == nil {
		return 0
	}
	return d.n
}

// Cap returns the maximum number of elements the deque can hold
// before allocating more memory.
func (d *Deque[T]) Cap() int {
	if d == nil {
		return 0
	}
	return len(d.slice)
}

func (d *Deque[T]) index(i int) int {
	i += d.start
	if i >= len(d.slice) {
		i -= len(d.slice)
	}
	return i
}

func (d *Deque[T]) logicalSlice(i, j int) ([]T, []T) {
	if i == j {
		i = d.index(i)
		return d.slice[i:i], nil
	}
	i = d.index(i)
	j = d.index(j)
	if i >= j {
		return d.slice[i:], d.slice[:j]
	}
	return d.slice[i:j], nil
}

// All returns an indexed iterator over the values in front-to-back order.
func (d *Deque[T]) All() iter.Seq2[int, T] {
	if d == nil {
		return func(yield func(int, T) bool) {}
	}
	return func(yield func(int, T) bool) {
		for i := 0; i < d.n; i++ {
			if !yield(i, d.slice[d.index(i)]) {
				return
			}
		}
	}
}

// Values returns an iterator over the values in front-to-back order.
func (d *Deque[T]) Values() iter.Seq[T] {
	if d == nil {
		return func(yield func(T) bool) {}
	}
	return func(yield func(T) bool) {
		for i := 0; i < d.n; i++ {
			if !yield(d.slice[d.index(i)]) {
				return
			}
		}
	}
}

// Front returns the element at the front of the deque.
// ok is true if and only if the deque is non-empty.
func (d *Deque[T]) Front() (_ T, ok bool) {
	if d == nil || d.n == 0 {
		var zero T
		return zero, false
	}
	return d.slice[d.start], true
}

// PushFront inserts the given elements at the front of the deque.
// Popping len(elems) from the front after PushFront
// will yield elems in the same order.
func (d *Deque[T]) PushFront(elems ...T) {
	d.Grow(len(elems))

	if d.n == 0 {
		// Special case: place first insert at beginning of slice.
		d.start = 0
		d.n = len(elems)
		copy(d.slice, elems)
		return
	}

	for i := range elems {
		d.start--
		if d.start < 0 {
			d.start += len(d.slice)
		}
		d.n++
		d.slice[d.start] = elems[len(elems)-i-1]
	}
}

// PopFront pops n elements from the front of the deque.
// PopFront panics if n > d.Len() or n < 0.
func (d *Deque[T]) PopFront(n int) {
	switch {
	case n == 0:
		return
	case n > d.Len():
		panic("deque underflow")
	case n < 0:
		panic("negative value to PopFront")
	}

	s1, s2 := d.logicalSlice(0, n)
	clear(s1)
	clear(s2)
	d.n -= n
	if d.n == 0 {
		d.start = 0
	} else {
		d.start = d.index(n)
	}
}

// Back returns the element at the back of the deque.
// ok is true if and only if the deque is non-empty.
func (d *Deque[T]) Back() (_ T, ok bool) {
	if d == nil || d.n == 0 {
		var zero T
		return zero, false
	}
	return d.slice[d.index(d.n-1)], true
}

// PushBack appends the given elements to the back of the deque.
// Popping len(elems) from the back after PushBack
// will yield elems in reverse order.
func (d *Deque[T]) PushBack(elems ...T) {
	d.Grow(len(elems))
	for _, x := range elems {
		d.slice[d.index(d.n)] = x
		d.n++
	}
}

// PopBack pops n elements from the back of the deque.
// PopBack panics if n > d.Len() or n < 0.
func (d *Deque[T]) PopBack(n int) {
	switch {
	case n == 0:
		return
	case n > d.Len():
		panic("deque underflow")
	case n < 0:
		panic("negative value to PopBack")
	}

	s1, s2 := d.logicalSlice(d.n-n, d.n)
	clear(s1)
	clear(s2)
	d.n -= n
	if d.n == 0 {
		d.start = 0
	}
}

// Grow increases the deque's capacity, if necessary,
// to guarantee space for another n elements.
// After Grow(n), at least n elements can be appended to the deque without another allocation.
// If n is negative or too large to allocate the memory, Grow panics.
func (d *Deque[T]) Grow(n int) {
	if n < 0 {
		panic("negative value to Grow")
	}
	if d.n+n <= len(d.slice) {
		return
	}
	s1, s2 := d.logicalSlice(0, d.n)
	d.slice = slices.Grow(append(slices.Clip(s1), s2...), n)
	d.start = 0
	// Always make len(d.slice) == cap(d.slice).
	// slices.Grow may add more capacity than requested.
	d.slice = d.slice[:cap(d.slice)]
}
