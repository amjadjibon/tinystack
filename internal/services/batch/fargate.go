package batch

import (
	"slices"
	"strconv"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	legacyLinuxVersion     = "1.3.0"
	x86Architecture        = "X86_64"
	fargatePlatform        = "FARGATE"
	latestPlatform         = "LATEST"
	windowsPlatformVersion = "1.0.0"
	linuxOS                = "LINUX"
	resourceMemory         = "MEMORY"
	quarterVCPU            = "0.25"
)

func isFargate(typ string) bool { return typ == fargatePlatform || typ == "FARGATE_SPOT" }

// Fargate options are validated metadata; the local runtime does not implement Fargate networking or platforms.
func platformOK(c *api.ContainerProperties, fargate bool) error {
	if !fargate {
		if c.NetworkConfiguration != nil || c.FargatePlatformConfiguration != nil || c.RuntimePlatform != nil {
			return invalid("Fargate container options require the FARGATE platform")
		}

		return nil
	}

	if c.ExecutionRoleArn == nil {
		return invalid("Fargate requires executionRoleArn")
	}

	if c.Vcpus != nil || c.Memory != nil {
		return invalid("Fargate requires resourceRequirements instead of vcpus/memory")
	}

	if n := c.NetworkConfiguration; n != nil && n.AssignPublicIp != nil &&
		!slices.Contains([]string{"ENABLED", "DISABLED"}, *n.AssignPublicIp) {
		return invalid("assignPublicIp must be ENABLED or DISABLED")
	}

	version := latestPlatform
	if p := c.FargatePlatformConfiguration; p != nil && p.PlatformVersion != nil {
		version = *p.PlatformVersion
	}

	if !slices.Contains([]string{latestPlatform, legacyLinuxVersion, "1.4.0", windowsPlatformVersion}, version) {
		return invalid("Unsupported Fargate platformVersion")
	}

	arch, os := x86Architecture, linuxOS
	if r := c.RuntimePlatform; r != nil {
		if r.CpuArchitecture != nil {
			arch = *r.CpuArchitecture
		}

		if r.OperatingSystemFamily != nil {
			os = *r.OperatingSystemFamily
		}
	}

	if !slices.Contains([]string{x86Architecture, "ARM64"}, arch) ||
		!slices.Contains(
			[]string{
				linuxOS,
				"WINDOWS_SERVER_2019_CORE",
				"WINDOWS_SERVER_2019_FULL",
				"WINDOWS_SERVER_2022_CORE",
				"WINDOWS_SERVER_2022_FULL",
			},
			os,
		) {
		return invalid("Unsupported runtimePlatform")
	}

	if os != linuxOS && (arch != x86Architecture || (version != latestPlatform && version != windowsPlatformVersion)) {
		return invalid("Windows requires X86_64 and platform LATEST or 1.0.0")
	}

	if os == linuxOS && (version == windowsPlatformVersion || arch == "ARM64" && version == legacyLinuxVersion) {
		return invalid("Unsupported Linux architecture/platform version combination")
	}

	cpu, memory := "", int64(0)
	for _, r := range c.ResourceRequirements {
		if common.Value(r.Type) == resourceVCPU {
			cpu = common.Value(r.Value)
		}

		if common.Value(r.Type) == resourceMemory {
			memory, _ = strconv.ParseInt(common.Value(r.Value), 10, 32)
		}
	}

	type bounds struct{ min, max, step int64 }
	sizes := map[string]bounds{
		quarterVCPU: {512, 2048, 512}, "0.5": {1024, 4096, 1024}, "1": {2048, 8192, 1024},
		"2": {4096, 16384, 1024}, "4": {8192, 30720, 1024}, "8": {16384, 61440, 4096},
		"16": {32768, 122880, 8192},
	}

	b, ok := sizes[cpu]
	valid := ok && memory >= b.min && memory <= b.max && (memory-b.min)%b.step == 0
	if cpu == quarterVCPU {
		valid = memory == 512 || memory == 1024 || memory == 2048
	}

	if cpu == "32" {
		valid = memory == 61440 || memory == 122880 || memory == 249856
	}

	if !valid {
		return invalid("Invalid Fargate VCPU/MEMORY combination")
	}

	if os != linuxOS && (cpu == quarterVCPU || cpu == "0.5") {
		return invalid("Windows Fargate requires at least 1 VCPU")
	}

	if (cpu == "8" || cpu == "16" || cpu == "32") && (os != linuxOS || version == legacyLinuxVersion) {
		return invalid("Large Fargate sizes require Linux platform 1.4.0 or LATEST")
	}

	return nil
}
