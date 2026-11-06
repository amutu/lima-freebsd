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
	"path/filepath"
	"runtime"
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
	"github.com/lima-vm/lima/v2/pkg/limayaml"
)

// ErrNotSupported is returned by operations the bhyve driver does not implement.
var ErrNotSupported = errors.New("not supported by the bhyve driver")

// LimaBhyveDriver implements driver.Driver on top of FreeBSD's bhyve(8).
type LimaBhyveDriver struct {
	Instance     *limatype.Instance
	SSHLocalPort int

	cmd       *exec.Cmd
	waitCh    chan error
	vmName    string
	tapName   string
	bridge    string
	netConfig limayaml.BhyveNetworkConfig
	stopMux   sync.Mutex
}

var _ driver.Driver = (*LimaBhyveDriver)(nil)

func New() *LimaBhyveDriver {
	return &LimaBhyveDriver{}
}

func (l *LimaBhyveDriver) Configure(_ context.Context, inst *limatype.Instance) (*driver.ConfiguredDriver, error) {
	if err := fillConfig(inst.Config); err != nil {
		return nil, err
	}
	netConfig, err := limayaml.ResolveBhyveNetwork(inst.Dir, inst.Config)
	if err != nil {
		return nil, err
	}
	l.Instance = inst
	l.netConfig = netConfig
	l.bridge = netConfig.Bridge
	switch netConfig.Mode {
	case limayaml.BhyveModeSlirp:
		// Slirp is reached through the host-side hostfwd; keep the free port
		// allocated by the host agent.
		l.SSHLocalPort = inst.SSHLocalPort
		inst.SSHAddress = "127.0.0.1"
	default:
		// The tap is bridged, so the guest is reachable directly at its
		// static address and sshd listens on the standard port.
		l.SSHLocalPort = 22
		inst.SSHLocalPort = 22
		inst.SSHAddress = netConfig.Address.String()
	}
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

	l.vmName = VMName(l.Instance.Name, l.Instance.Dir)
	if err := ensureVMM(ctx); err != nil {
		return nil, err
	}

	var tap string
	var err error
	if l.netConfig.Mode == limayaml.BhyveModeSlirp {
		if err := checkSlirpPrerequisites(); err != nil {
			return nil, err
		}
	} else {
		tap, err = l.createTap(ctx, l.netConfig.Bridge)
		if err != nil {
			return nil, err
		}
	}

	cfg := Config{
		Name:         l.Instance.Name,
		InstanceDir:  l.Instance.Dir,
		LimaYAML:     l.Instance.Config,
		Tap:          tap,
		Slirp:        l.netConfig.Mode == limayaml.BhyveModeSlirp,
		SSHLocalPort: l.SSHLocalPort,
	}
	exe, args, err := Cmdline(cfg, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		l.cleanupTap(ctx)
		return nil, err
	}

	// bhyve must run as root: the tap device created by ifconfig is owned by
	// root and cannot be opened by an unprivileged process.
	argv := append([]string{exe}, args...)
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}

	serialLog := filepath.Join(l.Instance.Dir, filenames.SerialLog)
	logFile, err := os.Create(serialLog)
	if err != nil {
		l.cleanupTap(ctx)
		return nil, err
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		logFile.Close()
		l.cleanupTap(ctx)
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
		l.cleanupTap(ctx)
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
		l.cleanupTap(ctx)
		return nil
	}
	select {
	case <-l.waitCh:
	case <-time.After(30 * time.Second):
		if l.cmd != nil && l.cmd.Process != nil {
			_ = l.cmd.Process.Kill()
		}
		l.cleanupTap(ctx)
		return fmt.Errorf("bhyve VM %#q did not exit within 30s of being destroyed", vmName)
	}
	l.cleanupTap(ctx)
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
	l.cleanupTap(ctx)
	return nil
}

func (l *LimaBhyveDriver) InspectStatus(_ context.Context, inst *limatype.Instance) string {
	if inst == nil {
		return ""
	}
	// The guest address is static, so expose it to `limactl list` and
	// `limactl show-ssh` even while the instance is stopped.
	if netConfig, err := limayaml.ResolveBhyveNetwork(inst.Dir, inst.Config); err == nil {
		if netConfig.Mode == limayaml.BhyveModeSlirp {
			inst.SSHAddress = "127.0.0.1"
		} else {
			inst.SSHAddress = netConfig.Address.String()
			inst.SSHLocalPort = 22
		}
	} else {
		logrus.WithError(err).Warn("Failed to resolve the bhyve network configuration")
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
	if l.Instance == nil {
		return "", errors.New("bhyve driver is not configured")
	}
	if l.Instance.SSHAddress != "" {
		return l.Instance.SSHAddress, nil
	}
	netConfig, err := limayaml.ResolveBhyveNetwork(l.Instance.Dir, l.Instance.Config)
	if err != nil {
		return "", err
	}
	if netConfig.Mode == limayaml.BhyveModeSlirp {
		return "127.0.0.1", nil
	}
	return netConfig.Address.String(), nil
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
	tapFileName  = "bhyve.tap"
	libslirpPath = "/usr/local/lib/libslirp.so"
)

// checkPrerequisites verifies that everything required to boot a bhyve VM is installed.
func checkPrerequisites(ctx context.Context) error {
	for _, exe := range []string{bhyveExe, bhyvectlExe} {
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("%#q is not available: %w (bhyve is part of the FreeBSD base system)", exe, err)
		}
	}
	return nil
}

