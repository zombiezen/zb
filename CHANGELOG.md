# zb Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

[Unreleased]: https://github.com/256lights/zb/compare/v0.1.0...main

## [Unreleased][]

Version 0.2 adds support for remote caching.
This is a foundational feature which allows zb users to reuse build results
from public caches, continuous integration servers, or even other teammates' machines.
[256 Lights](https://256lights.llc) runs a public cache for the [standard library](https://github.com/256lights/zb-stdlib),
which turns the "Hello, World!" build on a new machine
from roughly 10 minutes to XX seconds with a reasonably fast internet connection.

### Added

- `zb serve` can now be [configured](https://zb.256lights.llc/admin/configuration#confval-server.download)
  to download built artifacts from a [remote cache](https://zb.256lights.llc/binary-cache/).
  ([#39](https://github.com/256lights/zb/issues/39)
  and [#43](https://github.com/256lights/zb/issues/43))
  Thank you to [@Abdiramen](https://github.com/Abdiramen)
  for the support in getting the algorithm over the finish line.
  Remote caches can be Google Cloud Storage
  ([#258](https://github.com/256lights/zb/issues/258)),
  network filesystems, or any HTTP server.
- `zb serve` can also be configured to upload built artifacts to a remote cache.
  ([#165](https://github.com/256lights/zb/issues/165))
  HTTP remote caches must support PUT requests to be used.
  Build artifacts are compressed using [zstd][] by default,
  falling back to gzip or uncompressed if the remote cache does not support zstd.
  ([#260](https://github.com/256lights/zb/issues/260))
- `zb serve` now signs realizations using configured keys.
  ([#159](https://github.com/256lights/zb/issues/159))
- `zb build` will restrict realizations to public keys it trusts.
  A new `zb build --clean` flag allows building without using any realizations.
  ([#20](https://github.com/256lights/zb/issues/20))
- A new `zb store object copy` command
  enables copying objects to and from remote stores.
  ([#337](https://github.com/256lights/zb/pull/337))
- New [`lazy` function](https://zb.256lights.llc/lua/extensions#lazy).
  ([#83](https://github.com/256lights/zb/issues/83))
- New [`readFile` function](https://zb.256lights.llc/lua/extensions#readFile).
  ([#148](https://github.com/256lights/zb/issues/148))
  Thank you to [@winterqt](https://github.com/winterqt)!
- HTTP requests can be authenticated via a `.netrc` file.
- Various settings can now be set in a [configuration file](https://zb.256lights.llc/configuration).
  ([#162](https://github.com/256lights/zb/issues/162))
- `zb serve` will reject new build requests
  and exit successfully after all builds have completed
  after receiving `SIGUSR2`.
  ([#259](https://github.com/256lights/zb/issues/259))
- `zb build` now [uses an `__outputs` metamethod](https://zb.256lights.llc/build-args)
  to determine what derivations to build
  rather than requiring the result to be a `derivation` function result.
  A new [built-in `outputs` function](https://zb.256lights.llc/lua/extensions#outputs)
  serves to access the metatable field.
  Thank you to [@ocurr](https://github.com/ocurr) for the design feedback!
  ([#117](https://github.com/256lights/zb/issues/117))

[zstd]: https://en.wikipedia.org/wiki/Zstd

### Changed

- `zb eval` now prints its output using Lua's `tostring` rules
  instead of its own formatting logic.
- Attempting to run multiple `zb serve` processes
  on a single store database now results in an error.
  ([#207](https://github.com/256lights/zb/issues/207))
  Thank you to [@Abdiramen](https://github.com/Abdiramen)!
- Small HTTP requests are now cached on disk.
  ([#140](https://github.com/256lights/zb/issues/140))
  This benefits `zb build` arguments of remote URLs
  as well as remote caching in `zb serve`.

### Removed

- URL arguments no longer search top-level tables with the current system string.
  This wasn't documented, but the standard library assumed its usage.
- `zb serve` will no longer import objects without a content address set in the trailer.
  Omitting the content address would cause `zb serve` to compute a source content address,
  causing an additional full read over the store object.
  Existing zb tooling would always send the content address,
  since computing the content address is necessary to compute the proper store path.
  This was originally intended as a way to be compatible with `nix-store --export`,
  but it's unclear that such a property is useful or desirable,
  and removing support for this feature makes the import more robust.

### Fixed

- `zb store object delete` is no longer flaky.
  ([#135](https://github.com/256lights/zb/issues/135))
- `zb build --out-link` is now respected.
  ([#324](https://github.com/256lights/zb/issues/324))
- Lua operator metamethods now receive their arguments in the correct order
  when one of the operands is a constant.
  ([#152](https://github.com/256lights/zb/issues/152))
- `string.format` in Lua now handles infinity and NaN.
  Thank you to [@HigherOrderLogic](https://github.com/HigherOrderLogic)!
  ([#78](https://github.com/256lights/zb/issues/78))
- Improved backend disk performance by performing less `fsync` syscalls.
- `zb serve` now performs more integrity checks during store import.
  ([#66](https://github.com/256lights/zb/issues/66))
- Updated to Go 1.26.5.

## [0.1.0][] - 2025-06-15

Initial public release.
Special thanks to [@ocurr](https://github.com/ocurr) for early tester feedback
and to [@ejrichards](https://github.com/ejrichards) for NixOS support!

[0.1.0]: https://github.com/256lights/zb/releases/tag/v0.1.0
