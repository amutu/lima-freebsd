// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

// Package bhyve implements the "bhyve" VM driver for FreeBSD hosts.
//
// The command line builder in this file is deliberately free of platform build
// tags so that it can be unit-tested on any host OS. Everything that actually
// executes bhyve lives in bhyve_driver_freebsd.go.
package bhyve

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/docker/go-units"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
	"github.com/lima-vm/lima/v2/pkg/limayaml"
)

// Config holds everything needed to build a bhyve command line.
type Config struct {
	Name         string
	InstanceDir  string
	LimaYAML     *limatype.LimaYAML
	SSHLocalPort int
}

// Guest SSHD port. bhyve's slirp backend forwards host:SSHLocalPort to this.
const guestSSHPort = 22

// PCI slots. Slot 0 is the host bridge and slot 31 is reserved for the LPC
// bridge on amd64, so the devices below live in between.
const (
	slotHostBridge = 0
	slotDisk       = 1
	slotCIData     = 2
	slotNet        = 3
	slotRnd        = 4
	slotNinePFirst = 5
	slotLPC        = 31
)

// UEFI firmware used on amd64, from sysutils/bhyve-firmware.
var amd64FirmwareCandidates = []string{
	"/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
	"/usr/local/share/uefi-firmware/BHYVE_UEFI_CODE.fd",
}

// U-Boot firmware used on arm64, from u-boot-bhyve-arm64.
var arm64FirmwareCandidates = []string{
	"/usr/local/share/u-boot/u-boot-bhyve-arm64/u-boot.bin",
}

// Firmware returns the boot ROM path for the given architecture.
// An explicit override can be given with `vmOpts.bhyve.firmware`.
func Firmware(arch limatype.Arch, override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("firmware %#q is not accessible: %w", override, err)
		}
		return override, nil
	}
	var candidates []string
	var hint string
	switch arch {
	case limatype.X8664:
		candidates = amd64FirmwareCandidates
		hint = "pkg install bhyve-firmware"
	case limatype.AARCH64:
		candidates = arm64FirmwareCandidates
		hint = "pkg install u-boot-bhyve-arm64"
	default:
		return "", fmt.Errorf("bhyve does not support architecture %#q", arch)
	}
	for _, f := range candidates {
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("could not find the bhyve boot firmware for %#q (tried %v; hint: %s)",
		arch, candidates, hint)
}

// VMName returns the bhyve VM name for a Lima instance.
//
// bhyve VM names live in a global namespace under /dev/vmm and are limited to
// VM_MAX_NAMELEN-1 (31) bytes, so the instance name cannot be used verbatim.
// A short hash of the instance directory keeps distinct instances apart even
// when their names collide after truncation.
func VMName(instanceName, instanceDir string) string {
	const maxLen = 31
	sanitized := sanitizeVMName(instanceName)
	suffix := "-" + shortHash(instanceDir, 8)
	if len(sanitized)+len(suffix) > maxLen {
		sanitized = sanitized[:maxLen-len(suffix)]
	}
	return sanitized + suffix
}

// sanitizeVMName keeps [A-Za-z0-9_.-] and replaces everything else with '-'.
func sanitizeVMName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "lima"
	}
	return out
}

// shortHash returns the first n hex characters of the FNV-1a 64-bit hash of s.
func shortHash(s string, n int) string {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	hex := strconv.FormatUint(h, 16)
	for len(hex) < n {
		hex = "0" + hex
	}
	if len(hex) > n {
		hex = hex[:n]
	}
	return hex
}