func checkSlirpPrerequisites() error {
	if _, err := os.Stat(libslirpPath); err != nil {
		return fmt.Errorf("libslirp is required by the bhyve slirp backend but %#q is missing (hint: pkg install libslirp): %w",
			libslirpPath, err)
	}
	return nil
}

// tapFilePath records the tap interface name so that Stop and Delete can clean
// it up even after the host agent has been restarted.
func (l *LimaBhyveDriver) tapFilePath() string {
	return filepath.Join(l.Instance.Dir, tapFileName)
}

// createTap creates a tap(4) device and attaches it to the configured bridge.
func (l *LimaBhyveDriver) createTap(ctx context.Context, bridge string) (string, error) {
	if bridge == "" {
		return "", errors.New("bridge is not configured")
	}
	if out, err := runCmd(ctx, "ifconfig", bridge); err != nil {
		return "", fmt.Errorf("bridge %#q is not available: %w (%s); hint: create it with "+
			"`vm switch create public` and attach a host interface with `vm switch add public <iface>`",
			bridge, err, strings.TrimSpace(out))
	}
	out, err := runRoot(ctx, "ifconfig", "tap", "create")
	if err != nil {
		return "", fmt.Errorf("failed to create a tap interface: %w (%s)", err, strings.TrimSpace(out))
	}
	tap := strings.TrimSpace(out)
	if tap == "" {
		return "", errors.New("ifconfig tap create returned an empty interface name")
	}
	cleanup := func() {
		_, _ = runRoot(ctx, "ifconfig", bridge, "deletem", tap)
		_, _ = runRoot(ctx, "ifconfig", tap, "destroy")
	}
	if out, err := runRoot(ctx, "ifconfig", tap, "up"); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to bring up %#q: %w (%s)", tap, err, strings.TrimSpace(out))
	}
	if out, err := runRoot(ctx, "ifconfig", bridge, "addm", tap); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to attach %#q to %#q: %w (%s)", tap, bridge, err, strings.TrimSpace(out))
	}
	// Record both names: the bridge may be reconfigured between Start and Stop.
	if err := os.WriteFile(l.tapFilePath(), []byte(bridge+"\n"+tap+"\n"), 0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to record the tap interface: %w", err)
	}
	l.bridge = bridge
	l.tapName = tap
	return tap, nil
}

// readTapRecord returns the bridge and tap names recorded at Start.
func (l *LimaBhyveDriver) readTapRecord() (bridge, tap string) {
	if l.Instance == nil {
		return "", ""
	}
	b, err := os.ReadFile(l.tapFilePath())
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	switch len(lines) {
	case 1:
		// Compatibility with the earlier one-line format.
		return "", strings.TrimSpace(lines[0])
	case 2:
		return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	default:
		return "", ""
	}
}

// cleanupTap detaches and destroys the tap interface, if any.
func (l *LimaBhyveDriver) cleanupTap(ctx context.Context) {
	bridge, tap := l.readTapRecord()
	if bridge == "" {
		bridge = l.bridge
	}
	if tap == "" {
		tap = l.tapName
	}
	if bridge == "" {
		bridge = limayaml.DefaultBhyveBridge
	}
	if tap == "" {
		return
	}
	_, _ = runRoot(ctx, "ifconfig", bridge, "deletem", tap)
	_, _ = runRoot(ctx, "ifconfig", tap, "destroy")
	l.tapName = ""
	l.bridge = ""
	if l.Instance != nil {
		_ = os.Remove(l.tapFilePath())
	}
}

// ensureVMM loads the vmm(4) kernel module if it is not loaded yet.
func ensureVMM(ctx context.Context) error {
	if _, err := os.Stat("/dev/vmm"); err == nil {
		return nil
	}
	out, err := runRoot(ctx, "kldload", "vmm")
	if err != nil {
		if strings.Contains(out, "already loaded") {
			return nil
		}
		return fmt.Errorf("failed to load the vmm kernel module: %w (%s)", err, strings.TrimSpace(out))
	}
	if _, err := os.Stat("/dev/vmm"); err != nil {
		return fmt.Errorf("vmm loaded but /dev/vmm is still missing; the CPU or the hypervisor may not support "+
			"virtualization (nested virtualization is usually unavailable): %w", err)
	}
	return nil
}

func runBhyvectl(ctx context.Context, args ...string) (string, error) {
	return runRoot(ctx, bhyvectlExe, args...)
}

// runCmd runs a command without elevation.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runRoot runs a command as root, using passwordless sudo when needed.
func runRoot(ctx context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
