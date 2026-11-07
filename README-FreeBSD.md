# Lima on FreeBSD 使用指南

本文档介绍如何在 FreeBSD 主机上构建、安装和使用 Lima。当前仓库是 Lima 的 FreeBSD 端口，除上游功能外还包括：

- 原生 `bhyve` 驱动（FreeBSD 原生架构默认使用）
- FreeBSD 上的 QEMU 适配（跨架构时使用，纯 TCG 模拟）
- bhyve / QEMU 默认使用 `9p` 挂载
- bhyve 按 FreeBSD 版本选择默认网络：
  - FreeBSD <= 15.0：`tap` + 网桥
  - FreeBSD >= 15.1：`slirp,open`（可访问公网）
- bhyve 的 `vmOpts.bhyve` 网络配置（bridge/network/gateway/address/firmware）

> 需要 FreeBSD 14/15、amd64 或 arm64。bhyve 只能运行与主机同架构的 guest；跨架构请使用 QEMU。

---

## 1. 驱动选择

| 场景 | 推荐驱动 | 说明 |
|---|---|---|
| FreeBSD 原生架构 | `bhyve` | 硬件加速，默认驱动 |
| 跨架构（如 amd64 主机跑 arm64 guest） | `qemu` | 纯 TCG 软件模拟，速度较慢 |
| 需要图形界面 | 不推荐 | bhyve 无图形控制台；请使用 SSH/端口转发 |
| 需要快照 | bhyve 不支持；QEMU 视驱动/配置 | bhyve 快照未实现；可用 ZFS 快照实例目录 |

FreeBSD 上 `limactl start` 默认使用 `bhyve`；需要 QEMU 时显式指定 `--vm-type=qemu`。

---

## 2. 主机准备

### 2.1 安装依赖

```bash
# bhyve 启动固件（按架构选择）
pkg install bhyve-firmware              # amd64
pkg install u-boot-bhyve-arm64          # arm64

# bhyve slirp 网络（FreeBSD >= 15.1 使用）
pkg install libslirp

# 可选：qemu-img 用于镜像格式转换，或使用 QEMU 驱动
pkg install qemu

# 从源码构建
pkg install gmake go
```

`bhyve` 本身属于 FreeBSD 基本系统，不需要额外安装。

### 2.2 加载 vmm(4)

```bash
sudo kldload vmm
ls /dev/vmm
```

如需开机自动加载：

```bash
sudo sysrc kld_list+=vmm
```

### 2.3 bhyve 网桥与 NAT（bridge 模式）

FreeBSD <= 15.0 的 bhyve 默认使用 `tap` + 网桥；15.1 以上如果选择 `bridge` 模式也同样需要。默认桥名为 `vm-public`，地址为 `172.16.0.1/24`。

如果已经使用 vm-bhyve，可以直接复用现成的 `vm-public`：

```bash
pkg install vm-bhyve
vm init
vm switch create public
vm switch add public <uplink>          # 例如 em0、re0、wlan0
sudo ifconfig vm-public 172.16.0.1/24 up
```

也可以手工创建：

```bash
sudo ifconfig bridge create            # 输出例如 bridge0
sudo ifconfig bridge0 name vm-public
sudo ifconfig vm-public 172.16.0.1/24 up
```

配置 pf NAT（把 `<uplink>` 换成实际上网接口）：

```text
# /etc/pf.conf
nat on <uplink> inet from 172.16.0.0/24 to any -> (<uplink>)
```

```bash
sudo sysrc pf_enable=YES
sudo sysrc gateway_enable=YES
sudo pfctl -f /etc/pf.conf
```

说明：

- Lima 会给 bhyve bridge 实例分配 `172.16.0.2`-`172.16.0.9` 的静态地址，避开 vm-bhyve 默认 dnsmasq 池（`172.16.0.10`-`172.16.0.254`）。
- 如果 dnsmasq 占用了这些地址，或需要更多实例，请在实例配置里显式设置 `vmOpts.bhyve.address`。
- guest 的 DNS 使用主机 `/etc/resolv.conf` 中的 nameserver，通过 bridge NAT 访问。
- `host.lima.internal` 指向 bridge 网关（默认 `172.16.0.1`）。

### 2.4 sudo 权限

bhyve、`ifconfig`、`bhyvectl`、`kldload` 都通过 `sudo -n` 执行，需要免密 sudo。可以只放行这些命令：

```text
# /etc/sudoers.d/lima
<user> ALL=(ALL) NOPASSWD: /usr/sbin/bhyve, /usr/sbin/bhyvectl, /sbin/ifconfig, /sbin/kldload
```

```bash
sudo visudo -cf /etc/sudoers.d/lima
```

