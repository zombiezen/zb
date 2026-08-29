// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/alecthomas/kong"
	kongcompletion "github.com/jotaen/kong-completion"
	"golang.org/x/tools/txtar"
	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/internal/storetest"
	"zb.256lights.llc/pkg/internal/system"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nixbase32"
)

const objectDigestSize = 32

type command struct {
	Debug          bool                   `kong:"help=Show debugging output."`
	GenerateDigest *generateDigestCommand `kong:"cmd"`
	Derivation     *derivationCommand     `kong:"cmd"`
	Txtar          *txtarCommand          `kong:"cmd"`
}

func main() {
	c := new(command)
	k := kong.Must(c,
		kong.Name("zb-test-tool"),
		kong.Description("Utilities for testing zb"),
		kong.Bind(c),
		kong.Vars{
			"directory": string(zbstore.DefaultDirectory()),
		},
	)
	kongcompletion.Register(k)

	kc, err := k.Parse(os.Args[1:])
	ctx := context.Background()
	initLogging(c.Debug)
	if err != nil {
		log.Errorf(ctx, "%v", err)
		os.Exit(1)
	}
	kc.BindTo(ctx, (*context.Context)(nil))
	err = kc.Run()
	if err != nil {
		log.Errorf(context.Background(), "%v", err)
		os.Exit(1)
	}
}

type generateDigestCommand struct {
}

func (c *generateDigestCommand) Signature() string {
	return `kong:"cmd,help=Generate a random object digest"`
}

func (c *generateDigestCommand) Run(kc *kong.Context) error {
	buf := make([]byte, 0, objectDigestSize+len("\n"))
	buf = appendNewDigest(buf)
	buf = append(buf, '\n')
	_, err := kc.Stdout.Write(buf)
	return err
}

func appendNewDigest(dst []byte) []byte {
	entropy := make([]byte, nixbase32.DecodedLen(objectDigestSize))
	rand.Read(entropy)
	dst = slices.Grow(dst, objectDigestSize)
	newEnd := len(dst) + objectDigestSize
	nixbase32.Encode(dst[len(dst):newEnd], entropy)
	return dst[:newEnd]
}

type derivationCommand struct {
	InputPlaceholder  *derivationInputPlaceholderCommand  `kong:"cmd"`
	OutputPlaceholder *derivationOutputPlaceholderCommand `kong:"cmd"`
	Format            *derivationFormatCommand            `kong:"cmd,aliases='fmt'"`
	FixedOutput       *derivationFixedOutputCommand       `kong:"cmd"`
}

type derivationInputPlaceholderCommand struct {
	OutputReference zbstore.OutputReference `kong:"arg"`
}

func (c *derivationInputPlaceholderCommand) Signature() string {
	return `kong:"cmd,help=Hash the placeholder for an input derivation\\'s output"`
}

func (c *derivationInputPlaceholderCommand) Run(kc *kong.Context) error {
	_, err := fmt.Fprintln(kc.Stdout, zbstore.UnknownCAOutputPlaceholder(c.OutputReference))
	return err
}

type derivationOutputPlaceholderCommand struct {
	OutputName string `kong:"arg,default=out"`
}

func (c *derivationOutputPlaceholderCommand) Signature() string {
	return `kong:"cmd,help=Hash the placeholder for an output"`
}

func (c *derivationOutputPlaceholderCommand) Run(kc *kong.Context) error {
	_, err := fmt.Fprintln(kc.Stdout, zbstore.HashPlaceholder(c.OutputName))
	return err
}

type derivationFormatCommand struct {
	InputPath string `kong:"name=file,arg,optional"`
	Write     bool   `kong:"short=w,help=Rewrite the input file."`
}

func (c *derivationFormatCommand) Signature() string {
	return `kong:"cmd,help=Pretty-print a derivation with whitespace"`
}

