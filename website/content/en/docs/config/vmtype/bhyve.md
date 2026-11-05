---
title: bhyve
weight: 6
---

| ⚡ Requirement | FreeBSD, native architecture (no cross-arch emulation) |
| ------------- | ------------------------------------------------------ |

[bhyve](https://man.freebsd.org/cgi/man.cgi?query=bhyve&sektion=8) is the hypervisor that ships with the FreeBSD base system (`/usr/sbin/bhyve`), and the only way to run a guest with hardware acceleration on FreeBSD: QEMU on FreeBSD has no accelerator to attach to and therefore runs in pure emulation (TCG).

Because bhyve cannot emulate a foreign architecture, Lima falls back to QEMU when the guest architecture does not match the host. To run, for example, an amd64 guest on an arm64 FreeBSD host, use `--vm-type=qemu` and accept the slow emulation.

## Host prerequisites

bhyve itself is part of the base system, but a boot ROM and the user-mode network backend are packages:

```bash
pkg install libslirp                    # required by the slirp network backend
pkg install bhyve-firmware              # amd64: UEFI boot ROM (BHYVE_UEFI.fd)
pkg install u-boot-bhyve-arm64          # arm64: U-Boot boot ROM (mandatory there)
pkg install qemu                        # optional, for qemu-img image conversion
```

`qemu-img` is only needed to convert images that are not already raw (for example the qcow2 images used by most templates); bhyve itself reads raw images only.

Running bhyve requires either root or membership of the `vmm` group:

```bash
sudo pw groupmod vmm -m "$USER"
```

Lima uses `sudo -n` when the current user is not privileged, so passwordless sudo for `bhyve`/`bhyvectl`/`kldload` is required otherwise.

## Using the driver with Lima

```bash
limactl start --vm-type=bhyve
limactl shell default
```

```yaml
vmType: bhyve
```

## Mounts

`reverse-sshfs` is the default and needs nothing from the hypervisor beyond a working network stack. `9p` is also available (bhyve's `virtio-9p` exists on both amd64 and arm64) and can be selected explicitly:

```yaml
mountType: 9p
```

`virtiofs` is not available.

## Notes and caveats

- The guest console is the serial console; there is no graphical output (`limactl start --display` is not supported).
- Snapshots are not implemented. Use ZFS snapshots of the instance directory instead.
- Stopping an instance is equivalent to pulling the plug: bhyve cannot be asked to send an ACPI power button event, so `bhyvectl --destroy` is used.
- A guest-initiated reboot ends the instance rather than restarting it.
- Guest-to-internet ICMP (`ping` on a public address) is not forwarded: the user-mode network would have to emit real ICMP packets, which FreeBSD only permits for root. TCP, UDP and DNS are unaffected.
