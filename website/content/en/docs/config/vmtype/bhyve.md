---
title: bhyve
weight: 6
---

| ⚡ Requirement | FreeBSD, native architecture (no cross-arch emulation) |
| ------------- | ------------------------------------------------------ |

[bhyve](https://man.freebsd.org/cgi/man.cgi?query=bhyve&sektion=8) is the hypervisor that ships with the FreeBSD base system (`/usr/sbin/bhyve`), and the only way to run a guest with hardware acceleration on FreeBSD: QEMU on FreeBSD has no accelerator to attach to and therefore runs in pure emulation (TCG).

Because bhyve cannot emulate a foreign architecture, Lima falls back to QEMU when the guest architecture does not match the host. To run, for example, an amd64 guest on an arm64 FreeBSD host, use `--vm-type=qemu` and accept the slow emulation.

## Host prerequisites

bhyve itself is part of the base system, but a boot ROM and a network backend are needed:

```bash
pkg install bhyve-firmware              # amd64: UEFI boot ROM (BHYVE_UEFI.fd)
pkg install u-boot-bhyve-arm64          # arm64: U-Boot boot ROM (mandatory there)
pkg install libslirp                    # slirp mode (FreeBSD >= 15.1)
pkg install qemu                        # optional, for qemu-img image conversion
```

`qemu-img` is only needed to convert images that are not already raw (for example the qcow2 images used by most templates); bhyve itself reads raw images only.

`networking` selects the backend:

- `auto` (default): `slirp` on FreeBSD >= 15.1, because that release added the
  slirp `open` option for outbound NAT; `bridge` on older releases.
- `slirp`: bhyve's libslirp backend with `open`. The guest uses the fixed
  `10.0.2.15/24` address and `10.0.2.2` gateway, and SSH is reached through a
  host port forward. Requires `libslirp` and FreeBSD >= 15.1.
- `bridge`: a `tap(4)` attached to a vm-bhyve bridge. The guest gets a static
  address from `network`/`address`.

The `bridge`, `network`, `gateway`, and `address` fields are used only in
`bridge` mode:

```yaml
vmType: bhyve
vmOpts:
  bhyve:
    networking: auto          # auto | slirp | bridge
    bridge: vm-public         # bridge mode default
    network: 172.16.0.0/24    # bridge mode default
    gateway: 172.16.0.1       # bridge mode default: first usable host
    address: 172.16.0.3       # bridge mode default: derived from the instance dir
    # firmware: /usr/local/share/uefi-firmware/BHYVE_UEFI.fd
```

In bridge mode, the configured bridge must already exist and must have the
gateway address. Internet access for the guests comes from the host's NAT rule
for the configured network, for example:

```bash
vm switch create public
vm switch add public em0               # or the host's uplink interface
# pf: nat on em0 inet from <network> to any -> (em0)
```

If `address` is omitted, Lima derives a stable address from the first eight
usable hosts of `network` (`2`-`9` for a /24), which is outside the default
vm-bhyve dnsmasq pool. Set `address` explicitly to avoid collisions or to use
another range.

bhyve, `ifconfig`, `bhyvectl`, and `kldload` are invoked through `sudo -n`, so
passwordless sudo for those commands is required unless Lima itself runs as
root. Membership of the `vmm` group alone is not enough, because the tap device
is created and owned by root.

## Using the driver with Lima

```bash
limactl start --vm-type=bhyve
limactl shell default
```

```yaml
vmType: bhyve
```

## Networking

In bridge mode, Lima creates a tap device, attaches it to the configured bridge,
and configures the guest statically from `cidata`:

- guest address: `vmOpts.bhyve.address`, or a stable value derived from the
  instance directory within `vmOpts.bhyve.network`
- default route and `host.lima.internal`: the configured gateway
- DNS: the host's `/etc/resolv.conf` nameservers, reached through the bridge NAT
- SSH: directly to the guest at port 22 (no port forwarding is needed)

In slirp mode, the guest uses `10.0.2.15/24` with gateway `10.0.2.2`; SSH is
reached at `127.0.0.1:<host port>` through bhyve's `hostfwd`, and the `open`
option provides outbound NAT for Internet access.

In both modes the guest has normal outbound TCP, UDP, DNS, and ICMP
connectivity, and `limactl port-forward` works over SSH.

## Mounts

`9p` is the default (bhyve's `virtio-9p` is available on both amd64 and arm64).
`reverse-sshfs` remains available as an opt-in, but the guest must already have
`sshfs` installed:

```yaml
mountType: 9p            # default
# mountType: reverse-sshfs
```

`virtiofs` is not available.

## Notes and caveats

- The guest console is the serial console; there is no graphical output (`limactl start --display` is not supported).
- Snapshots are not implemented. Use ZFS snapshots of the instance directory instead.
- Stopping an instance is equivalent to pulling the plug: bhyve cannot be asked to send an ACPI power button event, so `bhyvectl --destroy` is used.
- A guest-initiated reboot ends the instance rather than restarting it.
