# EC2 virtual machines

EC2 defaults to `records`: instances have AWS-shaped state without executing guest software.
Set `ec2.runtime: lima` or `ec2.runtime: qemu` to boot real Linux guests. Install the selected driver yourself; TinyStack does not install VM software.
The first launch downloads a pinned cloud image (several GB); boots can take several minutes.
Allow at least 20 GiB disk per guest plus the image cache, guest RAM and virtualization overhead.

```yaml
ec2:
  runtime: lima
  vm:
    max_vms: 2
    max_memory_mib: 4096
  lima:
    command: limactl
```

Start TinyStack on a free, user-selected port, for example:

```sh
tinystack start --port 18080 --console-port 18081 --config tinystack.yaml
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:18080
```

Select an AMI with `aws ec2 describe-images --filters Name=name,Values='al2023-*'` or
`Name=name,Values='ubuntu/*'`. Choose `arm64` on Apple Silicon/ARM hosts with `t4g.micro`,
or `x86_64` with `t3.micro` on x86 hosts. The AMI determines guest architecture.

```sh
aws ec2 run-instances --image-id <AMI-ID> --instance-type t4g.micro --count 1   --user-data file://startup.sh --key-name <IMPORTED-KEY-NAME>   --iam-instance-profile Name=<PROFILE-NAME>
aws ec2 describe-instances
```

This command creates a VM and downloads the image if absent. Use a `#!/bin/sh` startup script,
or `#cloud-config`. Supported cloud-config directives are `bootcmd`, `users`, `write_files`,
`packages`, `package_update`, `package_upgrade`, and `runcmd`; others fail explicitly.
Scripts run once through the shared guest setup with the TinyStack environment exported.
Cloud-config modules run through `cloud-init single`; this is not a full EC2 cloud-init datasource.
Package directives and the guest nftables dependency may download packages inside the guest.
The key pair public key is installed for `ec2-user` (AL2023) or `ubuntu`.

The instance remains pending until the driver detects SSH readiness (authenticated SSH for Lima, an SSH banner for QEMU). Failed boots
terminate with `Server.InternalError`. Inspect the `tinystack:ssh-endpoint` tag for `127.0.0.1:PORT`;
use `ssh -p PORT ec2-user@127.0.0.1` (or `ubuntu`) with your imported private key.
EC2 private/public IP fields remain emulated addresses. Security groups, routing and NAT are **not enforced**.
Guests have their driver's user-mode network access. There are no host directory mounts or automatic guest port forwards.
Only t3, t4g, m5, m7g and c5 sizes have real VM sizing. Memory is never silently capped;
an oversized request or exhausted VM slots returns `InsufficientInstanceCapacity`.
Stopped VMs reserve slots until terminated. Both drivers support Linux guests only.

## Metadata and AWS access

Each instance gets its own host loopback HTTP listener on an ephemeral port, saved for restart recovery.
Lima's `host.lima.internal` reaches host loopback. Guest cloud-init bootcmd adds a **guest-only** nftables (or iptables fallback)
OUTPUT rule redirecting `169.254.169.254:80` to Lima's host gateway `192.168.5.2:PORT`.
The same setup is checked after dependencies install. No host network, DNS or trust settings change;
TinyStack never binds the metadata IP, DNS port or HTTPS port for this feature.

The listener fixes the instance ID before passing IMDS requests to the existing metadata service.
NAT source IP addresses do not select the instance. Both IMDSv1 and IMDSv2 work, honoring instance
metadata options. Attach an IAM instance profile with an EC2 trust role to obtain temporary credentials.
`AWS_ENDPOINT_URL=http://host.lima.internal:PORT` and the region are exported for user data and written
to `/etc/environment` for login sessions. The listener passes AWS API requests directly to TinyStack's
gateway, so loopback-only main listeners and ephemeral test servers work too.