func (c *derivationFormatCommand) Run(kc *kong.Context) error {
	file := os.Stdin
	inputIsStdin := c.InputPath == "" || c.InputPath == "-"
	if !inputIsStdin {
		var flag int
		if c.Write {
			flag = os.O_RDWR
		} else {
			flag = os.O_RDONLY
		}
		var err error
		file, err = os.OpenFile(c.InputPath, flag, 0)
		if err != nil {
			return err
		}
		defer file.Close()
	}

	originalData, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	noWhitespace, err := storetest.MinimizeDerivation(originalData)
	if err != nil {
		return err
	}
	drv := new(zbstore.Derivation)
	if err := drv.UnmarshalText(noWhitespace); err != nil {
		return err
	}

	newData := marshalIndentDerivation(drv)
	if !c.Write || inputIsStdin {
		if _, err := kc.Stdout.Write(newData); err != nil {
			return err
		}
	} else {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return err
		}
		if _, err := file.Write(newData); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

type derivationFixedOutputCommand struct {
	Name string   `kong:"arg,help=Filename of the derivation without a digest."`
	Hash nix.Hash `kong:"arg,help=Hash of the file."`

	Directory zbstore.Directory `kong:"name=dir,default=${directory},help=Compute for the store directory."`
}

func (c *derivationFixedOutputCommand) Signature() string {
	return `kong:"cmd,help=Print the output path for a fixed-output derivation."`
}

func (c *derivationFixedOutputCommand) Run(kc *kong.Context) error {
	ca := nix.FlatFileContentAddress(c.Hash)
	path, err := zbstore.FixedCAOutputPath(c.Directory, c.Name, ca, zbstore.References{})
	if err != nil {
		return err
	}
	fmt.Fprintln(kc.Stdout, path)
	return nil
}

type txtarCommand struct {
	FillSystems *txtarFillSystemsCommand `kong:"cmd"`
}

type txtarFillSystemsCommand struct {
	Systems   []system.System `kong:"name=system,default='x86_64-linux,aarch64-linux,aarch64-apple-macos,x86_64-pc-windows'"`
	InputPath string          `kong:"name=file,arg,optional"`
	Write     bool            `kong:"short=w,help=Rewrite the input file."`
}

func (c *txtarFillSystemsCommand) Signature() string {
	return `kong:"cmd,help=Duplicate derivations in a txtar for other systems"`
}

func (c *txtarFillSystemsCommand) Run(kc *kong.Context) error {
	file := os.Stdin
	inputIsStdin := c.InputPath == "" || c.InputPath == "-"
	if !inputIsStdin {
		var flag int
		if c.Write {
			flag = os.O_RDWR
		} else {
			flag = os.O_RDONLY
		}
		var err error
		file, err = os.OpenFile(c.InputPath, flag, 0)
		if err != nil {
			return err
		}
		defer file.Close()
	}

	originalData, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	archive := txtar.Parse(originalData)
	derivations := make(map[zbstore.Path]*zbstore.Derivation)
	for _, curr := range archive.Files {
		fakePath, err := zbstore.DefaultUnixDirectory.Object(curr.Name)
		if err != nil {
			continue
		}
		drvName, isDrv := fakePath.DerivationName()
		if !isDrv {
			continue
		}
		drvData, err := storetest.MinimizeDerivation(curr.Data)
		if err != nil {
			return fmt.Errorf("%s: %v", curr.Name, err)
		}
		drv := &zbstore.Derivation{Name: drvName}
		if err := drv.UnmarshalText(drvData); err != nil {
			return fmt.Errorf("%s: %v", curr.Name, err)
		}
		drv.Dir, err = inferDerivationDirectory(drv)
		if err != nil {
			return fmt.Errorf("%s: %v", curr.Name, err)
		}
		drvPath, err := drv.Dir.Object(curr.Name)
		if err != nil {
			return fmt.Errorf("%s: %v", curr.Name, err)
		}
		derivations[drvPath] = drv
	}

	var derivationSequence iter.Seq[*zbstore.Derivation] = func(yield func(*zbstore.Derivation) bool) {
		for _, file := range archive.Files {
			drv := derivationForFileName(derivations, file.Name)
			if drv != nil {
				if !yield(drv) {
					return
				}
			}
		}
	}
	for i := 0; i < len(archive.Files); i++ {
		curr := archive.Files[i]
		drv := derivationForFileName(derivations, curr.Name)
		if drv == nil {
			continue
		}

		for _, wantSystem := range c.Systems {
			hasWantedSystem := derivationForSystem(derivationSequence, drv.Name, func(sys system.System) bool {
				return sys == wantSystem
			}) != nil
			if hasWantedSystem {
				continue
			}
			templateFile := derivationForSystem(derivationSequence, drv.Name, func(sys system.System) bool {
				return wantSystem.OS.IsWindows() == sys.OS.IsWindows()
			})
			if templateFile == nil {
				templateFile = drv
			}

			newDrv := rewriteDerivationForSystem(drv, wantSystem, derivations)
			base := string(appendNewDigest(nil)) + "-" + newDrv.Name + zbstore.DerivationExt
			newDrvPath, err := newDrv.Dir.Object(base)
			if err != nil {
				return err
			}
			derivations[newDrvPath] = newDrv

			i++
			archive.Files = slices.Insert(archive.Files, i, txtar.File{
				Name: base,
				Data: marshalIndentDerivation(newDrv),
			})
		}
	}

	newData := txtar.Format(archive)
	if !c.Write || inputIsStdin {
		if _, err := kc.Stdout.Write(newData); err != nil {
			return err
		}
	} else {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return err
		}
		if _, err := file.Write(newData); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func inferDerivationDirectory(drv *zbstore.Derivation) (zbstore.Directory, error) {
	if drv.Dir != "" {
		return drv.Dir, nil
	}
	sys, err := system.Parse(drv.System)
	if err != nil {
		return "", fmt.Errorf("infer directory: %v", err)
	}
	switch {
	case sys.OS.IsWindows():
		return zbstore.DefaultWindowsDirectory, nil
	case sys.OS.IsDarwin() || sys.OS.IsLinux():
		return zbstore.DefaultUnixDirectory, nil
	default:
		return "", fmt.Errorf("infer directory: unknown system %s", drv.System)
	}
}

func derivationForSystem(derivations iter.Seq[*zbstore.Derivation], drvName string, f func(system.System) bool) *zbstore.Derivation {
	for drv := range derivations {
		if drv.Name != drvName {
			continue
		}
		if drvSystem, err := system.Parse(drv.System); err == nil && f(drvSystem) {
			return drv
		}
	}
	return nil
}

func derivationForFileName(derivations map[zbstore.Path]*zbstore.Derivation, name string) *zbstore.Derivation {
	for drvPath, drv := range derivations {
		if drvPath.Base() == name {
			return drv
		}
	}
	return nil
}

func rewriteDerivationForSystem(drv *zbstore.Derivation, wantSystem system.System, others map[zbstore.Path]*zbstore.Derivation) *zbstore.Derivation {
	originalSystem, err := system.Parse(drv.System)
	if err != nil {
		panic(err)
	}

	var wantDirectory zbstore.Directory
	switch {
	case wantSystem.OS.IsWindows() && !originalSystem.OS.IsWindows():
		wantDirectory = zbstore.DefaultWindowsDirectory
	case drv.Dir == "" || !wantSystem.OS.IsWindows() && originalSystem.OS.IsWindows():
		wantDirectory = zbstore.DefaultUnixDirectory
	default:
		wantDirectory = drv.Dir
	}

	var replacements []string
	var newInputSources sets.Sorted[zbstore.Path]
	for oldSrc := range drv.InputSources.Values() {
		newSrc, err := drv.Dir.Object(oldSrc.Base())
		if err != nil {
			panic(err)
		}
		newInputSources.Add(newSrc)
		replacements = append(replacements, string(oldSrc), string(newSrc))
	}

	newInputDerivations := make(map[zbstore.Path]*sets.Sorted[string])
	for oldDrvPath, outputNames := range drv.InputDerivations {
		oldDrv := others[oldDrvPath]
		if oldDrv == nil {
			newInputDerivations[oldDrvPath] = outputNames
			continue
		}
		var newDrvPath zbstore.Path
		for otherDrvPath, other := range others {
			otherSystem, err := system.Parse(other.System)
			if err != nil {
				continue
			}
			if otherDrvPath.Name() == oldDrvPath.Name() && otherSystem == wantSystem {
				newDrvPath = otherDrvPath
				break
			}
		}
		if newDrvPath == "" {
			newInputDerivations[oldDrvPath] = outputNames
			continue
		}
		newInputDerivations[newDrvPath] = outputNames
		for outputName := range outputNames.Values() {
			oldRef := zbstore.OutputReference{
				DrvPath:    oldDrvPath,
				OutputName: outputName,
			}
			newRef := zbstore.OutputReference{
				DrvPath:    newDrvPath,
				OutputName: outputName,
			}
			replacements = append(replacements,
				zbstore.UnknownCAOutputPlaceholder(oldRef),
				zbstore.UnknownCAOutputPlaceholder(newRef),
			)
		}
	}

	newDrv := drv.ReplaceStrings(strings.NewReplacer(replacements...))
	newDrv.Dir = wantDirectory
	newDrv.System = wantSystem.String()
	newDrv.InputSources = newInputSources
	newDrv.InputDerivations = newInputDerivations
	return newDrv
}

func marshalIndentDerivation(drv *zbstore.Derivation) []byte {
	const indent = "  "
	var buf []byte
	buf = append(buf, "Derive(\n"+indent+"["...)
	if len(drv.Outputs) <= 1 {
		for outName, t := range drv.Outputs {
			var err error
			buf, err = zbstore.AppendDerivationOutput(buf, drv.Dir, drv.Name, outName, t)
			if err != nil {
				panic(err)
			}
		}
	} else {
		buf = append(buf, "\n"+indent...)
		for i, outName := range xmaps.SortedKeys(drv.Outputs) {
			buf = append(buf, indent...)
			var err error
			buf, err = zbstore.AppendDerivationOutput(buf, drv.Dir, drv.Name, outName, drv.Outputs[outName])
			if err != nil {
				panic(err)
			}
			if i < len(drv.Outputs)-1 {
				buf = append(buf, ',')
			}
			buf = append(buf, "\n"+indent...)
		}
	}

	buf = append(buf, "],\n"+indent+"["...)
	if len(drv.InputDerivations) > 0 {
		buf = append(buf, "\n"+indent...)
	}
	for i, k := range xmaps.SortedKeys(drv.InputDerivations) {
		buf = append(buf, indent+"("...)
		buf = aterm.AppendString(buf, string(k))
		buf = append(buf, ", ["...)
		outputs := drv.InputDerivations[k]
		for j, out := range outputs.All() {
			if j > 0 {
				buf = append(buf, ", "...)
			}
			buf = aterm.AppendString(buf, out)
		}
		buf = append(buf, "])"...)
		if i < len(drv.InputDerivations)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, "\n"+indent...)
	}

	buf = append(buf, "],\n"+indent+"["...)
	if drv.InputSources.Len() > 0 {
		buf = append(buf, "\n"+indent...)
	}
	for i, src := range drv.InputSources.All() {
		buf = append(buf, indent...)
		buf = aterm.AppendString(buf, string(src))
		if i < drv.InputSources.Len()-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, "\n"+indent...)
	}

	buf = append(buf, "],\n"+indent...)
	buf = aterm.AppendString(buf, drv.System)
	buf = append(buf, ",\n"+indent...)
	buf = aterm.AppendString(buf, drv.Builder)

	buf = append(buf, ",\n"+indent+"["...)
	if len(drv.Args) > 0 {
		buf = append(buf, "\n"+indent...)
	}
	for i, arg := range drv.Args {
		buf = append(buf, indent...)
		buf = aterm.AppendString(buf, arg)
		if i < len(drv.Args)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, "\n"+indent...)
	}

	buf = append(buf, "],\n"+indent+"["...)
	if len(drv.Env) > 0 {
		buf = append(buf, "\n"+indent...)
	}
	for i, k := range xmaps.SortedKeys(drv.Env) {
		buf = append(buf, indent+"("...)
		buf = aterm.AppendString(buf, k)
		buf = append(buf, ", "...)
		buf = aterm.AppendString(buf, drv.Env[k])
		buf = append(buf, ')')
		if i < len(drv.Env)-1 {
			buf = append(buf, ',')
		}
		buf = append(buf, "\n"+indent...)
	}

	buf = append(buf, "]\n)\n"...)

	return buf
}

var initLogOnce sync.Once

func initLogging(showDebug bool) {
	initLogOnce.Do(func() {
		minLogLevel := log.Info
		if showDebug {
			minLogLevel = log.Debug
		}
		log.SetDefault(&log.LevelFilter{
			Min:    minLogLevel,
			Output: log.New(os.Stderr, "zb: ", log.StdFlags, nil),
		})
	})
}
