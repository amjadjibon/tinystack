package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/ec2"
)

func (r *Runtime) firmware(binary string) (string, error) {
	if r.cfg.Firmware != "" {
		if _, err := os.Stat(r.cfg.Firmware); err != nil {
			return "", fmt.Errorf("QEMU ARM firmware: %w", err)
		}

		return r.cfg.Firmware, nil
	}

	resolved, err := exec.LookPath(binary)
	if err != nil {
		return "", err
	}

	if path, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = path
	}

	root := filepath.Dir(filepath.Dir(resolved))
	for _, candidate := range []string{
		filepath.Join(root, "share", "qemu", "edk2-aarch64-code.fd"),
		filepath.Join(filepath.Dir(resolved), "share", "edk2-aarch64-code.fd"),
		"/usr/share/qemu/edk2-aarch64-code.fd", "/usr/share/AAVMF/AAVMF_CODE.fd",
		"/usr/share/edk2/aarch64/QEMU_EFI.fd", "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("ARM UEFI firmware missing; set ec2.qemu.firmware to edk2-aarch64-code.fd")
}

func accelerator(osName, hostArch, guestArch string, allowTCG, kvm bool) (string, error) {
	host := map[string]string{"amd64": guestX86, guestARM: guestARM}[hostArch]
	accel := ""
	if host == guestArch {
		switch osName {
		case "darwin":
			accel = "hvf"
		case "linux":
			if kvm {
				accel = "kvm"
			}

		case osWindows:
			if guestArch == guestX86 {
				accel = "whpx"
			}
		}
	}

	if allowTCG {
		if accel == "" {
			return "tcg", nil
		}

		return accel, nil
	}

	if accel == "" {
		return "", fmt.Errorf(
			"QEMU hardware acceleration unavailable for %s/%s guest %s; check /dev/kvm access or enable ec2.qemu.allow_tcg",
			osName,
			hostArch,
			guestArch,
		)
	}

	return accel, nil
}

func (r *Runtime) arguments(
	spec *ec2.VMSpec,
	dir string,
	port int,
	qmpAddress, osName, hostArch string,
) ([]string, error) {
	if spec.Arch != guestARM && spec.Arch != guestX86 {
		return nil, fmt.Errorf("unsupported QEMU architecture %q", spec.Arch)
	}

	kvm := false
	if osName == "linux" {
		// #nosec G304 -- fixed device, opened only to check read/write access; no ioctl or VM creation.
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err == nil {
			kvm = true
			_ = f.Close()
		}
	}

	accel, err := accelerator(osName, hostArch, spec.Arch, r.cfg.AllowTCG, kvm)
	if err != nil {
		return nil, err
	}

	machineType, cpu := "q35", "max"
	if spec.Arch == guestARM {
		machineType = "virt"
		cpu = "cortex-a72"
	}

	if !strings.Contains(accel, "tcg") {
		cpu = "host"
	}

	// QEMU option values escape commas by doubling them. Semicolons are literal
	// argv bytes here: shell/GRUB escaping would corrupt the NoCloud serial string.
	escape := func(s string) string { return strings.ReplaceAll(s, ",", ",,") }
	args := []string{
		"-name", spec.Name, "-machine", machineType + ",accel=" + accel, "-cpu", cpu,
		"-smp", strconv.Itoa(spec.CPUs), "-m", strconv.Itoa(spec.MemoryMiB),
		"-display", "none", "-monitor", "none", "-serial", "file:" + filepath.Join(dir, "serial.log"),
		"-pidfile", filepath.Join(dir, "qemu.pid"), "-qmp", escape(qmpAddress) + ",server=on,wait=off",
		"-drive", "file=" + escape(filepath.Join(dir, "root.qcow2")) + ",if=virtio,format=qcow2",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", port),
		"-device", "virtio-net-pci,netdev=net0",
		"-smbios", fmt.Sprintf("type=1,serial=ds=nocloud-net;s=http://10.0.2.2:%d/", spec.Port),
	}

	if spec.Arch == guestARM {
		firmware, err := r.firmware(r.cfg.SystemCommand[spec.Arch])
		if err != nil {
			return nil, err
		}

		args = append(args, "-bios", firmware)
	}

	if osName != osWindows {
		args = append(args, "-daemonize")
	}

	return args, nil
}

const (
	guestARM     = "arm64"
	guestX86     = "x86_64"
	osWindows    = "windows"
	networkUnix  = "unix"
	actionDelete = "delete"
)
