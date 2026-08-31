// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package frontend

import (
	"context"
	"fmt"
	"strings"

	"zb.256lights.llc/pkg/internal/lua"
	"zb.256lights.llc/pkg/internal/lualex"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/nix"
)

const derivationTypeName = "derivation"

type derivation struct {
	*zbstore.Derivation
	path zbstore.Path
}

func (drv *derivation) Freeze() error { return nil }

func registerDerivationMetatable(ctx context.Context, l *lua.State) error {
	lua.NewMetatable(l, derivationTypeName)
	err := lua.SetPureFunctions(ctx, l, 0, map[string]lua.Function{
		"__index":     indexDerivation,
		"__pairs":     derivationPairs,
		"__tostring":  derivationToString,
		"__outputs":   derivationOutputs,
		"__concat":    concatDerivation,
		"__metatable": nil, // prevent Lua access to metatable
	})
	if err != nil {
		return err
	}
	l.Pop(1)
	return nil
}

func (eval *Eval) derivationFunction(ctx context.Context, l *lua.State) (int, error) {
	if !l.IsTable(1) {
		return 0, lua.NewTypeError(l, 1, lua.TypeTable.String())
	}
	drv := &derivation{
		Derivation: &zbstore.Derivation{
			Dir: eval.storeDir,
			Env: make(map[string]string),
		},
	}

	// Configure outputs.
	var h nix.Hash
	switch typ := l.RawField(1, "outputHash"); typ {
	case lua.TypeNil:
	case lua.TypeString:
		s, _ := l.ToString(-1)
		var err error
		h, err = nix.ParseHash(s)
		if err != nil {
			return 0, fmt.Errorf("outputHash argument: %v", err)
		}
	default:
		return 0, fmt.Errorf("outputHash argument: %v expected, got %v", lua.TypeString, typ)
	}
	l.Pop(1)

	switch typ := l.RawField(1, "outputHashMode"); typ {
	case lua.TypeNil:
		if !h.IsZero() {
			drv.Outputs = map[string]*zbstore.DerivationOutputType{
				zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(nix.FlatFileContentAddress(h)),
			}
		}
	case lua.TypeString:
		switch mode, _ := l.ToString(-1); mode {
		case "flat":
			drv.Outputs = map[string]*zbstore.DerivationOutputType{
				zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(nix.FlatFileContentAddress(h)),
			}
		case "recursive":
			drv.Outputs = map[string]*zbstore.DerivationOutputType{
				zbstore.DefaultDerivationOutputName: zbstore.FixedCAOutput(nix.RecursiveFileContentAddress(h)),
			}
		default:
			return 0, fmt.Errorf("outputHashMode argument: invalid mode %q", mode)
		}
	default:
		return 0, fmt.Errorf("outputHashMode argument: %v expected, got %v", lua.TypeString, typ)
	}
	l.Pop(1)

	if h.IsZero() {
		// TODO(someday): Multiple outputs.
		drv.Outputs = map[string]*zbstore.DerivationOutputType{
			zbstore.DefaultDerivationOutputName: zbstore.RecursiveFileFloatingCAOutput(nix.SHA256),
		}
	}

	// Start a copy of the table.
	l.CreateTable(0, int(l.RawLen(1)))
	tableCopyIndex := l.Top()

	// Obtain environment variables from extra pairs.
	l.PushNil()
	for l.Next(1) {
		if l.Type(-2) != lua.TypeString {
			// Skip this pair.
			l.Pop(1)
			continue
		}

		// Store copy of pair into table.
		l.PushValue(-2) // Push key.
		l.PushValue(-2) // Push value.
		// TODO(soon): Validate primitive or list.
		l.RawSet(tableCopyIndex)

		// Handle special pairs.
		k, _ := l.ToString(-2)
		switch k {
		case "name":
			if typ := l.Type(-1); typ != lua.TypeString {
				return 0, fmt.Errorf("name argument: %v expected, got %v", lua.TypeString, typ)
			}
			drv.Name, _ = l.ToString(-1)
			if _, err := eval.storeDir.Object("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-" + drv.Name); err != nil {
				return 0, fmt.Errorf("name argument: %s is an invalid name", lualex.Quote(drv.Name))
			}
		case "system":
			if typ := l.Type(-1); typ != lua.TypeString {
				return 0, fmt.Errorf("system argument: %v expected, got %v", lua.TypeString, typ)
			}
			drv.System, _ = l.ToString(-1)
		case "builder":
			if typ := l.Type(-1); typ != lua.TypeString {
				return 0, fmt.Errorf("system argument: %v expected, got %v", lua.TypeString, typ)
			}
			var err error
			drv.Builder, err = stringToEnvVar(l, drv.Derivation, -1)
			if err != nil {
				return 0, fmt.Errorf("%s: %v", k, err)
			}
		case "args":
			if typ := l.Type(-1); typ != lua.TypeTable {
				return 0, fmt.Errorf("args argument: %v expected, got %v", lua.TypeTable, typ)
			}
			err := ipairs(ctx, l, -1, func(i int64) error {
				arg, err := stringToEnvVar(l, drv.Derivation, -1)
				if err != nil {
					return fmt.Errorf("#%d: %v", i, err)
				}
				drv.Args = append(drv.Args, arg)
				return nil
			})
			if err != nil {
				return 0, fmt.Errorf("%s %v", k, err)
			}
		}

		v, err := toEnvVar(ctx, l, drv.Derivation, -1, true)
		if err != nil {
			return 0, fmt.Errorf("%s: %v", k, err)
		}
		drv.Env[k] = v

		// Remove value, keeping key for the next iteration.
		l.Pop(1)
	}

	for outputName, outType := range drv.Outputs {
		switch {
		case outType.IsFloating():
			drv.Env[outputName] = zbstore.HashPlaceholder(outputName)
		case outType.IsFixed():
			p, err := drv.OutputPath(outputName)
			if err != nil {
				panic(err)
			}
			drv.Env[outputName] = string(p)
		default:
			panic(outputName + " has an unhandled output type")
		}
	}
	obj, err := drv.Derivation.Export(nix.SHA256)
	if err != nil {
		return 0, fmt.Errorf("derivation: %v", err)
	}
	if err := eval.store.WriteObject(ctx, obj); err != nil {
		return 0, fmt.Errorf("derivation: %v", err)
	}
	drv.path = obj.StorePath

	pushStorePath(l, drv.path)
	if err := l.SetField(ctx, tableCopyIndex, "drvPath"); err != nil {
		return 0, fmt.Errorf("derivation: %v", err)
	}
	for outputName, outType := range drv.Outputs {
		var placeholder string
		switch {
		case outType.IsFloating():
			placeholder = zbstore.UnknownCAOutputPlaceholder(zbstore.OutputReference{
				DrvPath:    drv.path,
				OutputName: zbstore.DefaultDerivationOutputName,
			})
		case outType.IsFixed():
			// TODO(someday): We already computed this earlier.
			p, err := drv.OutputPath(outputName)
			if err != nil {
				panic(err)
			}
			placeholder = string(p)
		}
		ref := zbstore.OutputReference{
			DrvPath:    drv.path,
			OutputName: outputName,
		}
		l.PushStringContext(placeholder, sets.New(contextValue{outputReference: ref}.String()))
		if err := l.SetField(ctx, tableCopyIndex, outputName); err != nil {
			return 0, fmt.Errorf("derivation: %v", err)
		}
	}

	l.NewUserdata(drv, 1)
	l.Rotate(-2, -1) // Swap userdata and argument table copy.
	if err := l.SetUserValue(-2, 1); err != nil {
		return 0, err
	}

	if err := lua.SetMetatable(l, derivationTypeName); err != nil {
		return 0, err
	}

	return 1, nil
}

