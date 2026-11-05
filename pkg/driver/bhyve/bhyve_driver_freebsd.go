// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd

package bhyve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-units"
	rawImage "github.com/lima-vm/go-qcow2reader/image/raw"
	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/v2/pkg/driver"
	"github.com/lima-vm/lima/v2/pkg/iso9660util"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
)

// ErrNotSupported is returned by operations the bhyve driver does not implement.
var ErrNotSupported = errors.New("not supported by the bhyve driver")

// LimaBhyveDriver implements driver.Driver on top of FreeBSD's bhyve(8).
type LimaBhyveDriver struct {
	Instance     *limatype.Instance
	SSHLocalPort int

	cmd     *exec.Cmd
	waitCh  chan error
	vmName  string
	stopMux sync.Mutex
}

var _ driver.Driver = (*LimaBhyveDriver)(nil)

func New() *LimaBhyveDriver {
	return &LimaBhyveDriver{}
}

func (l *LimaBhyveDriver) Configure(_ context.Context, inst *limatype.Instance) (*driver.ConfiguredDriver, error) {
	if err := fillConfig(inst.Config); err != nil {
		return nil, err
	}
	l.Instance = inst
	l.SSHLocalPort = inst.SSHLocalPort
	return &driver.ConfiguredDriver{Driver: l}, nil
}

// fillConfig and validateConfig live in bhyve.go so that they can be unit
// tested on any host OS.

func (l *LimaBhyveDriver) Validate(_ context.Context) error {
	if l.Instance == nil || l.Instance.Config == nil {
		return errors.New("bhyve driver is not configured")
	}
	return validateConfig(l.Instance.Config)
}