Shutdown **leaves VMs running** and closes the host listeners. Restart with the same persistent store to
restore listeners on their saved ports; those ports must still be free. Use persistent storage if you need
recovery after restart. An in-memory store loses ownership when the process exits. Terminate before
exiting an in-memory deployment, or manually manage its leftover VMs with the selected driver.
Terminate deletes one owned VM; reset enumerates and deletes only the store's ownership prefix,
including interrupted launches. Other Lima instances, Colima and Docker are outside this cleanup.

## Images and overrides

AL2023 catalogue entries use Amazon Linux 2023.7.20250609.0 KVM qcow2 (x86_64 and arm64).
Ubuntu 24.04 entries use Canonical release-20260926 cloud images for both architectures.
The AWS-shaped catalogue names are fixed fixtures, not the VM build version.
URLs and SHA256 digests are pinned in `internal/services/ec2/vm_images.go`, with official checksum
manifest source URLs. AL2 has no default VM mapping and fails launch in VM modes.
Override a catalogue AMI's image with a verified HTTPS URL and SHA256/SHA512 digest:

```yaml
ec2:
  runtime: lima
  vm:
    images:
      ami-<catalogue-id>:
        url: https://example.com/compatible-linux-cloud-image.qcow2
        digest: sha256:<64-hex-digits>
```

Overrides retain the catalogue architecture and login user; arbitrary unknown AMI IDs remain unsupported.

## Tests

Ordinary tests use a fake `limactl` in a temporary directory and never create a VM.
The explicit real-VM test downloads an image, creates a VM, runs user data, checks guest IMDS role
credentials, and terminates it. Run only after installing Lima and accepting those resource effects:

```sh
TINYSTACK_VM_TESTS=1 go test ./tests/integration -run '^TestLimaRealVM$' -timeout 20m
```

## Driver comparison

| Driver | Host support | Guest network | Readiness | Provisioning |
| --- | --- | --- | --- | --- |
| Lima | Lima-supported macOS/Linux hosts | Lima user network | Authenticated SSH | Lima provision |
| QEMU | Linux KVM, macOS HVF, Windows x86_64 WHPX | QEMU user/slirp | SSH banner and QMP | NoCloud HTTP |

QEMU uses `-netdev user`: its default host gateway `10.0.2.2` reaches host loopback.
Only SSH is forwarded, to `127.0.0.1` on an ephemeral port. The instance listener serves
`/meta-data`, `/user-data`, and `/vendor-data` with fixed instance identity, alongside IMDS and AWS APIs.
SMBIOS serial carries `ds=nocloud-net;s=http://10.0.2.2:PORT/`; argv is passed directly, so no shell escaping
of the semicolon is needed. The trailing slash is required. `nocloud-net` supports the older pinned AL2023
cloud-init as well as Ubuntu. No seed ISO is built.

The shared setup prefers `nft` and falls back to `iptables`; it installs nftables if neither exists.
AL2023 uses nftables rather than depending on the legacy iptables package. The redirect and AWS endpoint
use `10.0.2.2` for QEMU and Lima's host gateway for Lima. Only the guest firewall is changed.

## QEMU setup

Install QEMU yourself using your operating system's package manager or a trusted Windows distribution;
TinyStack does not install software, enable hypervisors, or change device permissions.

```yaml
ec2:
  runtime: qemu
  vm:
    max_vms: 2
    max_memory_mib: 4096
  qemu:
    system_command:
      arm64: qemu-system-aarch64
      x86_64: qemu-system-x86_64
    img_command: qemu-img
    allow_tcg: false
    # firmware: /absolute/path/to/edk2-aarch64-code.fd
    # cache_dir: /absolute/path/to/ec2-images
    # state_dir: /absolute/path/to/ec2-vms
```

- **macOS:** install a QEMU build with HVF and user/slirp networking. Apple Silicon uses arm64 AMIs
  and t4g instances; Intel uses x86_64 AMIs and t3 instances. ARM requires EDK2 firmware, normally bundled
  at `share/qemu/edk2-aarch64-code.fd` relative to the binary's installation prefix.
