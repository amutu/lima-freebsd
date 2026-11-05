// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package bhyve

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/lima-vm/lima/v2/pkg/limatype"
)

// newTestConfig builds a Config whose instance directory contains a disk and a
// cidata ISO, so that Cmdline's existence checks pass.
func newTestConfig(t *testing.T, arch limatype.Arch, mountType limatype.MountType, mounts []limatype.Mount) Config {
	t.Helper()
	dir := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "disk"), []byte{}, 0o644))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, "cidata.iso"), []byte{}, 0o644))

	cpus := 4
	memory := "4GiB"
	disk := "100GiB"
	y := &limatype.LimaYAML{
		Arch:      &arch,
		CPUs:      &cpus,
		Memory:    &memory,
		Disk:      &disk,
		MountType: &mountType,
		Mounts:    mounts,
	}
	return Config{
		Name:         "default",
		InstanceDir:  dir,
		LimaYAML:     y,
		SSHLocalPort: 60022,
	}
}

func fakeFirmware(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "BHYVE_UEFI.fd")
	assert.NilError(t, os.WriteFile(f, []byte{}, 0o644))
	return f
}

func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	assert.Assert(t, i >= 0 && i+1 < len(args), "flag %q not found in %v", flag, args)
	return args[i+1]
}

func hasArg(args []string, want string) bool {
	return slices.Contains(args, want)
}

func TestCmdlineAmd64(t *testing.T) {
	fw := fakeFirmware(t)
	cfg := newTestConfig(t, limatype.X8664, limatype.REVSSHFS, nil)
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}

	exe, args, err := Cmdline(cfg, "freebsd", "amd64")
	assert.NilError(t, err)
	assert.Equal(t, exe, "/usr/sbin/bhyve")

	// amd64 uses the LPC bridge plus -l for bootrom and console.
	assert.Assert(t, hasArg(args, "-s"))
	assert.Assert(t, hasArg(args, "-A"), "amd64 must pass -A: %v", args)
	assert.Assert(t, hasArg(args, "-H"), "amd64 must pass -H: %v", args)
	assert.Assert(t, hasArg(args, "-P"), "amd64 must pass -P: %v", args)
	assert.Assert(t, hasArg(args, "-D"), "must pass -D: %v", args)
	assert.Assert(t, hasArg(args, "31,lpc"), "amd64 must configure the LPC bridge: %v", args)
	assert.Assert(t, hasArg(args, "bootrom,"+fw), "amd64 must use -l bootrom: %v", args)
	assert.Assert(t, hasArg(args, "com1,stdio"), "amd64 must use -l com1: %v", args)
	assert.Assert(t, !hasArg(args, "-o"), "amd64 must not use -o: %v", args)

	// Devices.
	assert.Equal(t, argAfter(t, args, "-c"), "4")
	assert.Equal(t, argAfter(t, args, "-m"), "4096M")
	assert.Assert(t, hasArg(args, "0,hostbridge"), args)
	assert.Assert(t, hasArg(args, "1,virtio-blk,"+filepath.Join(cfg.InstanceDir, "disk")), args)
	assert.Assert(t, hasArg(args, "2,ahci-cd,"+filepath.Join(cfg.InstanceDir, "cidata.iso")), args)
	assert.Assert(t, hasArg(args, "3,virtio-net,slirp,hostfwd=tcp::60022-:22"), args)
	assert.Assert(t, hasArg(args, "4,virtio-rnd"), args)

	// The last argument is the VM name.
	assert.Equal(t, args[len(args)-1], VMName(cfg.Name, cfg.InstanceDir))
}

