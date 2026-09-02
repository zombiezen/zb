# End-to-End Tests

This directory holds zb's end-to-end test suite.
Each test is written in [txtar][] format
with the comment written in [`rsc.io/script`][] syntax (similar to bash).

[`rsc.io/script`]: https://pkg.go.dev/rsc.io/script
[txtar]: https://pkg.go.dev/golang.org/x/tools/txtar

## Scripts

Scripts are run from a temporary directory, which `$HOME`/`%USERPROFILE%` is set to.
`$ZB_STORE_DIR` is set to a unique store directory for the test.
The test harness will automatically start a server
and record its socket in `$ZB_STORE_SOCKET`.
`$PATH` is inherited from the test process.

Scripts have all the [default commands](https://pkg.go.dev/rsc.io/script@v0.0.2/scripttest#DefaultCmds), plus:

- `zb`: run zb with the given arguments
- `read name...`: read one line from the stdout buffer and assign to names

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
- `short` is active when the `-test.short` flag is set.
- `verbose` is active when the `-test.v` flag is set.
- Conditions of the form `exec:foo` are active
  when the executable `foo` is present in `$PATH`.

## Files

Files in the txtar archive are written to the test's working directory
before the script is run.
If a file's content ends with `^D`, then the trailing newline that txtar ordinarily adds
will be stripped before writing.
