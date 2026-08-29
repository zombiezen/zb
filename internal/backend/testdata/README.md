# Backend Tests

This directory holds the backend test suite.
Each backend test has a corresponding function in the package's tests.
Each test is written in [txtar][] format
with the comment written in [`rsc.io/script`][] syntax (similar to bash).

[`rsc.io/script`]: https://pkg.go.dev/rsc.io/script
[txtar]: https://pkg.go.dev/golang.org/x/tools/txtar

## Store Objects

Before running the test script,
the files in the txtar archive are converted into store objects
with `internal/storetest.TxtarObjects`.
Txtar filenames are written relative to the store directory
and given fake digests
(which can be generated with `go run zb.256lights.llc/pkg/cmd/zb-test-tool generate-digest`)
so that references can be detected and rewritten.
`internal/storetest.TxtarObjects` will process .drv files
by stripping any whitespace surrounding tokens
and rewriting their store paths to match the test's temporary store directory.

If a store object's filenames are prefixed with `[fallback]` and/or `[backend]`,
then the store object will be written only to those stores specified.
A store object prefixed with `[null]` is only used to compute a path,
but will not be written to either store.
By default, store objects will only be written to the backend store.

## Scripts

Scripts are run from the temporary store directory created for the test.

Scripts have the following commands available:

- `realize [--clean] drvPath...`: realize one or more derivations in the store.
  If a single derivation path is used,
  then output paths will be available in environment variables
  matching their output names (e.g. `$out`).
- `fetch path...`: fetch one or more store objects from fallback
- `write-realization drvPath!outputName path`: write a realization to the fallback store
- `cmpinfo path...`: verify that info from store matches info from test
- `delete [-r] path...`: delete one or more store objects
- `env [key[=value]...]`: set or log the values of environment variables
- `echo string...`: display a line of text
- `storepath path...`: writes store paths to stdout, followed by a newline
- `realpath path...`: writes filesystem paths to stdout, followed by a newline
- `read name...`: read one line from the stdout buffer and assign to names
- `stdout pattern`: find lines in the stdout buffer that match a pattern
- `stderr pattern`: find lines in the stderr buffer that match a pattern
- `grep pattern file`: find lines in a file that match a pattern
- `exists [-readonly] [-exec] file...`: check that files exist
- `only [path...]`: verify that the store contains exactly the set of objects named
- `wait`: wait for completion of background commands
- `stop [msg]`: stop execution of the script without reporting an error
- `skip [msg]`: skip the current test

## Script Conditions

The command prefix `[cond]` indicates that the command on the rest of the line should only run when the condition is satisfied.

A condition can be negated: `[!windows]` means to run the rest of the line
only if the test is not running on Windows.
Multiple conditions may be given for a single command,
for example, `[x86_64] [linux] skip`.
The command will run if all conditions are satisfied.

Scripts have the following conditions available:

- `x86_64`
- `aarch64`
- `linux`
- `macos`
- `windows`