注意：仅加入 `vmm` 组不足以创建/打开 root 所有的 tap 设备，仍需要 sudo 或直接以 root 运行 Lima。

### 2.5 检查环境

```bash
ls /usr/sbin/bhyve /usr/sbin/bhyvectl
ls /usr/local/lib/libslirp.so
ls /usr/local/share/uefi-firmware/BHYVE_UEFI.fd
ifconfig vm-public
```

---

## 3. 构建与安装

### 3.1 从源码构建

FreeBSD 的 `/usr/bin/make` 是 BSD make，不能直接构建 Lima；请使用 `gmake`（GNU make）。

```bash
cd /path/to/lima

# 如果 go 不在 PATH，加入 Go 的 bin 目录
export PATH=/usr/local/go/bin:$PATH          # 或本机 Go 安装路径
export GOPROXY=https://goproxy.cn,direct     # 网络受限时可选

gmake native
gmake manpages                               # 可选：生成 man 手册
```

Go 版本需要满足 `go.mod` 中的要求（当前为 1.26.x 或更新）；如果 FreeBSD 包中的 Go 太旧，请使用更新的 Go 工具链。

`gmake native` 会构建：

- `limactl` 及 CLI 插件
- Linux x86_64 guestagent
- 默认模板和全部模板
- helpers（`lima`、`nerdctl.lima`、`docker.lima` 等）

### 3.2 安装

```bash
sudo gmake install
```

安装到 `/usr/local`：

| 路径 | 内容 |
|---|---|
| `/usr/local/bin/limactl` | 主命令 |
| `/usr/local/bin/lima` | shell 包装脚本 |
| `/usr/local/libexec/lima/` | CLI 插件 |
| `/usr/local/share/lima/` | guestagent、模板 |
| `/usr/local/share/man/man1/` | man 手册（执行 `gmake manpages` 后） |

`gmake install` 会先执行 `uninstall`，因此会替换已有的 `/usr/local/bin/limactl` 和 `/usr/local/share/lima`；不会删除 `~/.lima` 中的实例。

### 3.3 验证

```bash
limactl --version
limactl start --list-drivers       # 应包含 bhyve 和 qemu
limactl start --list-templates     # 应包含 ubuntu-24.04、alpine 等
man limactl
```

---

## 4. 快速开始

### 4.1 bhyve（原生架构）

```bash
limactl start --name=dev --vm-type=bhyve \
  --cpus=2 --memory=4 --disk=20 \
  template:ubuntu-24.04

limactl list
limactl shell dev
```

在 FreeBSD 15.0 上使用 bridge 模式，`limactl list` 的 SSH 列类似：

```text
172.16.0.3:22
```

在 FreeBSD 15.1 以上默认使用 slirp，SSH 列类似：

```text
127.0.0.1:<随机端口>
```

两种模式下 `limactl shell dev` 都可用。

### 4.2 QEMU（跨架构或非 bhyve 场景）

```bash
limactl start --name=arm --vm-type=qemu --arch=aarch64 \
  --cpus=2 --memory=4 --disk=20 \
  template:alpine
```

QEMU 在 FreeBSD 上没有硬件加速，只能使用 TCG，启动和运行会比较慢；能用 bhyve 时优先用 bhyve。

### 4.3 常用命令

```bash
limactl list                       # 列出实例
limactl start <name>               # 启动
limactl shell <name>               # 进入 guest
limactl shell <name> -- uname -a   # 在 guest 执行命令
limactl stop <name>                # 停止
limactl restart <name>             # 重启
limactl delete <name>              # 删除
limactl edit <name>                # 编辑实例配置
limactl info                       # 诊断信息
limactl prune                      # 清理无用对象
```

端口转发：

```bash
limactl start --name=web --port-forward=8080:80 template:ubuntu-24.04
```

挂载（默认 9p，`~` 只读）：

```bash
limactl start --name=dev --mount '~/work:w' template:ubuntu-24.04
limactl start --name=dev --mount-writable template:ubuntu-24.04
```

查看 SSH 配置：

```bash
cat ~/.lima/dev/ssh.config
ssh -F ~/.lima/dev/ssh.config lima-dev
```

---

## 5. bhyve 网络

### 5.1 默认模式

`vmOpts.bhyve.networking` 控制默认后端：

```yaml
vmOpts:
  bhyve:
    networking: auto   # auto | slirp | bridge
```

- `auto`（默认）：
  - FreeBSD >= 15.1：`slirp`，使用 bhyve 新增的 `open` 参数打开公网 NAT
  - FreeBSD <= 15.0：`bridge`，使用 `tap` + `vm-public`
- `slirp`：显式 slirp；15.1 以下会报错
- `bridge`：显式 tap + 网桥