// CreateDisk materialises "<dir>/disk" from the downloaded image.
//
// bhyve reads raw images only, so Lima has already converted the image to raw
// by the time we get here (see Info().Features.SupportedImageFormats).
func (l *LimaBhyveDriver) CreateDisk(ctx context.Context) error {
	diskPath := filepath.Join(l.Instance.Dir, filenames.Disk)
	if _, err := os.Stat(diskPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	imagePath := filepath.Join(l.Instance.Dir, filenames.Image)
	isISO, err := iso9660util.IsISO9660(imagePath)
	if err != nil {
		return err
	}
	if isISO {
		isoPath := filepath.Join(l.Instance.Dir, filenames.ISO)
		if err := os.Rename(imagePath, isoPath); err != nil {
			return err
		}
		diskSize, err := units.RAMInBytes(*l.Instance.Config.Disk)
		if err != nil {
			return fmt.Errorf("invalid disk size %#q: %w", *l.Instance.Config.Disk, err)
		}
		f, err := os.Create(diskPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Truncate(diskSize)
	}
	return os.Rename(imagePath, diskPath)
}

func (l *LimaBhyveDriver) BootScripts(_ context.Context) (map[string][]byte, error) {
	return nil, nil
}

// Start boots the VM and returns a channel that receives the process exit status.
func (l *LimaBhyveDriver) Start(ctx context.Context) (chan error, error) {
	if err := checkPrerequisites(ctx); err != nil {
		return nil, err
	}

	cfg := Config{
		Name:         l.Instance.Name,
		InstanceDir:  l.Instance.Dir,
		LimaYAML:     l.Instance.Config,
		SSHLocalPort: l.SSHLocalPort,
	}
	exe, args, err := Cmdline(cfg, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	l.vmName = VMName(l.Instance.Name, l.Instance.Dir)

	if err := ensureVMM(ctx); err != nil {
		return nil, err
	}

	// bhyve itself must run as root unless the user is in the "vmm" group.
	argv := append([]string{exe}, args...)
	if needsSudo() {
		argv = append([]string{"sudo", "-n"}, argv...)
	}

	serialLog := filepath.Join(l.Instance.Dir, filenames.SerialLog)
	logFile, err := os.Create(serialLog)
	if err != nil {
		return nil, err
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		logFile.Close()
		return nil, err
	}

	logrus.Infof("Starting bhyve: %v", argv)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		devNull.Close()
		return nil, fmt.Errorf("failed to start bhyve: %w", err)
	}

	waitCh := make(chan error, 1)
	l.cmd = cmd
	l.waitCh = waitCh
	go func() {
		err := cmd.Wait()
		logFile.Close()
		devNull.Close()
		waitCh <- exitError(err, serialLog)
		// Close so that a later Stop() does not block waiting for a status that
		// was already delivered to (and consumed by) the host agent.
		close(waitCh)
	}()

	return waitCh, nil
}

// exitError maps the bhyve exit status to an error, appending the tail of the
// serial log when bhyve itself failed, so that the reason is visible in the
// host agent log instead of being buried in serial.log.
func exitError(err error, serialLogPath string) error {
	interpreted := interpretExit(err)
	if interpreted == nil {
		return nil
	}
	detail := lastLine(serialLogPath)
	if detail == "" {
		return interpreted
	}
	if strings.Contains(detail, "vm_openf") {
		// bhyve cannot create the VM at all, which almost always means the CPU
		// does not expose virtualization (e.g. running inside another VM
		// without nested virtualization).
		return fmt.Errorf("%w: %s (hint: bhyve could not create the VM, so the host CPU does not provide "+
			"virtualization here; nested virtualization is usually unavailable, check `dmesg | grep vmm`)",
			interpreted, detail)
	}
	return fmt.Errorf("%w: %s", interpreted, detail)
}

// lastLine returns the last non-empty line of the file, or "" if unavailable.
func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

func (l *LimaBhyveDriver) Stop(ctx context.Context) error {
	l.stopMux.Lock()
	defer l.stopMux.Unlock()

	vmName := l.vmName
	if vmName == "" && l.Instance != nil {
		vmName = VMName(l.Instance.Name, l.Instance.Dir)
	}
	if vmName == "" {
		return errors.New("bhyve driver is not configured")
	}

	// bhyvectl --destroy makes bhyve exit; "could not be opened" means it is
	// already gone, which is what we want.
	if out, err := runBhyvectl(ctx, "--destroy", "--vm="+vmName); err != nil {
		if !isNotRunning(out) {
			return fmt.Errorf("failed to destroy the bhyve VM %#q: %w (%s)", vmName, err, strings.TrimSpace(out))
		}
	}

	if l.waitCh == nil {
		return nil
	}
	select {
	case <-l.waitCh:
	case <-time.After(30 * time.Second):
		if l.cmd != nil && l.cmd.Process != nil {
			_ = l.cmd.Process.Kill()
		}
		return fmt.Errorf("bhyve VM %#q did not exit within 30s of being destroyed", vmName)
	}
	return nil
}

func isNotRunning(bhyvectlOutput string) bool {
	return strings.Contains(bhyvectlOutput, "could not be opened") ||
		strings.Contains(bhyvectlOutput, "No such file or directory")
}

func (l *LimaBhyveDriver) Delete(ctx context.Context) error {
	if l.Instance == nil {
		return nil
	}
	vmName := VMName(l.Instance.Name, l.Instance.Dir)
	_, _ = runBhyvectl(ctx, "--destroy", "--vm="+vmName)
	return nil
}

func (l *LimaBhyveDriver) InspectStatus(_ context.Context, inst *limatype.Instance) string {
	if inst == nil {
		return ""
	}
	name := VMName(inst.Name, inst.Dir)
	if _, err := os.Stat(filepath.Join("/dev/vmm", name)); err == nil {
		return "Running"
	}
	return ""
}

func (l *LimaBhyveDriver) Info(_ context.Context) driver.Info {
	info := driver.Info{
		Name: "bhyve",
		Features: driver.DriverFeatures{
			// bhyve reads raw images only; Lima converts anything else to raw.
			SupportedImageFormats: []string{string(rawImage.Type)},
			CanRunGUI:             false,
			DynamicSSHAddress:     false,
			StaticSSHPort:         false,
			SkipSocketForwarding:  false,
			NoCloudInit:           false,
		},
	}
	if l.Instance != nil && l.Instance.Dir != "" {
		info.InstanceDir = l.Instance.Dir
	}
	// No vsock and no virtio-serial port: the host agent reaches the guest
	// agent over SSH, like the QEMU driver does on macOS/Linux.
	return info
}

func (l *LimaBhyveDriver) SSHAddress(_ context.Context) (string, error) {
	return "127.0.0.1", nil
}

func (l *LimaBhyveDriver) AdditionalSetupForSSH(_ context.Context) error {
	return nil
}

func (l *LimaBhyveDriver) ForwardGuestAgent(_ context.Context) bool {
	return true
}

func (l *LimaBhyveDriver) GuestAgentConn(_ context.Context) (net.Conn, string, error) {
	return nil, "", nil
}

func (l *LimaBhyveDriver) Create(_ context.Context) error {
	return nil
}

func (l *LimaBhyveDriver) RunGUI(_ context.Context) error {
	return fmt.Errorf("%w: bhyve does not implement a graphical console; it uses the serial console", ErrNotSupported)
}

func (l *LimaBhyveDriver) ChangeDisplayPassword(_ context.Context, _ string) error {
	return fmt.Errorf("%w: bhyve has no display password", ErrNotSupported)
}

func (l *LimaBhyveDriver) DisplayConnection(_ context.Context) (string, error) {
	return "", fmt.Errorf("%w: bhyve has no display", ErrNotSupported)
}

func (l *LimaBhyveDriver) CreateSnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("%w: use ZFS snapshots of the instance directory instead", ErrNotSupported)
}

func (l *LimaBhyveDriver) ApplySnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("%w: use ZFS snapshots of the instance directory instead", ErrNotSupported)
}

