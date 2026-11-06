# AGENTS.md

Guidance for AI coding agents working in the Lima repository. It points to the authoritative docs
instead of duplicating them, so it stays in sync with the code.

## This checkout is a FreeBSD port

This tree is Lima plus a FreeBSD host port, not plain upstream `lima-vm/lima`. The port is the
commit `port to freebsd` and adds:

- a new `bhyve` VM driver under `pkg/driver/bhyve/`;
- FreeBSD handling in the QEMU driver (`Accel()` reports `tcg` with a one-time warning, and 9p /
  virtiofs are rejected because the FreeBSD QEMU build has no `-virtfs`);
- `limatype.BHYVE` as a `VMType`, and `DefaultDriver()` returning `bhyve` on FreeBSD;
- `osutil.CheckICMPForwarding` / `WarnIfICMPUnavailable` for the guest-ICMP limitation.

`upstream-lima/` is untracked PR material (patch series, PR descriptions). Do not edit, move, or
commit it as part of a code change.

## AI contribution rules

Read @website/content/en/docs/community/contributing.md and follow its "AI Contribution Rules"
section. In short: a human must review all generated content, commits need `git commit -s` (DCO),
and AI assistance is disclosed with an `Assisted-by:` trailer.

## Build, test, lint

Lima is `github.com/lima-vm/lima/v2` (imports use the `/v2` suffix). The build uses a GNU Makefile;
output goes to `_output/`.

```bash
gmake native       # build limactl + native guestagent + templates (fastest full dev build)
gmake minimal      # build just limactl + native guestagent + default template
go test ./...      # unit tests - never boot VMs
gmake bats         # integration tests (BATS); boots real VMs (needs git submodules)
gmake lint         # editorconfig, golangci-lint, yamllint, ls-lint, shellcheck, ltag, ...
gmake generate     # go generate ./... (protobuf, mocks); CI also runs `gmake check-generated`
```

**On FreeBSD, use `gmake`, not `make`.** `/usr/bin/make` is BSD make and fails immediately on this
Makefile (`.SECONDEXPANSION`). `gmake` comes from `pkg install gmake`. Upstream docs and CI use
`make`; the same targets apply.

Unit tests never execute VMs; anything that boots a VM is a BATS or template test under `hack/`
(for example `./hack/test-templates.sh ./templates/default.yaml`). `gmake bats` first depends on
`native`, so it rebuilds before running. BATS style rules are in
`website/content/en/docs/dev/testing/bats-style.md`.

If `go test` fails with `fork/exec .../pkg.test: permission denied`, the temp directory is mounted
`noexec`; set `TMPDIR` and `GOTMPDIR` to an exec-enabled path (for example `$HOME/.gotmp`).

CI also enforces: `gmake shellcheck`, `gmake shfmt`, `gmake go-licenses`, `NO_COLOR=true gmake
gomodjail`, `gmake gosocialcheck`, `gmake ltag`, `gmake protolint`, editorconfig/yamllint/ls-lint,
and a check that `templates/*.yaml` round-trips through `limactl edit`.

## Where things are

Pointers to the authoritative sources - read these rather than a duplicated copy here.

- Architecture, the three processes (`limactl` / hostagent / guestagent), the on-disk `${LIMA_HOME}`
  layout, and every `LIMA_CIDATA_*` variable:
  [`website/content/en/docs/dev/internals.md`](website/content/en/docs/dev/internals.md).
- Driver interface, internal vs external drivers, registration and call ordering:
  [`website/content/en/docs/dev/drivers.md`](website/content/en/docs/dev/drivers.md).
- Config / data model: `pkg/limatype` (core `LimaYAML` / `Instance` types), `pkg/limayaml`
  (load / default / validate), `pkg/limatmpl` and `pkg/templatestore` (templates).
- Drivers (virtualization backends): `pkg/driver/`; registration via `pkg/registry`.
- Guest provisioning: `pkg/cidata/` builds `cidata.iso` (guestagent binary, boot scripts, and the
  `user-data` file). `user-data` uses the cloud-config YAML format defined by cloud-init, which a
  guest may consume with an implementation other than Python cloud-init.
- Instance lifecycle: `pkg/instance/`.

## Internal drivers follow a fixed pattern

Each internal driver has `pkg/driver/<name>/register.go` guarded by a build tag (for example
`freebsd && !external_bhyve`) that calls `registry.Register`, and a blank import in
`cmd/limactl/main_<name>*.go` that is also build-tag guarded. The Makefile maps
`ADDITIONAL_DRIVERS=qemu|vz|wsl2` to `external_*` build tags for building a driver as a separate
`lima-driver-<name>` executable; there is no external build path for bhyve.

## bhyve driver specifics

`pkg/driver/bhyve/` splits into two files on purpose:

