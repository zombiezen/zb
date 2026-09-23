// Copyright 2026 The zb Authors
// SPDX-License-Identifier: MIT

package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"

	"zb.256lights.llc/pkg/internal/osutil"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/sets"
	"zb.256lights.llc/pkg/zbstore"
	"zombiezen.com/go/log"
)

const bubblewrapProgramName = "bwrap"

func runBubblewrapped(ctx context.Context, invocation *builderInvocation) error {
	if dir := invocation.derivation.Dir; !dir.IsNative() {
		return fmt.Errorf("using non-native store %s", dir)
	}

	inputs := make(sets.Set[zbstore.Path])
	for inputPath := range invocation.derivation.InputSources.Values() {
		err := invocation.closure(inputPath, func(path zbstore.Path) bool {
			inputs.Add(path)
			return true
		})
		if err != nil {
			return err
		}
	}
	for input := range invocation.derivation.InputDerivationOutputs() {
		inputPath, err := invocation.lookup(input)
		if err != nil {
			return err
		}
		err = invocation.closure(inputPath, func(path zbstore.Path) bool {
			inputs.Add(path)
			return true
		})
		if err != nil {
			return err
		}
	}
	// If any of the sandbox paths reference a store path,
	// then add the store object's closure as an input.
	for _, hostPath := range invocation.sandboxPaths {
		hostStorePath, _, err := invocation.derivation.Dir.ParsePath(hostPath)
		if err != nil {
			continue
		}
		err = invocation.closure(hostStorePath, func(path zbstore.Path) bool {
			inputs.Add(path)
			return true
		})
		if err != nil {
			return err
		}
	}

	// Create a temporary directory inside the store
	// so we can rename the outputs to their expected locations.
	outputsDir := filepath.Join(invocation.realStoreDir, invocation.derivationPath.Base()+".outputs")
	if err := os.Mkdir(outputsDir, 0o755); err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(outputsDir); err != nil {
			log.Errorf(ctx, "Failed to clean up: %v", err)
		}
	}()

	caFile, err := defaultSystemCertFile()
	if err != nil {
		return err
	}

	const workDir = "/build"
	var b bubblewrapArgBuilder
	defer b.closeFiles()
	b.init(workDir, invocation.hasNetwork())
	b.perms(0o777 | os.ModeSticky)
	b.dir("/tmp")
	b.bind(invocation.buildDir, workDir)
	b.dir("/pts")

	b.perms(0o755)
	b.dir("/etc")
	if err := b.writeFile("/etc/passwd", sandboxPasswd(1000, 1000)); err != nil {
		return err
	}
	if err := b.writeFile("/etc/group", sandboxGroup(1000)); err != nil {
		return err
	}
	err = b.writeFile("/etc/hosts", []byte(""+
		"127.0.0.1 localhost\n"+
		"::1 localhost\n"))
	if err != nil {
		return err
	}
	if invocation.hasNetwork() {
		err := b.writeFile("/etc/nsswitch.conf", []byte(""+
			"hosts: files dns\n"+
			"services: files\n"))
		if err != nil {
			return err
		}
		if err := b.mirror("/etc/resolv.conf", "/etc/resolv.conf"); err != nil {
			return err
		}
		if err := b.mirror("/etc/services", "/etc/services"); err != nil {
			return err
		}
		if err := b.mirror("/etc/hosts", "/etc/hosts"); err != nil {
			return err
		}
		if caFile != "" {
			if err := b.mirror(caFile, "/etc/ssl/certs/ca-certificates.crt"); err != nil {
				return err
			}
		}
	}
	b.chmod("/etc", 0o555)

	b.dir(filepath.Dir(string(invocation.derivation.Dir)))
	b.bind(outputsDir, string(invocation.derivation.Dir))
	for input := range inputs {
		if inputDir := input.Dir(); inputDir != invocation.derivation.Dir {
			return fmt.Errorf("input %s is not inside %s", input, invocation.derivation.Dir)
		}
		hostPath := filepath.Join(invocation.realStoreDir, input.Base())
		if err := b.mirror(hostPath, string(input)); err != nil {
			return err
		}
	}
	for sandboxPath, hostPath := range invocation.sandboxPaths {
		if err := b.mirror(hostPath, sandboxPath); err != nil {
			return err
		}
	}

	env := maps.Clone(invocation.derivation.Env)
	fillBaseEnv(env, invocation.derivation.Dir, workDir, invocation.cores)
	for k, v := range xmaps.Sorted(env) {
		b.setenv(k, v)
	}

	c := exec.CommandContext(ctx, bubblewrapProgramName)
	c.Args = append(c.Args, "--level-prefix", "--args", "3", "--", invocation.derivation.Builder)
	c.Args = append(c.Args, invocation.derivation.Args...)
	setCancelFunc(c)
	c.Stdout = invocation.logWriter
	c.Stderr = invocation.logWriter
	log.Debugf(ctx, "bubblewrap arguments: %+q", b.args)
	c.ExtraFiles, err = b.finish()
	if err != nil {
		return err
	}
	if err := c.Run(); err != nil {
		return builderFailure{err}
	}

	for outputName, outputPath := range invocation.outputPaths {
		src := filepath.Join(outputsDir, outputPath.Base())
		dst := filepath.Join(invocation.realStoreDir, outputPath.Base())
		if err := os.Rename(src, dst); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// If the output does not exist, ignore the error.
				// The overall builder run will detect it and report it more appropriately to the user.
				ref := zbstore.OutputReference{
					DrvPath:    invocation.derivationPath,
					OutputName: outputName,
				}
				log.Debugf(ctx, "Failed to move output to destination for %v: %v", ref, err)
				continue
			}
			return err
		}
	}

	return nil
}

