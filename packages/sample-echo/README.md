# sample-echo

A deterministic stand-in plugin member, written in .NET: it transforms each instruction's text
(upper-case or reversed) and can be asked to block, hand back, fail, sleep or publish. Hire it to try
plugin members, triggers on a plugin's events and Stop without running a model.
[`sample-echo-go`](../sample-echo-go) is the same plugin in Go. See `docs/plugins.md` in the Yawble
repository for the manifest, the protocol and hiring.

- **Self-contained.** The image guarantees the .NET runtime, Node and Python 3, and nothing else.
  Everything else this plugin needs lives in its own folder: `dotnet publish` puts every NuGet
  package it uses into `lib/` beside `SampleEcho.dll`. Nothing a plugin needs is ever added to the
  image.
- **`requires`.** The manifest declares `"requires": ["dotnet"]`: the runtime it needs from the
  image. The Host refuses the plugin, naming the runtime, when that runtime is not on its path.
- **One launcher for both processors.** A framework-dependent build runs unchanged on x64 and arm64,
  so `sample-echo` (the launcher) serves both and the manifest has no `platforms`.

Building it needs the .NET SDK (10) as well as sh: the only package here that does. Build a version
folder and install it:

```sh
packages/sample-echo/build.sh ~/plugins-build/sample-echo/0.1.0
yawble plugin install ~/plugins-build/sample-echo/0.1.0
```

or install the `sample-echo-0.1.0.zip` that `scripts/build.sh` writes, unpacked.