func toEnvVar(ctx context.Context, l *lua.State, drv *zbstore.Derivation, idx int, allowLists bool) (string, error) {
	idx = l.AbsIndex(idx)
	switch typ := l.Type(idx); typ {
	case lua.TypeNil:
		return "", nil
	case lua.TypeBoolean:
		if !l.ToBoolean(idx) {
			return "", nil
		}
		return "1", nil
	case lua.TypeString, lua.TypeNumber:
		return stringToEnvVar(l, drv, idx)
	default:
		if hasMethod, err := lua.CallMeta(ctx, l, idx, "__tostring"); err != nil {
			return "", err
		} else if hasMethod {
			s, err := stringToEnvVar(l, drv, -1)
			l.Pop(1)
			if err != nil {
				return "", fmt.Errorf("__tostring result: %v", err)
			}
			return s, nil
		}

		// No __tostring? Then assume this is an array/list.
		if typ != lua.TypeTable {
			return "", fmt.Errorf("%v cannot be used as an environment variable", typ)
		}
		if !allowLists {
			return "", fmt.Errorf("sub-tables not permitted as environment variable values")
		}
		sb := new(strings.Builder)
		err := ipairs(ctx, l, idx, func(i int64) error {
			if i > 1 {
				sb.WriteString(" ")
			}
			s, err := toEnvVar(ctx, l, drv, -1, false)
			if err != nil {
				return fmt.Errorf("#%d: %v", i, err)
			}
			sb.WriteString(s)
			return nil
		})
		if err != nil {
			return "", err
		}
		return sb.String(), nil
	}
}

func stringToEnvVar(l *lua.State, drv *zbstore.Derivation, idx int) (string, error) {
	if !l.IsString(idx) {
		return "", fmt.Errorf("%v is not a string", l.Type(idx))
	}
	l.PushValue(idx) // Clone so that we don't munge a number.
	defer l.Pop(1)
	s, _ := l.ToString(-1)
	for dep := range l.StringContext(-1).All() {
		c, err := parseContextString(dep)
		if err != nil {
			return "", fmt.Errorf("internal error: %v", err)
		}
		switch {
		case c.path != "":
			drv.InputSources.Add(zbstore.Path(dep))
		case !c.outputReference.IsZero():
			if drv.InputDerivations == nil {
				drv.InputDerivations = make(map[zbstore.Path]*sets.Sorted[string])
			}
			if drv.InputDerivations[c.outputReference.DrvPath] == nil {
				drv.InputDerivations[c.outputReference.DrvPath] = new(sets.Sorted[string])
			}
			drv.InputDerivations[c.outputReference.DrvPath].Add(c.outputReference.OutputName)
		default:
			return "", fmt.Errorf("internal error: unhandled context %v", c)
		}
	}
	return s, nil
}