func sandboxPasswd(builderUID, builderGID int) []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("root:x:0:0:Nix build user:/build:/noshell\n")
	if builderUID != 0 {
		fmt.Fprintf(buf, "zb:x:%d:%d:zb build user:/build:/noshell\n", builderUID, builderGID)
	}
	buf.WriteString("nobody:x:65534:65534:Nobody:/:/noshell\n")
	return buf.Bytes()
}

func sandboxGroup(builderGID int) []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("root:x:0:\n")
	if builderGID != 0 {
		fmt.Fprintf(buf, "zb:!:%d:\n", builderGID)
	}
	buf.WriteString("nogroup:x:65534:\n")
	return buf.Bytes()
}

func defaultSystemCertFile() (string, error) {
	if path := os.Getenv("SSL_CERT_FILE"); path != "" {
		return path, nil
	}

	paths := iter.Seq[string](func(yield func(string) bool) {
		// Debian/Ubuntu/Gentoo etc.
		if !yield("/etc/ssl/certs/ca-certificates.crt") {
			return
		}
		// Fedora/RHEL 6
		if !yield("/etc/pki/tls/certs/ca-bundle.crt") {
			return
		}
		// OpenSUSE
		if !yield("/etc/ssl/ca-bundle.pem") {
			return
		}
		// OpenELEC
		if !yield("/etc/pki/tls/cacert.pem") {
			return
		}
		// CentOS/RHEL 7
		if !yield("/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem") {
			return
		}
		// Alpine Linux
		if !yield("/etc/ssl/cert.pem") {
			return
		}
	})
	return osutil.FirstPresentFile(paths)
}

type bubblewrapArgBuilder struct {
	args       []byte
	extraFiles []*os.File
}

func (b *bubblewrapArgBuilder) init(cwd string, network bool) {
	b.closeFiles()
	b.extraFiles = append(b.extraFiles, nil)

	b.args = b.args[:0]
	b.args = append(b.args, ""+
		"--unshare-user"+
		"\x00--unshare-pid"+
		"\x00--unshare-ipc"+
		"\x00--unshare-cgroup-try"+
		"\x00--new-session"+
		"\x00--die-with-parent"+
		"\x00--uid\x001000"+
		"\x00--gid\x001000"+
		"\x00--dev\x00/dev"+
		"\x00--proc\x00/proc"+
		"\x00--clearenv"+
		"\x00--chdir\x00"...)
	b.args = append(b.args, cwd...)
	if !network {
		b.args = append(b.args, "\x00--unshare-net\x00--unshare-uts"...)
	}
}

func (b *bubblewrapArgBuilder) closeFiles() {
	for _, f := range slices.Backward(b.extraFiles) {
		if f != nil {
			f.Close()
		}
	}
	clear(b.extraFiles)
	b.extraFiles = b.extraFiles[:0]
}