版本检测使用 `freebsd-version -u`（失败时回退 `-k`、`uname -r`），因为 bhyve 属于 userland。

### 5.2 bridge 模式

```yaml
vmType: bhyve
mountType: 9p
vmOpts:
  bhyve:
    networking: bridge
    bridge: vm-public          # 默认
    network: 172.16.0.0/24     # 默认
    gateway: 172.16.0.1        # 默认：network 的第一个可用地址
    address: 172.16.0.3        # 默认：从实例目录推导到 .2-.9
    # firmware: /usr/local/share/uefi-firmware/BHYVE_UEFI.fd
```

- `bridge`/`network`/`gateway`/`address` 只在 bridge 模式生效
- guest 使用静态地址，SSH 直接连接 `guestIP:22`
- 支持正常的 TCP/UDP/DNS/ICMP 出网（通过主机 NAT）
- 需要主机上存在对应的 bridge 和 NAT 规则

### 5.3 slirp 模式

```yaml
vmType: bhyve
vmOpts:
  bhyve:
    networking: slirp   # 需要 FreeBSD >= 15.1 和 libslirp
```

- guest 固定使用 `10.0.2.15/24`，网关 `10.0.2.2`
- SSH 通过 `hostfwd` 映射到 `127.0.0.1:<host port>`
- `open` 提供公网 NAT；guest 的 DNS 使用主机 `/etc/resolv.conf`，通过该 NAT 访问
- bhyve 命令形如：
  ```text
  -s 3,virtio-net,slirp,open,hostfwd=tcp::<port>-:22,mac=...
  ```
- 需要 `pkg install libslirp`
- `networking: slirp` 在 15.0 及以下会明确报错，请改用 `bridge` 或 `auto`

### 5.4 固定地址/自定义地址

bridge 模式下，如果实例较多，`.2`-`.9` 只有 8 个默认槽位。可以通过 `address` 显式指定：

```yaml
vmOpts:
  bhyve:
    networking: bridge
    network: 172.16.0.0/24
    gateway: 172.16.0.1
    address: 172.16.0.20
```

地址必须位于 `network` 内、不能是网关/网络地址/广播地址；不合法会在启动时直接报错。

---

## 6. 挂载

bhyve 和 QEMU 默认都使用 `9p`：

```yaml
mountType: 9p
```

- `9p` 由 bhyve/QEMU 自身提供，不需要 guest 额外安装包
- 默认挂载 `~` 为只读；写挂载：
  ```bash
  limactl start --name=dev --mount '~/work:w' template:ubuntu-24.04
  ```
  或在 YAML 中设置 `mounts[].writable: true`
- `reverse-sshfs` 仍可用，但 guest 必须已安装 `sshfs`：
  ```yaml
  mountType: reverse-sshfs
  ```
- `virtiofs` 在 FreeBSD 上不可用

如果 guest 镜像的内核没有 9p 支持，请改用 `reverse-sshfs`。

---

## 7. QEMU on FreeBSD

QEMU 在 FreeBSD 上：

- 没有 KVM/bhyve 加速后端，只能 `-accel tcg`；启动和运行明显慢于 bhyve
- 适合跨架构 guest，或临时验证
- FreeBSD 上的 QEMU 默认挂载也是 `9p`（需要 QEMU 构建时启用 `virtfs`；新版 `emulators/qemu` 已启用）
- 如果 QEMU 构建没有 virtfs：
  ```yaml
  mountType: reverse-sshfs
  ```
- QEMU 的 user-mode 网络由 libslirp 提供；guest 的 TCP/UDP/DNS 出网正常
- FreeBSD 不允许普通用户创建 ICMP socket，因此 **QEMU guest 到公网的 `ping` 会失败**；`ping` 网关仍然可用。请用 `nc -z <host> 443` 或 `curl` 验证连通性
- 启动时会看到一次 ICMP 警告，这是预期行为

---

## 8. 故障排查