- **Linux:** install QEMU system binaries, qemu-img, slirp and (on ARM) EDK2/AAVMF firmware. Enable KVM
  and give the TinyStack user read/write access to `/dev/kvm` through your distribution's normal setup.
  Choose the host's architecture. TinyStack checks device access before downloading images.
- **Windows:** use x86_64 QEMU with slirp and enable Windows Hypervisor Platform yourself. WHPX must be
  available to that executable. Windows ARM hardware acceleration is not supported; explicitly opt into TCG.
  Windows uses a loopback TCP QMP socket and detached process creation, since `-daemonize` is unavailable.

Hardware acceleration is required by default. Launch failures report an acceleration/configuration error.
`allow_tcg: true` permits slower software emulation, including a guest architecture different from the host.
The driver first attempts available hardware acceleration, then retries with TCG on launch failure.
ARM firmware is discovered beside the executable or in common QEMU/AAVMF paths; set `firmware` if needed.
x86_64 uses Q35 with default SeaBIOS: Amazon's GPT KVM image supports both BIOS and UEFI, so OVMF is optional
and is not needed by this driver. ARM uses `virt` and requires UEFI.

The QEMU image cache defaults to `os.UserCacheDir()/tinystack/ec2-images` (macOS `~/Library/Caches`,
Linux `$XDG_CACHE_HOME` or `~/.cache`, Windows `%LocalAppData%`). Downloads stream to temporary files;
SHA256 (or a configured SHA512 override) is verified before atomic publication. Concurrent writers lock
by digest. An interrupted download may leave a `.lock` directory; after confirming no downloader is running,
remove that lock to retry. A mismatch discards the download and fails launch.

State defaults to `os.UserCacheDir()/tinystack/ec2-vms`. Each store-prefixed instance directory contains a
qcow2 overlay, saved launch settings, serial log and pidfile. The root disk grows to the catalogue root volume size (8 GiB) when the base is
smaller; larger images are never shrunk. Custom EC2 root block-device sizing is not currently exposed.
Unix QMP uses short hashed socket names under the state root; configure a short state path if your platform's
Unix socket path limit is exceeded. Do not evict the image cache while overlays reference it.

Stop sends ACPI powerdown, then kills the tracked process after 30 seconds. Start relaunches the same disk and SSH port;
reboot sends `system_reset`. Terminate and reset quit owned QEMU processes and remove owned state. If a pidfile remains but QMP is unreachable,
cleanup fails explicitly and retains state for manual recovery rather than signaling an unverified PID.
Shutdown leaves detached guests running. Retain the persistent TinyStack store, QEMU state and image cache
for recovery, and keep the saved listener/SSH ports available.

To try QEMU on **macOS or Linux**, install the prerequisites above, save this YAML as `tinystack.yaml`,
and start `tinystack start --port 18080 --console-port 18081 --config tinystack.yaml`.
Export the AWS environment shown earlier, import your SSH public key with
`aws ec2 import-key-pair --key-name vm-key --public-key-material fileb://$HOME/.ssh/id_ed25519.pub`,
then select an AL2023 AMI matching your host with `describe-images` and launch:

```sh
aws ec2 run-instances --image-id <AMI-ID> --instance-type <t4g.micro-or-t3.micro> --count 1 --key-name vm-key
aws ec2 describe-instances
# Read tinystack:ssh-endpoint, then SSH as ec2-user using your imported private key.
aws ec2 terminate-instances --instance-ids <INSTANCE-ID>
```

An explicit real QEMU smoke test is also available (downloads a cloud image and creates a guest):

```sh
TINYSTACK_VM_TESTS=1 go test ./tests/integration -run '^TestQEMURealVM$' -timeout 20m
```

Ordinary QEMU tests re-execute the Go test binary as fake qemu-img/QEMU programs; no real VM is created.
See [QEMU networking](https://www.qemu.org/docs/master/system/invocation.html),
[NoCloud](https://docs.cloud-init.io/en/26.1/reference/datasources/nocloud.html), and
[AL2023 boot support](https://docs.aws.amazon.com/linux/al2023/ug/kvm-supported-configurations.html).