func (b *bubblewrapArgBuilder) finish() ([]*os.File, error) {
	var err error
	b.extraFiles[0], err = newUnlinkedFile(b.args)
	if err != nil {
		b.closeFiles()
		return nil, err
	}
	extraFiles := b.extraFiles
	b.extraFiles = nil
	return extraFiles, nil
}

func (b *bubblewrapArgBuilder) setenv(key, value string) {
	b.args = append(b.args, "\x00--setenv\x00"...)
	b.args = append(b.args, key...)
	b.args = append(b.args, 0)
	b.args = append(b.args, value...)
}

func (b *bubblewrapArgBuilder) bind(src, dest string) {
	b.args = append(b.args, "\x00--bind\x00"...)
	b.args = append(b.args, src...)
	b.args = append(b.args, 0)
	b.args = append(b.args, dest...)
}

func (b *bubblewrapArgBuilder) bindReadOnly(src, dest string) {
	b.args = append(b.args, "\x00--ro-bind\x00"...)
	b.args = append(b.args, src...)
	b.args = append(b.args, 0)
	b.args = append(b.args, dest...)
}

func (b *bubblewrapArgBuilder) dir(dest string) {
	b.args = append(b.args, "\x00--dir\x00"...)
	b.args = append(b.args, dest...)
}

func (b *bubblewrapArgBuilder) file(dest string, f *os.File) {
	b.args = append(b.args, "\x00--file\x00"...)
	b.args = strconv.AppendInt(b.args, int64(3+len(b.extraFiles)), 10)
	b.args = append(b.args, 0)
	b.args = append(b.args, dest...)
	b.extraFiles = append(b.extraFiles, f)
}

func (b *bubblewrapArgBuilder) writeFile(dest string, content []byte) error {
	f, err := newUnlinkedFile(content)
	if err != nil {
		return err
	}
	b.file(dest, f)
	return nil
}

func (b *bubblewrapArgBuilder) symlink(oldname, newname string) {
	b.args = append(b.args, "\x00--symlink\x00"...)
	b.args = append(b.args, oldname...)
	b.args = append(b.args, 0)
	b.args = append(b.args, newname...)
}

func (b *bubblewrapArgBuilder) mirror(oldname, newname string) (err error) {
	defer func() {
		if err != nil {
			err = &os.LinkError{
				Op:  "bind mount",
				Old: oldname,
				New: newname,
				Err: err,
			}
		}
	}()

	info, err := os.Lstat(oldname)
	if err != nil {
		return err
	}
	switch info.Mode().Type() {
	case os.ModeDir:
		b.perms(0o777)
		b.dir(filepath.Dir(newname))
		b.bindReadOnly(oldname, newname)
	case os.ModeSymlink:
		target, err := os.Readlink(oldname)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(oldname), target)
		}
		b.perms(0o777)
		b.symlink(target, newname)
	default:
		b.perms(0o777)
		if err := b.writeFile(newname, nil); err != nil {
			return err
		}
		b.bindReadOnly(oldname, newname)
	}
	return nil
}

func (b *bubblewrapArgBuilder) perms(perm os.FileMode) {
	b.args = append(b.args, "\x00--perms\x00"...)
	b.args = appendFileMode(b.args, perm)
}

func (b *bubblewrapArgBuilder) chmod(name string, perm os.FileMode) {
	b.args = append(b.args, "\x00--chmod\x00"...)
	b.args = appendFileMode(b.args, perm)
	b.args = append(b.args, 0)
	b.args = append(b.args, name...)
}

func appendFileMode(dst []byte, perm os.FileMode) []byte {
	dst = slices.Grow(dst, 6)
	buf := append(dst[len(dst):], "000"...)
	mode := uint64(perm.Perm())
	if perm&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	buf = strconv.AppendUint(buf, mode, 8)
	return append(dst, buf[len(buf)-4:]...)
}

func newUnlinkedFile(content []byte) (*os.File, error) {
	if len(content) == 0 {
		return os.Open(os.DevNull)
	}
	f, err := os.CreateTemp("", "sandbox*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	if _, err := f.WriteAt(content, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
