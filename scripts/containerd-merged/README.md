# containerd-merged

`main.go` is the source of record for astrokube's merged **containerd + ctr**
multi-call binary. There is intentionally **no `go.mod`/`go.sum` here**: the file
is not built standalone. `../build-containerd-merged.sh` copies the exact
`github.com/containerd/containerd/v2 @ v2.2.3` module out of the Go module cache
into a writable scratch dir, drops this file in as `cmd/astrokube-containerd/`,
and builds it there so the module's own `go.mod`/`go.sum` drive dependency
resolution (matching the previously-verified standalone binaries). Editors will
flag the containerd imports as unresolved here — that's expected.

## Why merge containerd + ctr but not the shim

`containerd`, `ctr`, and `containerd-shim-runc-v2` are all the same Go module, so
linking them separately bakes three copies of the Go runtime + the containerd
library into the zero-C share (~120 MB total).

- **containerd + ctr → one binary** (dispatch on `argv[0]`; `ctr` is a hard link
  to `containerd` on the share). Safe: `ctr` is a gRPC client and never
  initializes the plugin registry, so the daemon's registered-but-unused builtin
  plugins are harmless in `ctr` mode.
- **the shim stays its own binary.** `pkg/shim/shim.go` initializes *every*
  plugin in the global registry with no plugin config, so a shim that also linked
  the daemon's builtins would try to init daemon-only plugins (e.g.
  `imageverifier/bindir`) and panic on their absent config. Upstream ships the
  shim as a separate binary for the same reason.

Result on the share: `containerd`(+`ctr`) ~43 MB + `containerd-shim-runc-v2`
~14 MB ≈ **57 MB**, down from ~120 MB, with no functional change (verified
end-to-end: containerd boots, image import + `ctr run` execute a container via
the runc shim, zero panics).