// Cmdline returns the bhyve executable and its arguments.
func Cmdline(cfg Config, goos string, goarch string) (string, []string, error) {
	if cfg.LimaYAML == nil {
		return "", nil, errors.New("nil LimaYAML")
	}
	y := cfg.LimaYAML
	if y.Arch == nil {
		return "", nil, errors.New("nil arch")
	}
	arch := *y.Arch

	// bhyve cannot emulate a foreign architecture.
	if !isNativeArch(arch, goarch) {
		return "", nil, fmt.Errorf("bhyve cannot run %#q guests on a %#q host (no cross-architecture emulation); use vmType: qemu",
			arch, goarch)
	}
	if y.Memory == nil || y.CPUs == nil {
		return "", nil, errors.New("field `memory` and `cpus` must be set")
	}

	memBytes, err := units.RAMInBytes(*y.Memory)
	if err != nil {
		return "", nil, fmt.Errorf("invalid memory %#q: %w", *y.Memory, err)
	}
	if memBytes < 128*1024*1024 {
		return "", nil, fmt.Errorf("bhyve requires at least 128MiB of memory, got %#q", *y.Memory)
	}

	diskPath := filepath.Join(cfg.InstanceDir, filenames.Disk)
	if _, err := os.Stat(diskPath); err != nil {
		return "", nil, fmt.Errorf("disk %#q is not accessible: %w", diskPath, err)
	}

	vmName := VMName(cfg.Name, cfg.InstanceDir)

	var args []string

	// amd64-only flags. arm64's bhyve rejects -A/-H/-P ("illegal option").
	if goarch == "amd64" {
		args = append(args,
			"-A", // xAPIC mode
			"-H", // yield vCPU on HLT
			"-P", // force vCPU exit on PAUSE
		)
	}
	// -D: destroy the VM when the guest powers off, so bhyve exits by itself.
	args = append(args, "-D")

	args = append(args,
		"-c", strconv.Itoa(*y.CPUs),
		"-m", fmt.Sprintf("%dM", memBytes/(1024*1024)),
		"-s", fmt.Sprintf("%d,hostbridge", slotHostBridge),
	)

	firmware, err := Firmware(arch, FirmwareOverride(y))
	if err != nil {
		return "", nil, err
	}

	// Disk. bhyve only reads raw images; Lima converts to raw beforehand via
	// Info().Features.SupportedImageFormats.
	args = append(args, "-s", fmt.Sprintf("%d,virtio-blk,%s", slotDisk, diskPath))

	// cloud-init / ignition seed, attached as a CD-ROM.
	cidataPath := filepath.Join(cfg.InstanceDir, filenames.CIDataISO)
	if _, err := os.Stat(cidataPath); err == nil {
		args = append(args, "-s", fmt.Sprintf("%d,ahci-cd,%s", slotCIData, cidataPath))
	}

	// Networking: libslirp gives us user-mode NAT plus a static host->guest
	// forward for SSH, without any host network configuration and without root.
	// Everything else (dynamic port forwarding, the guest agent) rides on SSH.
	if cfg.SSHLocalPort <= 0 {
		return "", nil, fmt.Errorf("invalid SSH local port %d", cfg.SSHLocalPort)
	}
	args = append(args, "-s", fmt.Sprintf("%d,virtio-net,slirp,hostfwd=tcp::%d-:%d",
		slotNet, cfg.SSHLocalPort, guestSSHPort))

	// Entropy.
	args = append(args, "-s", fmt.Sprintf("%d,virtio-rnd", slotRnd))

	// 9p shares, when mountType is 9p. bhyve's virtio-9p is available on both
	// amd64 and arm64 (verified with `bhyve -s help`).
	if y.MountType != nil && *y.MountType == limatype.NINEP {
		slot := slotNinePFirst
		for _, f := range y.Mounts {
			tag := limayaml.MountTag(f.Location, *f.MountPoint)
			conf := fmt.Sprintf("%d,virtio-9p,%s=%s", slot, tag, f.Location)
			if f.Writable == nil || !*f.Writable {
				conf += ",ro"
			}
			args = append(args, "-s", conf)
			slot++
		}
	}

	// Console and boot ROM. The two differ between amd64 (-l / LPC) and arm64
	// (-o, no LPC bridge). `-l` is rejected outright on arm64.
	switch goarch {
	case "amd64":
		args = append(args,
			"-s", fmt.Sprintf("%d,lpc", slotLPC),
			"-l", "bootrom,"+firmware,
			"-l", "com1,stdio",
		)
	case "arm64":
		args = append(args,
			"-o", "bootrom="+firmware,
			"-o", "console=stdio",
		)
	default:
		return "", nil, fmt.Errorf("bhyve is not supported on %#q", goarch)
	}

	args = append(args, vmName)

	exe := "/usr/sbin/bhyve"
	if goos != "freebsd" {
		// Only reached from unit tests; keeps the builder honest about its host.
		exe = "bhyve"
	}
	return exe, args, nil
}