func TestCmdlineArm64(t *testing.T) {
	fw := fakeFirmware(t)
	cfg := newTestConfig(t, limatype.AARCH64, limatype.REVSSHFS, nil)
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}

	_, args, err := Cmdline(cfg, "freebsd", "arm64")
	assert.NilError(t, err)

	// arm64 rejects -A/-H/-P and has no -l at all.
	for _, bad := range []string{"-A", "-H", "-P", "-l"} {
		assert.Assert(t, !hasArg(args, bad), "arm64 must not pass %q: %v", bad, args)
	}
	// arm64 configures console and bootrom through -o.
	assert.Assert(t, hasArg(args, "bootrom="+fw), "%v", args)
	assert.Assert(t, hasArg(args, "console=stdio"), "%v", args)
	// ...and has no LPC bridge.
	for _, a := range args {
		assert.Assert(t, !strings.HasSuffix(a, ",lpc"), "arm64 must not configure an LPC bridge: %v", args)
	}
	assert.Assert(t, hasArg(args, "-D"), "%v", args)
	assert.Assert(t, hasArg(args, "3,virtio-net,slirp,hostfwd=tcp::60022-:22"), "%v", args)
}

func TestCmdlineNinePMounts(t *testing.T) {
	fw := fakeFirmware(t)
	writable := true
	readOnly := false
	mp1, mp2 := "/mnt/one", "/mnt/two"
	cfg := newTestConfig(t, limatype.X8664, limatype.NINEP, []limatype.Mount{
		{Location: "/host/one", MountPoint: &mp1, Writable: &writable},
		{Location: "/host/two", MountPoint: &mp2, Writable: &readOnly},
	})
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}

	_, args, err := Cmdline(cfg, "freebsd", "amd64")
	assert.NilError(t, err)

	// Slots 5 and 6, with `ro` only on the read-only share.
	var nineP []string
	for _, a := range args {
		if strings.Contains(a, ",virtio-9p,") {
			nineP = append(nineP, a)
		}
	}
	assert.Equal(t, len(nineP), 2, "want 2 virtio-9p devices, got %v", nineP)
	assert.Assert(t, strings.HasPrefix(nineP[0], "5,virtio-9p,"), nineP[0])
	assert.Assert(t, !strings.HasSuffix(nineP[0], ",ro"), "writable share must not be ro: %s", nineP[0])
	assert.Assert(t, strings.HasPrefix(nineP[1], "6,virtio-9p,"), nineP[1])
	assert.Assert(t, strings.HasSuffix(nineP[1], ",ro"), "read-only share must be ro: %s", nineP[1])
}

func TestCmdlineNoNinePWhenRevSSHFS(t *testing.T) {
	fw := fakeFirmware(t)
	mp := "/mnt/one"
	cfg := newTestConfig(t, limatype.X8664, limatype.REVSSHFS, []limatype.Mount{
		{Location: "/host/one", MountPoint: &mp},
	})
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}

	_, args, err := Cmdline(cfg, "freebsd", "amd64")
	assert.NilError(t, err)
	for _, a := range args {
		assert.Assert(t, !strings.Contains(a, "virtio-9p"), "reverse-sshfs must not add 9p devices: %v", args)
	}
}

func TestCmdlineRejectsForeignArch(t *testing.T) {
	fw := fakeFirmware(t)
	cfg := newTestConfig(t, limatype.X8664, limatype.REVSSHFS, nil)
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}

	_, _, err := Cmdline(cfg, "freebsd", "arm64")
	assert.ErrorContains(t, err, "cannot run")
	assert.ErrorContains(t, err, "vmType: qemu")
}

func TestCmdlineRejectsInvalidSSHPort(t *testing.T) {
	fw := fakeFirmware(t)
	cfg := newTestConfig(t, limatype.X8664, limatype.REVSSHFS, nil)
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}
	cfg.SSHLocalPort = 0

	_, _, err := Cmdline(cfg, "freebsd", "amd64")
	assert.ErrorContains(t, err, "SSH local port")
}

func TestCmdlineRejectsMissingDisk(t *testing.T) {
	fw := fakeFirmware(t)
	cfg := newTestConfig(t, limatype.X8664, limatype.REVSSHFS, nil)
	cfg.LimaYAML.VMOpts = limatype.VMOpts{limatype.BHYVE: map[string]any{"firmware": fw}}
	assert.NilError(t, os.Remove(filepath.Join(cfg.InstanceDir, "disk")))

	_, _, err := Cmdline(cfg, "freebsd", "amd64")
	assert.ErrorContains(t, err, "disk")
}