func (l *LimaBhyveDriver) DeleteSnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("%w: use ZFS snapshots of the instance directory instead", ErrNotSupported)
}

func (l *LimaBhyveDriver) ListSnapshots(_ context.Context) ([]driver.Snapshot, error) {
	return nil, fmt.Errorf("%w: use ZFS snapshots of the instance directory instead", ErrNotSupported)
}

// ---------------------------------------------------------------------------
// host-side helpers

const (
	bhyveExe     = "/usr/sbin/bhyve"
	bhyvectlExe  = "/usr/sbin/bhyvectl"
	libslirpPath = "/usr/local/lib/libslirp.so"
)

// checkPrerequisites verifies that everything required to boot a bhyve VM is installed.
func checkPrerequisites(ctx context.Context) error {
	for _, exe := range []string{bhyveExe, bhyvectlExe} {
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("%#q is not available: %w (bhyve is part of the FreeBSD base system)", exe, err)
		}
	}
	// bhyve dlopen()s libslirp for the user-mode network backend.
	if _, err := os.Stat(libslirpPath); err != nil {
		return fmt.Errorf("libslirp is required by the bhyve slirp network backend but %#q is missing (hint: pkg install libslirp): %w",
			libslirpPath, err)
	}
	return nil
}

// ensureVMM loads the vmm(4) kernel module if it is not loaded yet.
func ensureVMM(ctx context.Context) error {
	if _, err := os.Stat("/dev/vmm"); err == nil {
		return nil
	}
	// kldload always needs root; membership of the "vmm" group is enough to
	// *run* bhyve, but not to load kernel modules.
	args := []string{"kldload", "vmm"}
	if os.Geteuid() != 0 {
		args = append([]string{"sudo", "-n"}, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already loaded") {
			return nil
		}
		return fmt.Errorf("failed to load the vmm kernel module: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat("/dev/vmm"); err != nil {
		return fmt.Errorf("vmm loaded but /dev/vmm is still missing; the CPU or the hypervisor may not support "+
			"virtualization (nested virtualization is usually unavailable): %w", err)
	}
	return nil
}

func runBhyvectl(ctx context.Context, args ...string) (string, error) {
	argv := append([]string{bhyvectlExe}, args...)
	if needsSudo() {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// needsSudo reports whether bhyve must be started through sudo.
//
// bhyve requires root, or membership of the "vmm" group (see vmm(4)).
func needsSudo() bool {
	if os.Geteuid() == 0 {
		return false
	}
	g, err := user.LookupGroup("vmm")
	if err != nil {
		return true
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return true
	}
	if os.Getgid() == gid {
		return false
	}
	groups, err := os.Getgroups()
	if err != nil {
		return true
	}
	for _, id := range groups {
		if id == gid {
			return false
		}
	}
	return true
}
