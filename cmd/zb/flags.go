// Copyright 2025 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/csv"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"

	"github.com/alecthomas/kong"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
)

// stringAllowList is an allow list of a set of strings.
// If all is true, then it is the set of all strings.
type stringAllowList struct {
	set sets.Set[string]
	all bool
}

func (list *stringAllowList) Has(s string) bool {
	if list == nil {
		return false
	}
	return list.all || list.set.Has(s)
}

// MarshalJSONTo marshals a list if list.all is true,
// otherwise the array of strings in the set.
func (list *stringAllowList) MarshalJSONTo(enc *jsontext.Encoder) error {
	if list.all {
		return enc.WriteToken(jsontext.True)
	}
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}
	seq := list.set.All()
	if deterministic, _ := jsonv2.GetOption(enc.Options(), jsonv2.Deterministic); deterministic {
		seq = slices.Values(slices.Sorted(seq))
	}
	for s := range seq {
		if err := enc.WriteToken(jsontext.String(s)); err != nil {
			return err
		}
	}
	if err := enc.WriteToken(jsontext.EndArray); err != nil {
		return err
	}
	return nil
}

// UnmarshalJSONFrom unmarshals a boolean or an array of strings.
// Booleans are treated as setting the all flag.
func (list *stringAllowList) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch kind := dec.PeekKind(); kind {
	case 't':
		*list = stringAllowList{all: true}
		if _, err := dec.ReadToken(); err != nil {
			return fmt.Errorf("unmarshal allow list: %w", err)
		}
	case 'f':
		*list = stringAllowList{all: false}
		if _, err := dec.ReadToken(); err != nil {
			return fmt.Errorf("unmarshal allow list: %w", err)
		}
	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return fmt.Errorf("unmarshal allow list: %w", err)
		}
		if list.set == nil {
			list.set = make(sets.Set[string])
		} else {
			list.set.Clear()
		}

	listBody:
		for {
			tok, err := dec.ReadToken()
			if err != nil {
				return fmt.Errorf("unmarshal allow list: %w", err)
			}
			switch tok.Kind() {
			case '"':
				list.set.Add(tok.String())
			case ']':
				break listBody
			default:
				return fmt.Errorf("unmarshal allow list: only strings allowed in list")
			}
		}
	default:
		return fmt.Errorf("unmarshal allow list: %v not supported", kind)
	}
	return nil
}

func mapStringSet(dc *kong.DecodeContext, target reflect.Value) error {
	if tp := target.Type(); tp != reflect.TypeFor[sets.Set[string]]() {
		return fmt.Errorf("map string set: target is a %v", tp)
	}
	var s string
	if err := dc.Scan.PopValueInto("string", &s); err != nil {
		return err
	}
	var set sets.Set[string]
	if target.IsNil() {
		set = make(sets.Set[string])
		target.Set(reflect.ValueOf(set))
	} else {
		set = target.Interface().(sets.Set[string])
	}
	if dc.Value.Tag.Sep == -1 {
		set.Add(s)
	} else {
		r := csv.NewReader(strings.NewReader(s))
		r.Comma = dc.Value.Tag.Sep
		vals, err := r.Read()
		if err != nil {
			return err
		}
		set.AddSeq(slices.Values(vals))
	}
	return nil
}

func mapPathMap(dc *kong.DecodeContext, target reflect.Value) error {
	sep, _ := dc.Value.Tag.GetSep("sep", '=')

	tp := target.Type()
	if tp.Kind() != reflect.Map {
		return fmt.Errorf("%v is not a map", tp)
	}
	keyType := tp.Key()
	valueType := tp.Elem()
	if keyType.Kind() != reflect.String || valueType.Kind() != reflect.String {
		return fmt.Errorf("%v is not a map[~string]~string", tp)
	}

	var s string
	if err := dc.Scan.PopValueInto("string", &s); err != nil {
		return err
	}

	if target.IsNil() {
		target.Set(reflect.MakeMap(tp))
	}
	for word := range strings.FieldsSeq(s) {
		k, v, isMap := strings.Cut(word, string(sep))
		if !isMap {
			v = k
		}
		target.SetMapIndex(reflect.ValueOf(k).Convert(keyType), reflect.ValueOf(v).Convert(valueType))
	}
	return nil
}

func mapNativeStorePath(dc *kong.DecodeContext, workdir string, target reflect.Value) error {
	storePathType := reflect.TypeFor[zbstore.Path]()
	if tp := target.Type(); tp != storePathType {
		if tp.Kind() == reflect.Slice && tp.Elem() == storePathType {
			return decodeSlice(dc, kong.MapperFunc(func(dc *kong.DecodeContext, target reflect.Value) error {
				return mapNativeStorePath(dc, workdir, target)
			}), target)
		}
		return fmt.Errorf("%v is not a zbstore.Path", tp)
	}

	var arg string
	if err := dc.Scan.PopValueInto("path", &arg); err != nil {
		return err
	}
	path, err := zbstore.ParsePath(resolvePath(workdir, arg))
	if err != nil {
		return err
	}
	target.Set(reflect.ValueOf(path))
	return nil
}

// decodeSlice scans values into a slice like Kong does
// with the given [kong.Mapper] for each element.
func decodeSlice(dc *kong.DecodeContext, mapper kong.Mapper, target reflect.Value) error {
	tp := target.Type()
	if tp.Kind() != reflect.Slice {
		return fmt.Errorf("%v is not a slice", tp)
	}

	var elementScanner *kong.Scanner
	if dc.Value.Flag != nil {
		token := dc.Scan.Pop()
		tail := ""
		sep := dc.Value.Tag.Sep
		if sep != -1 {
			tail += string(sep) + "..."
		}
		if token.IsEOL() {
			return fmt.Errorf("missing value, expecting \"<arg>%s\"", tail)
		}
		switch v := reflect.ValueOf(token.Value); v.Kind() {
		case reflect.String:
			elementScanner = kong.ScanAsType(token.Type, kong.SplitEscaped(v.String(), sep)...)
		case reflect.Array, reflect.Slice:
			tokens := make([]kong.Token, 0, v.Len())
			for _, element := range v.Seq2() {
				tokens = append(tokens, kong.Token{
					Type:  token.Type,
					Value: element,
				})
			}
			elementScanner = kong.ScanFromTokens(tokens...)
		default:
			elementScanner = kong.ScanFromTokens(token)
		}
	} else {
		tokens := dc.Scan.PopWhile(kong.Token.IsValue)
		elementScanner = kong.ScanFromTokens(tokens...)
	}

	elementType := tp.Elem()
	for !elementScanner.Peek().IsEOL() {
		newSlice := reflect.Append(target, reflect.Zero(elementType))
		newElement := newSlice.Index(newSlice.Len() - 1)
		if err := mapper.Decode(dc.WithScanner(elementScanner), newElement); err != nil {
			return err
		}
		target.Set(newSlice)
	}

	return nil
}

func findFlagByName(name string, flags iter.Seq[*kong.Flag]) *kong.Flag {
	for flag := range flags {
		if flag.Name == name {
			return flag
		}
	}
	return nil
}