| 现象 | 原因/处理 |
|---|---|
| `make: ... .SECONDEXPANSION` | 使用了 BSD make；改用 `gmake` |
| `go: command not found` | 安装 Go 并把其 `bin` 目录加入 `PATH` |
| `proxy.golang.org ... i/o timeout` | 设置 `GOPROXY=https://goproxy.cn,direct` |
| `fork/exec /tmp/go-build...: permission denied` | `/tmp` 是 `noexec`；设置 `TMPDIR=$HOME/.gotmp GOTMPDIR=$HOME/.gotmp` |
| `bridge "vm-public" is not available` | 创建 `vm-public`，或修改 `vmOpts.bhyve.bridge` |
| `libslirp is required` | `pkg install libslirp` |
| `must not be "slirp" before FreeBSD 15.1` | 使用 `networking: bridge`/`auto`，或升级到 15.1+ |
| `sudo: a password is required` | 配置免密 sudo（见 2.4） |
| `bhyve: no bootrom was configured` | `pkg install bhyve-firmware`，或设置 `vmOpts.bhyve.firmware` |
| `bhyve ... did not exit within 30s` | 再次 `limactl stop`；必要时 `sudo bhyvectl --destroy --vm=<name>` |
| bhyve 启动后马上结束 | guest 发起了 reboot；bhyve 视为实例结束，重新 `limactl start <name>` |
| QEMU guest `ping` 公网失败 | 预期限制；用 TCP/UDP 验证 |
| `sshfs` 相关失败 | 改用 `9p`，或在 guest 中安装 `sshfs` |
| `Permission denied (publickey)` 且 guest 中 `passwd` 字段为 `*LOCKED*` | 这是旧版本 `lock_passwd: true` 造成的：`pw lock` 写入 `*LOCKED*`，FreeBSD 的 sshd 会拒绝**所有**认证方式。用当前版本重建实例即可；临时办法是在 guest 控制台执行 `pw unlock -n <user>` |
| guest 无默认路由（`10.0.2.15` 已配置但 `ping 10.0.2.2` 不通） | nuageinit 只认 netplan 的 `gateway4`；旧版 cidata 用 `routes` 导致路由缺失。升级后重建实例 |
| `limactl list` 出现版本警告 | 旧实例的 `lima-version` 元数据；不影响使用 |
| 残留 tap 设备 | `sudo ifconfig vm-public deletem tapN && sudo ifconfig tapN destroy` |

补充：

- bhyve 停止等价于拔电源：驱动使用 `bhyvectl --destroy`，不会发送 ACPI 关机事件
- bhyve 不支持快照和图形界面；快照请用 ZFS
- `limactl sudoers` 目前不支持 FreeBSD，请手工维护 sudoers

---

## 9. 目录与日志

| 路径 | 说明 |
|---|---|
| `~/.lima/<instance>/lima.yaml` | 实例配置 |
| `~/.lima/<instance>/disk` | 磁盘 |
| `~/.lima/<instance>/cidata.iso` | cloud-init seed |
| `~/.lima/<instance>/serial.log` | 串口日志 |
| `~/.lima/<instance>/ha.stderr.log` | hostagent 日志 |
| `~/.lima/<instance>/ssh.config` | `ssh -F` 可用的 SSH 配置 |
| `~/.lima/<instance>/bhyve.tap` | bridge 模式记录的 bridge/tap 名称，用于停止时清理 |
| `~/.lima/_config/user` | Lima 默认 SSH 私钥 |
| `~/.lima/_config/networks.yaml` | Lima 网络配置 |

---

## 10. 开发与测试

```bash
cd /path/to/lima

gmake native                 # 完整开发构建
gmake minimal                # 最小构建
gmake install                # 安装到 /usr/local（需要 sudo）

go test ./...                # 单元测试，不启动虚拟机
gmake bats                   # BATS 集成测试，会启动真实虚拟机（需 submodule）
gmake lint                   # 各类 lint
gmake generate               # 生成 protobuf/mock 等
```

注意：

- 新 Go/shell/Makefile 文件需要 SPDX 头；提交需要 `git commit -s`（DCO）
- 更多开发约定见 `AGENTS.md` 和 `website/content/en/docs/community/contributing.md`

---

## 11. 已知限制

- bhyve 不支持跨架构、快照、图形控制台；guest reboot 会结束实例
- bhyve bridge 模式默认静态地址只有 `.2`-`.9`；多实例请设置 `address`
- bhyve slirp `open` 需要 FreeBSD >= 15.1；15.0 及以下默认使用 bridge
- QEMU on FreeBSD 只有 TCG；guest 到公网 ICMP 不可用
- QEMU 9p 需要 QEMU 构建启用 virtfs；旧构建请用 `reverse-sshfs`
- `virtiofs` 在 FreeBSD 上不可用
- `limactl sudoers` 不支持 FreeBSD

---

## 12. 参考资料

- bhyve 驱动文档：`website/content/en/docs/config/vmtype/bhyve.md`
- QEMU 驱动文档：`website/content/en/docs/config/vmtype/qemu.md`
- 内部结构、生命周期、`LIMA_CIDATA_*`：`website/content/en/docs/dev/internals.md`
- 驱动开发与注册：`website/content/en/docs/dev/drivers.md`
- 贡献规则（含 AI 贡献规则）：`website/content/en/docs/community/contributing.md`
- 上游文档：https://lima-vm.io