func isNativeArch(arch limatype.Arch, goarch string) bool {
	switch arch {
	case limatype.X8664:
		return goarch == "amd64"
	case limatype.AARCH64:
		return goarch == "arm64"
	default:
		return false
	}
}

// BhyveOpts mirrors vmOpts.bhyve.
type BhyveOpts struct {
	// Firmware overrides the auto-detected boot ROM path.
	Firmware string `yaml:"firmware,omitempty" json:"firmware,omitempty"`
}

// FirmwareOverride extracts vmOpts.bhyve.firmware, if any.
func FirmwareOverride(y *limatype.LimaYAML) string {
	raw, ok := y.VMOpts[limatype.BHYVE]
	if !ok || raw == nil {
		return ""
	}
	var opts BhyveOpts
	if err := limayaml.Convert(raw, &opts, "vmOpts.bhyve"); err != nil {
		return ""
	}
	return opts.Firmware
}

// fillConfig applies bhyve-specific defaults.
func fillConfig(cfg *limatype.LimaYAML) error {
	if cfg == nil {
		return errors.New("configuration is nil")
	}
	cfg.VMType = new(limatype.BHYVE)

	if cfg.MountType == nil || *cfg.MountType == "" || *cfg.MountType == "default" {
		// reverse-sshfs is the conservative default: it needs nothing from bhyve
		// beyond a working network stack, whereas virtio-9p has to be verified
		// against the guest kernel. Users can opt in with `mountType: 9p`.
		cfg.MountType = new(limatype.REVSSHFS)
	}
	return nil
}

func validateConfig(cfg *limatype.LimaYAML) error {
	if cfg == nil {
		return errors.New("configuration is nil")
	}
	if cfg.Arch == nil {
		return errors.New("field `arch` is required")
	}
	if !isNativeArch(*cfg.Arch, runtime.GOARCH) {
		return fmt.Errorf("field `arch` must be %#q for vmType: bhyve on a %#q host (bhyve cannot emulate a foreign architecture); use vmType: qemu instead",
			nativeArchName(runtime.GOARCH), runtime.GOARCH)
	}
	if cfg.MountType != nil {
		switch *cfg.MountType {
		case limatype.REVSSHFS, limatype.NINEP:
			// ok
		default:
			return fmt.Errorf("field `mountType` must be %#q or %#q for vmType: bhyve, got %#q",
				limatype.REVSSHFS, limatype.NINEP, *cfg.MountType)
		}
	}
	if cfg.Memory != nil {
		memBytes, err := units.RAMInBytes(*cfg.Memory)
		if err != nil {
			return fmt.Errorf("invalid memory %#q: %w", *cfg.Memory, err)
		}
		if memBytes < 128*1024*1024 {
			return fmt.Errorf("vmType: bhyve requires at least 128MiB of memory, got %#q", *cfg.Memory)
		}
	}
	return nil
}

func nativeArchName(goarch string) limatype.Arch {
	if goarch == "amd64" {
		return limatype.X8664
	}
	return limatype.AARCH64
}

// interpretExit maps the exit status of a bhyve process to an error meaningful
// to Lima.
func interpretExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	return interpretExitCode(exitErr.ExitCode())
}

// interpretExitCode maps bhyve's documented exit codes.
//
// bhyve exits with: 0 = guest reboot, 1 = guest poweroff, 2 = guest halt,
// 3 = triple fault (amd64 only), 4 = bhyve error, 5 = suspend.
// Poweroff, halt and reboot are normal terminations, so they are reported as nil.
func interpretExitCode(code int) error {
	switch code {
	case 0, 1, 2:
		return nil
	case 3:
		return errors.New("bhyve: guest triple fault")
	case 4:
		return errors.New("bhyve exited with an error")
	case 5:
		return errors.New("bhyve: guest suspended")
	default:
		return fmt.Errorf("bhyve exited with unexpected code %d", code)
	}
}