func toDerivation(l *lua.State) (*derivation, error) {
	const idx = 1
	if _, err := lua.CheckUserdata(l, idx, derivationTypeName); err != nil {
		return nil, err
	}
	drv := testDerivation(l, idx)
	if drv == nil {
		return nil, lua.NewArgError(l, idx, "could not extract derivation")
	}
	return drv, nil
}

func testDerivation(l *lua.State, idx int) *derivation {
	x, _ := lua.TestUserdata(l, idx, derivationTypeName)
	drv, _ := x.(*derivation)
	return drv
}

// indexDerivation handles the __index metamethod on derivations.
func indexDerivation(ctx context.Context, l *lua.State) (int, error) {
	if _, err := toDerivation(l); err != nil {
		return 0, err
	}
	l.UserValue(1, 1) // Push derivation argument table.
	l.PushValue(2)    // Copy key argument.
	if _, err := l.Table(ctx, -2); err != nil {
		return 0, err
	}
	return 1, nil
}

// derivationPairs handles the __pairs metamethod on derivations.
func derivationPairs(ctx context.Context, l *lua.State) (int, error) {
	if _, err := toDerivation(l); err != nil {
		return 0, err
	}
	l.UserValue(1, 1) // Push derivation argument table.
	l.PushPureFunction(1, derivationPairNext)
	l.PushNil()
	l.PushNil()
	return 3, nil
}

// derivationPairNext is the iterator function returned by the derivation __pairs metamethod.
func derivationPairNext(ctx context.Context, l *lua.State) (int, error) {
	l.PushValue(2) // Move control value to top of stack.
	if !l.Next(lua.UpvalueIndex(1)) {
		l.PushNil()
		return 1, nil
	}
	return 2, nil
}

// derivationToString handles the __tostring metamethod on derivations.
func derivationToString(ctx context.Context, l *lua.State) (int, error) {
	if _, err := toDerivation(l); err != nil {
		return 0, err
	}
	l.UserValue(1, 1) // Push derivation argument table.
	if _, err := l.Field(ctx, -1, "out"); err != nil {
		return 0, err
	}
	return 1, nil
}

// derivationOutputs handles the __outputs metamethod on derivations.
func derivationOutputs(ctx context.Context, l *lua.State) (int, error) {
	if _, err := toDerivation(l); err != nil {
		return 0, err
	}
	l.UserValue(1, 1) // Push derivation argument table.
	if _, err := l.Field(ctx, -1, "out"); err != nil {
		return 0, err
	}
	return 1, nil
}

// concatDerivation handles the __concat metamethod on derivations.
func concatDerivation(ctx context.Context, l *lua.State) (int, error) {
	l.SetTop(2)
	if testDerivation(l, 1) != nil {
		l.UserValue(1, 1) // Push derivation argument table.
		if _, err := l.Field(ctx, -1, "out"); err != nil {
			return 0, err
		}
		if err := l.Replace(1); err != nil {
			return 0, err
		}
		l.Pop(1)
	}
	if testDerivation(l, 2) != nil {
		l.UserValue(2, 1) // Push derivation argument table.
		if _, err := l.Field(ctx, -1, "out"); err != nil {
			return 0, err
		}
		if err := l.Replace(2); err != nil {
			return 0, err
		}
		l.Pop(1)
	}
	if err := l.Concat(ctx, 2); err != nil {
		return 0, err
	}
	return 1, nil
}

const derivationOutputContextPrefix = "!"

// A contextValue is a parsed Lua context string.
// It is conceptually a union type:
// at most one of the fields will have a non-zero value.
type contextValue struct {
	path            zbstore.Path
	outputReference zbstore.OutputReference
}

// parseContextString parses a marshaled [contextValue].
// It is the inverse of [contextValue.String].
func parseContextString(s string) (contextValue, error) {
	if rest, isDrv := strings.CutPrefix(s, derivationOutputContextPrefix); isDrv {
		ref, err := zbstore.ParseOutputReference(rest)
		if err != nil {
			return contextValue{}, fmt.Errorf("parse context string: %v", err)
		}
		return contextValue{outputReference: ref}, nil
	}

	p, err := zbstore.ParsePath(s)
	if err != nil {
		return contextValue{}, fmt.Errorf("parse context string: %v", err)
	}
	return contextValue{path: p}, nil
}

// String marshals a [contextValue] to a string.
// It is the inverse of [parseContextString].
func (c contextValue) String() string {
	switch {
	case c.path != "":
		return string(c.path)
	case !c.outputReference.IsZero():
		return derivationOutputContextPrefix + c.outputReference.String()
	default:
		return "<nil>"
	}
}

func pushStorePath(l *lua.State, path zbstore.Path) {
	l.PushStringContext(string(path), sets.New(contextValue{path: path}.String()))
}