- `bhyve.go` has **no build tag**. It is a pure bhyve command-line builder (`Config`, `Cmdline`,
  `Firmware`, `VMName`, `fillConfig`, `validateConfig`) so it can be unit-tested on any host;
  `bhyve_test.go` also has no build tag.
- `bhyve_driver_freebsd.go` is `//go:build freebsd` and does the real work: prerequisites, `sudo`,
  starting `bhyve`, serial log capture, and `bhyvectl` on stop.

When changing bhyve behavior, put command-line/config logic in `bhyve.go` and add a cross-platform
unit test; put process/root interactions in the FreeBSD-only file. `gmake` targets and `go test
./pkg/driver/bhyve` must still pass on non-FreeBSD hosts.

Other bhyve facts that are easy to get wrong:

- bhyve cannot emulate a foreign architecture. `validateConfig` rejects non-native `arch`; use
  `vmType: qemu` for cross-arch guests.
- VM names live in a global `/dev/vmm` namespace and are capped at 31 bytes, so `VMName` sanitizes
  the instance name and appends a hash of the instance directory.
- Default `mountType` is `9p`; `reverse-sshfs` is opt-in and needs sshfs in the guest. `virtiofs` is rejected.
- Networking mode is resolved by `limayaml.ResolveBhyveNetwork`: `vmOpts.bhyve.networking`
  (`auto`/`slirp`/`bridge`). `auto` selects `slirp,open` on FreeBSD >= 15.1 and the tap-on-bridge
  path on older releases. Bridge mode uses
  `vmOpts.bhyve.bridge`/`network`/`gateway`/`address` (defaults `vm-public`, `172.16.0.0/24`, first
  host, derived address). Slirp mode uses the fixed `10.0.2.0/24`, gateway `10.0.2.2`, guest
  `10.0.2.15`, and a `hostfwd` for SSH. The tap and bridge names are recorded in
  `<instance>/bhyve.tap` and removed on stop/delete.
- `Start` maps bhyve exit codes (0 reboot, 1 poweroff, 2 halt, 3 triple fault, 4 error, 5 suspend);
  normal guest terminations are reported as success.
- `Stop` cannot send ACPI power events; it runs `bhyvectl --destroy --vm=<name>` and waits up to
  30s before killing the process. A guest-initiated reboot ends the instance.
- Snapshots and the graphical console are unimplemented and return `ErrNotSupported`.
- `Info()` advertises supported image formats, so Lima converts non-raw disks via `qemu-img` before
  bhyve sees them.

## FreeBSD host gotchas

- Bridge mode needs the configured bridge with the gateway address and host NAT (pf) for guest
  Internet access; defaults are `vm-public` and `172.16.0.0/24`. Slirp mode (FreeBSD >= 15.1)
  needs `libslirp` and uses the fixed `10.0.2.0/24` network. `bhyve-firmware` (amd64 UEFI ROM) or
  `u-boot-bhyve-arm64` (arm64) is required, plus optional `qemu` for `qemu-img` conversion.
- `bhyve`, `ifconfig`, `bhyvectl`, and `kldload` run through `sudo -n`; passwordless sudo is
  required. Membership in the `vmm` group is not enough because the tap device is root-owned.
- QEMU on FreeBSD has no hardware accelerator and falls back to TCG; the warning fires once per
  process. The QEMU slirp path still cannot forward guest ICMP (`osutil.WarnIfICMPUnavailable`);
  bhyve tap/NAT does forward ICMP.
- QEMU defaults to `9p` on non-Windows hosts, including FreeBSD when the QEMU build has `virtfs`;
  Windows falls back to `reverse-sshfs`. Recent FreeBSD `emulators/qemu` packages enable virtfs.
- bhyve `-A/-H/-P` flags are amd64-only and are rejected on arm64; the console wiring differs
  between `-s N,lpc -l ...` (amd64) and `-o ...` (arm64).

## Conventions

- New Go, shell, Dockerfile, and Makefile files need an SPDX header (`ltag` enforces this in CI;
  other file types, including markdown, are exempt).
- Keep the `gomodjail` / `gosocialcheck` annotations in `go.mod`. `gmake gomodjail` statically
  verifies that the modules annotated `gomodjail:confined` cannot reach a denied capability
  (filesystem, network, process execution, raw syscalls, ...); when a dependency bump makes one
  of them reach a capability, `gmake gomodjail-fix` downgrades that annotation to
  `gomodjail:unconfined` instead of confining it by hand.
- Prefer the existing `logrus` logging and return errors with `%w` / `%#q` rather than composing
  messages by hand; the bhyve/QEMU code relies on that style for actionable hints.
- Run `gofmt` and `go vet`; CI runs golangci-lint on Linux, macOS, and Windows, so keep
  platform-specific code behind build tags.