func TestVMName(t *testing.T) {
	// Fits without truncation.
	assert.Equal(t, VMName("default", "/home/x/.lima/default"), "default-"+shortHash("/home/x/.lima/default", 8))

	// Illegal characters are replaced.
	name := VMName("my vm/with:chars", "/dir")
	assert.Assert(t, !strings.ContainsAny(name, " /:"), name)

	// Long names are truncated to bhyve's 31-byte limit but stay unique.
	long := strings.Repeat("a", 64)
	a := VMName(long, "/dir/a")
	b := VMName(long, "/dir/b")
	assert.Assert(t, len(a) <= 31, "len=%d: %s", len(a), a)
	assert.Assert(t, len(b) <= 31, "len=%d: %s", len(b), b)
	assert.Assert(t, a != b, "distinct instance dirs must yield distinct VM names: %s", a)

	// A degenerate name still yields something usable.
	assert.Assert(t, VMName("", "/dir") != "", "empty name must not produce an empty VM name")
}

func TestInterpretExitCodes(t *testing.T) {
	// 0=reboot, 1=poweroff, 2=halt are normal terminations.
	for _, code := range []int{0, 1, 2} {
		assert.NilError(t, interpretExitCode(code), "exit code %d must be treated as success", code)
	}
	assert.ErrorContains(t, interpretExitCode(3), "triple fault")
	assert.ErrorContains(t, interpretExitCode(4), "exited with an error")
	assert.ErrorContains(t, interpretExitCode(5), "suspended")
	assert.ErrorContains(t, interpretExitCode(42), "unexpected code 42")
}

func TestValidateConfig(t *testing.T) {
	// validateConfig checks the guest arch against the host arch, so the tests
	// have to use the host's own architecture for the positive cases.
	native := nativeArchName(runtime.GOARCH)
	foreign := limatype.X8664
	if native == limatype.X8664 {
		foreign = limatype.AARCH64
	}
	revsshfs, ninep, virtiofs := limatype.REVSSHFS, limatype.NINEP, limatype.VIRTIOFS
	memory := "4GiB"
	small := "64MiB"

	ok := &limatype.LimaYAML{Arch: &native, MountType: &revsshfs, Memory: &memory}
	assert.NilError(t, validateConfig(ok))

	withNineP := &limatype.LimaYAML{Arch: &native, MountType: &ninep, Memory: &memory}
	assert.NilError(t, validateConfig(withNineP))

	badMount := &limatype.LimaYAML{Arch: &native, MountType: &virtiofs, Memory: &memory}
	assert.ErrorContains(t, validateConfig(badMount), "mountType")

	tooSmall := &limatype.LimaYAML{Arch: &native, MountType: &revsshfs, Memory: &small}
	assert.ErrorContains(t, validateConfig(tooSmall), "128MiB")

	foreignArch := &limatype.LimaYAML{Arch: &foreign, MountType: &revsshfs, Memory: &memory}
	assert.ErrorContains(t, validateConfig(foreignArch), "vmType: qemu")
}

func TestFillConfigDefaultMountType(t *testing.T) {
	cfg := &limatype.LimaYAML{}
	assert.NilError(t, fillConfig(cfg))
	assert.Assert(t, cfg.MountType != nil)
	assert.Equal(t, *cfg.MountType, limatype.REVSSHFS)
	assert.Assert(t, cfg.VMType != nil)
	assert.Equal(t, *cfg.VMType, limatype.BHYVE)

	// An explicit mountType is preserved.
	ninep := limatype.NINEP
	cfg2 := &limatype.LimaYAML{MountType: &ninep}
	assert.NilError(t, fillConfig(cfg2))
	assert.Equal(t, *cfg2.MountType, limatype.NINEP)
}

func TestSanitizeVMName(t *testing.T) {
	assert.Equal(t, sanitizeVMName("ok-name_1.2"), "ok-name_1.2")
	assert.Equal(t, sanitizeVMName("a b/c"), "a-b-c")
	assert.Equal(t, sanitizeVMName("--"), "lima")
	assert.Equal(t, sanitizeVMName(""), "lima")
}
