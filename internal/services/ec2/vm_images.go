package ec2

import (
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/config"
)

// Official checksum manifests (read without downloading images):
// https://cdn.amazonlinux.com/al2023/os-images/2023.7.20250609.0/kvm/SHA256SUMS
// https://cdn.amazonlinux.com/al2023/os-images/2023.7.20250609.0/kvm-arm64/SHA256SUMS
// https://cloud-images.ubuntu.com/releases/noble/release-20260926/SHA256SUMS
var vmImages = map[string]config.VMImage{
	"amazon/x86_64": {
		URL: "https://cdn.amazonlinux.com/al2023/os-images/2023.7.20250609.0/kvm/" +
			"al2023-kvm-2023.7.20250609.0-kernel-6.1-x86_64.xfs.gpt.qcow2",
		Digest: "sha256:55218a566db332f4c38c5b83e5c7063bc36d0f603f009dbfef7d5cf3b9369abe",
	},
	"amazon/arm64": {
		URL: "https://cdn.amazonlinux.com/al2023/os-images/2023.7.20250609.0/kvm-arm64/" +
			"al2023-kvm-2023.7.20250609.0-kernel-6.1-arm64.xfs.gpt.qcow2",
		Digest: "sha256:aecdd5a82d103bcb2c44548c0a58a3f33d54cac353b1858c5a23c2bcd21f0166",
	},
	"ubuntu/x86_64": {
		URL:    "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-amd64.img",
		Digest: "sha256:6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2",
	},
	"ubuntu/arm64": {
		URL:    "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-arm64.img",
		Digest: "sha256:1d6bffe64b848468ac97f821d369a4846d983de1800ccf6b5ec8853e85cefc55",
	},
}

func vmImage(img *image, id string, overrides map[string]config.VMImage) (config.VMImage, string, error) {
	user, family := "ec2-user", "amazon"
	if img.Owner == canonicalOwner {
		user, family = "ubuntu", "ubuntu"
	}

	if override, ok := overrides[id]; ok {
		return override, user, nil
	}

	if !strings.HasPrefix(img.Name, "al2023-") && img.Owner != canonicalOwner {
		return config.VMImage{}, "", invalidParameter("AMI has no VM image mapping: " + id)
	}

	mapped, ok := vmImages[family+"/"+img.Architecture]
	if !ok {
		return config.VMImage{}, "", invalidParameter("unsupported VM image architecture")
	}

	return mapped, user, nil
}

// VMSize reports real vCPUs and memory in MiB, rather than silently rounding down.
func VMSize(instanceType string) (cpus, memory int, err error) {
	family, size, _ := strings.Cut(instanceType, ".")
	switch family {
	case "t3", vmT4g, "m5", "m7g", "c5":
	default:
		return 0, 0, invalidParameter("instance type has no VM sizing: " + instanceType)
	}

	units := map[string]int{
		"large": 1, "xlarge": 2, "2xlarge": 4, "4xlarge": 8, "8xlarge": 16,
		"12xlarge": 24, "16xlarge": 32,
	}

	if family == "t3" || family == vmT4g {
		small := map[string]int{"nano": 512, "micro": 1024, "small": 2048, "medium": 4096}
		if m, ok := small[size]; ok {
			return 2, m, nil
		}
	}

	unit, ok := units[size]
	if !ok {
		return 0, 0, invalidParameter(fmt.Sprintf("unsupported VM instance size: %s", instanceType))
	}

	ratio := 4096
	if family == "c5" {
		ratio = 2048
	}

	return 2 * unit, 2 * unit * ratio, nil
}
