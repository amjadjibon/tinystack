# EC2 virtual machines with Lima

EC2 defaults to `records`: instances have AWS-shaped state without executing guest software.
Set `ec2.runtime: lima` to boot real Linux guests. Install Lima yourself; TinyStack does not install it.
The first launch downloads a pinned cloud image (several GB); boots can take several minutes.
Allow at least 20 GiB disk per guest plus the image cache, guest RAM and virtualization overhead.

```yaml
ec2:
  runtime: lima
  lima:
    command: limactl
    max_vms: 2
    max_memory_mib: 4096
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
Scripts run once through Lima's cloud-init provision stage with the TinyStack environment exported.
Cloud-config modules run through `cloud-init single`; this is not a full EC2 cloud-init datasource.
Package directives and the guest iptables dependency may download packages inside the guest.
The key pair public key is installed for `ec2-user` (AL2023) or `ubuntu`.

The instance remains pending until Lima reports Running and authenticated SSH succeeds. Failed boots
terminate with `Server.InternalError`. Inspect the `tinystack:ssh-endpoint` tag for `127.0.0.1:PORT`;
use `ssh -p PORT ec2-user@127.0.0.1` (or `ubuntu`) with your imported private key.
EC2 private/public IP fields remain emulated addresses. Security groups, routing and NAT are **not enforced**.
Guests have Lima's network access. There are no host directory mounts or automatic guest port forwards.
Only t3, t4g, m5, m7g and c5 sizes have real VM sizing. Memory is never silently capped;
an oversized request or exhausted VM slots returns `InsufficientInstanceCapacity`.
Stopped VMs reserve slots until terminated. Linux guests only; other VM drivers are not implemented.

## Metadata and AWS access

Each instance gets its own host loopback HTTP listener on an ephemeral port, saved for restart recovery.
Lima's `host.lima.internal` reaches host loopback. Guest cloud-init bootcmd adds a **guest-only** iptables
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
exiting an in-memory deployment, or manually manage its leftover VMs with Lima.
Terminate deletes one owned VM; reset enumerates and deletes only the store's ownership prefix,
including interrupted launches. Other Lima instances, Colima and Docker are outside this cleanup.

## Images and overrides

AL2023 catalogue entries use Amazon Linux 2023.7.20250609.0 KVM qcow2 (x86_64 and arm64).
Ubuntu 24.04 entries use Canonical release-20260926 cloud images for both architectures.
The AWS-shaped catalogue names are fixed fixtures, not the VM build version.
URLs and SHA256 digests are pinned in `internal/services/ec2/vm_images.go`, with official checksum
manifest source URLs. AL2 has no default VM mapping and fails launch in Lima mode.
Override a catalogue AMI's image with a verified HTTPS URL and SHA256/SHA512 digest:

```yaml
ec2:
  runtime: lima
  lima:
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
