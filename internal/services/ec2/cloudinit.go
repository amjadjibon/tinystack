package ec2

import (
	"encoding/base64"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	vmWriteFiles = "write_files"
	vmRunCmd     = "runcmd"
	vmBootcmd    = "bootcmd"
	vmPackages   = "package_update_upgrade_install"
)

// Cloud-config modules supported by the VM adapter. Unknown directives fail at launch.
var vmCloudModules = map[string]string{
	vmBootcmd: vmBootcmd, vmWriteFiles: vmWriteFiles, "users": "users_groups",
	vmRunCmd: vmRunCmd, "packages": vmPackages,
	"package_update": vmPackages, "package_upgrade": vmPackages,
}

func validateVMUserData(data string) error {
	if data == "" || strings.HasPrefix(data, "#!") {
		return nil
	}

	if !strings.HasPrefix(data, "#cloud-config") {
		return invalidParameter("VM user data must be a shell script or #cloud-config")
	}

	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return invalidParameter("invalid cloud-config: " + err.Error())
	}

	for key := range cfg {
		if vmCloudModules[key] == "" {
			return invalidParameter("unsupported VM cloud-config directive: " + key)
		}
	}

	return nil
}

// GuestSetup generates driver-independent provisioning with a driver-selected host gateway.
func GuestSetup(spec *VMSpec, host, address string) (boot, setup string, err error) {
	if err := validateVMUserData(spec.UserData); err != nil {
		return "", "", err
	}

	endpoint := fmt.Sprintf("http://%s:%d", host, spec.Port)
	env := fmt.Sprintf(
		"AWS_ENDPOINT_URL=%s\nAWS_DEFAULT_REGION=%s\nAWS_REGION=%s\n",
		endpoint,
		spec.Region,
		spec.Region,
	)
	// The NAT rule affects only the guest. Lima maps its host gateway to host loopback.
	boot = fmt.Sprintf(`set -eu
if command -v nft >/dev/null 2>&1; then
 nft list table ip tinystack >/dev/null 2>&1 && nft delete table ip tinystack
 nft -f - <<'NFT'
table ip tinystack {
 chain output {
  type nat hook output priority -100; policy accept;
  ip daddr 169.254.169.254 tcp dport 80 dnat to %s:%d
 }
}

NFT
elif command -v iptables >/dev/null 2>&1; then
 iptables -t nat -C OUTPUT -d 169.254.169.254/32 -p tcp --dport 80 -j DNAT --to-destination %s:%d 2>/dev/null ||
 iptables -t nat -A OUTPUT -d 169.254.169.254/32 -p tcp --dport 80 -j DNAT --to-destination %s:%d
else
 echo 'TinyStack IMDS requires nftables or iptables' >&2
 exit 1
fi
`, address, spec.Port, address, spec.Port, address, spec.Port)
	setup = "#!/bin/sh\nset -eu\n" +
		"printf '%s' '" + base64.StdEncoding.EncodeToString([]byte(env)) + "' | base64 -d > /etc/tinystack.env\n" +
		"sed -i '/^AWS_ENDPOINT_URL=/d; /^AWS_DEFAULT_REGION=/d; /^AWS_REGION=/d' /etc/environment\n" +
		"cat /etc/tinystack.env >> /etc/environment\nset -a\n. /etc/tinystack.env\nset +a\n"
	if spec.PublicKey != "" {
		setup += fmt.Sprintf("id %s >/dev/null 2>&1 || useradd -m -s /bin/bash %s\n", spec.User, spec.User) +
			fmt.Sprintf("install -d -m 700 -o %s /home/%s/.ssh\n", spec.User, spec.User) +
			fmt.Sprintf("printf '%%s' '%s' | base64 -d > /home/%s/.ssh/authorized_keys\n",
				base64.StdEncoding.EncodeToString([]byte(spec.PublicKey)), spec.User) +
			fmt.Sprintf("chown %s /home/%s/.ssh/authorized_keys\nchmod 600 /home/%s/.ssh/authorized_keys\n",
				spec.User, spec.User, spec.User)
	}

	setup += `command -v nft >/dev/null 2>&1 || command -v iptables >/dev/null 2>&1 || {
 if command -v apt-get >/dev/null; then
 apt-get update && apt-get install -y nftables
 else
 dnf install -y nftables
 fi
}

` + boot
	setup += "[ ! -e /var/lib/tinystack-user-data.done ] || exit 0\n"
	if spec.UserData != "" {
		setup += "printf '%s' '" + base64.StdEncoding.EncodeToString([]byte(spec.UserData)) +
			"' | base64 -d > /var/lib/tinystack-user-data\n"
		if strings.HasPrefix(spec.UserData, "#!") {
			setup += "chmod 700 /var/lib/tinystack-user-data\n/var/lib/tinystack-user-data\n"
		} else {
			var cfg map[string]any
			_ = yaml.Unmarshal([]byte(spec.UserData), &cfg)
			for _, module := range []string{vmBootcmd, "users_groups", vmWriteFiles, vmPackages, vmRunCmd} {
				present := false
				for key := range cfg {
					if vmCloudModules[key] == module {
						present = true
					}
				}

				if present {
					setup += "cloud-init single --name cc_" + module +
						" --frequency always --file /var/lib/tinystack-user-data\n"
				}
			}

			if _, ok := cfg[vmRunCmd]; ok {
				setup += "sh /var/lib/cloud/instance/scripts/runcmd\n"
			}
		}
	}

	setup += "touch /var/lib/tinystack-user-data.done\n"
	return boot, setup, nil
}

// NoCloud delivers the shared setup through cloud-init, without an ISO.
func NoCloud(spec *VMSpec) ([]byte, error) {
	boot, setup, err := GuestSetup(spec, "10.0.2.2", "10.0.2.2")
	if err != nil {
		return nil, err
	}

	data, err := yaml.Marshal(map[string]any{
		"bootcmd": []any{[]string{"sh", "-c", boot}},
		"runcmd":  []any{[]string{"sh", "-c", setup}},
	})
	return append([]byte("#cloud-config\n"), data...), err
}

// LimaConfig generates YAML without host mounts or implicit port forwards.
func LimaConfig(spec *VMSpec) ([]byte, error) {
	boot, setup, err := GuestSetup(spec, "host.lima.internal", "192.168.5.2")
	if err != nil {
		return nil, err
	}

	arch := spec.Arch
	if arch == archARM {
		arch = "aarch64"
	}

	rootGiB := spec.RootGiB
	if rootGiB == 0 {
		rootGiB = rootVolumeSize
	}

	return yaml.Marshal(map[string]any{
		"vmType": tenancyDefault, "arch": arch,
		"images": []any{map[string]any{"location": spec.Image.URL, "digest": spec.Image.Digest, "arch": arch}},
		"cpus":   spec.CPUs, "memory": fmt.Sprintf("%dMiB", spec.MemoryMiB), "disk": fmt.Sprintf("%dGiB", rootGiB),
		"mounts": []any{}, "containerd": map[string]bool{"system": false, "user": false},
		"ssh":          map[string]any{"localPort": 0, "loadDotSSHPubKeys": false},
		"portForwards": []any{map[string]any{"guestPortRange": []int{1, 65535}, "ignore": true}},
		"provision": []any{
			map[string]string{"mode": "boot", "script": boot},
			map[string]string{"mode": "system", "script": setup},
		},
	})
}
