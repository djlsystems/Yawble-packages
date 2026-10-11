# sample-echo-go

A deterministic stand-in plugin member, written in Go: the same behaviour and skill as
[`sample-echo`](../sample-echo) (.NET). Hire it to try plugin members, triggers on a plugin's events
and Stop without running a model. To start a plugin of your own, use the Go plugin template in the
Yawble repository (see "Choosing a language" in its `docs/plugins.md`); this package is the same code
built and published as something to install.

- **Self-contained.** `main.go` uses the standard library only, and `build.sh` builds it with
  `CGO_ENABLED=0` into one static binary per processor, so the plugin needs nothing from the image.
- **Both processors.** `build.sh` builds `bin/linux-x64/sample-echo-go` (`GOARCH=amd64`) and
  `bin/linux-arm64/sample-echo-go` (`GOARCH=arm64`); the manifest's `platforms` map names each, and
  the Host runs the one for its own processor.
- **No `requires`.** A static Go binary needs no runtime from the image.

Build a version folder and install it:

```sh
packages/sample-echo-go/build.sh ~/plugins-build/sample-echo-go/0.1.0
yawble plugin install ~/plugins-build/sample-echo-go/0.1.0
```

or install the `sample-echo-go-0.1.0.zip` that `scripts/build.sh` writes, unpacked. Then hire a member
on `plugin:sample-echo-go` and send it any instruction; the skill `skills/sample-echo-go.md` lists
what it does with each.
