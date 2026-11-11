// Package qemu implements store-owned, detached QEMU guests using user-mode networking.
package qemu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
)

var _ ec2.Runtime = (*Runtime)(nil)

type Runtime struct {
	mu  sync.Map
	cfg config.QEMUConfig
}

type machine struct {
	Network string
	Address string
	Spec    ec2.VMSpec
	Port    int
}

func New(input *config.QEMUConfig) *Runtime {
	cfg := *input
	cache, err := os.UserCacheDir()
	if err == nil {
		if cfg.CacheDir == "" {
			cfg.CacheDir = filepath.Join(cache, "tinystack", "ec2-images")
		}

		if cfg.StateDir == "" {
			cfg.StateDir = filepath.Join(cache, "tinystack", "ec2-vms")
		}
	}

	if cfg.CacheDir != "" {
		if absolute, pathErr := filepath.Abs(cfg.CacheDir); pathErr == nil {
			cfg.CacheDir = absolute
		}
	}

	if cfg.StateDir != "" {
		if absolute, pathErr := filepath.Abs(cfg.StateDir); pathErr == nil {
			cfg.StateDir = absolute
		}
	}

	return &Runtime{cfg: cfg}
}

// run is the single executable boundary. Detached Windows children never inherit
// the request context; Unix QEMU forks itself with -daemonize.
func (r *Runtime) run(ctx context.Context, binary string, background bool, args ...string) ([]byte, error) {
	// #nosec G204 G702 -- user-configured executable; arguments never pass through a shell.
	cmd := exec.CommandContext(ctx, binary, args...)
	if background {
		// #nosec G204 G702 -- user-configured executable, detached from the request context.
		cmd = exec.CommandContext(context.WithoutCancel(ctx), binary, args...)
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}

		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		endpoint := ""
		for n, arg := range args {
			if arg == "-qmp" && n+1 < len(args) {
				endpoint = strings.Split(args[n+1], ",")[0]
			}
		}

		network, address, _ := strings.Cut(endpoint, ":")
		m := &machine{Network: network, Address: address}
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			if qmp(startup, m, "query-status") == nil {
				return nil, nil
			}

			select {
			case exitErr := <-exited:
				return nil, fmt.Errorf(
					"detached QEMU exited before QMP startup: %w (check accelerator and firmware)",
					exitErr,
				)
			case <-startup.Done():
				_ = cmd.Process.Kill()
				return nil, fmt.Errorf("detached QEMU QMP startup failed: %w", startup.Err())
			case <-ticker.C:
			}
		}

	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", binary, err, stderr.String())
	}

	return out, nil
}

func (r *Runtime) directory(name string) (string, error) {
	if r.cfg.StateDir == "" || !strings.HasPrefix(name, "tinystack-ec2-") || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\\:,`) {
		return "", fmt.Errorf("invalid QEMU state directory or ownership name %q", name)
	}

	return filepath.Join(r.cfg.StateDir, name), nil
}

func (r *Runtime) Resources(_ context.Context, owner string) ([]string, error) {
	if !strings.HasPrefix(owner, "tinystack-ec2-") || owner == "tinystack-ec2-" {
		return nil, fmt.Errorf("invalid QEMU ownership prefix")
	}

	entries, err := os.ReadDir(r.cfg.StateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), owner) {
			names = append(names, entry.Name())
		}
	}

	return names, nil
}

func freePort(ctx context.Context) (int, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (r *Runtime) Ensure(ctx context.Context, spec *ec2.VMSpec) error {
	unlock := r.lock(spec.Name)
	defer unlock()
	dir, err := r.directory(spec.Name)
	if err != nil {
		return err
	}

	if _, statErr := os.Stat(filepath.Join(dir, "machine.json")); statErr == nil {
		return r.start(ctx, spec.Name)
	}

	// Validate acceleration and firmware before downloading or creating any disk.
	if _, argsErr := r.arguments(
		spec,
		dir,
		0,
		"unix:"+filepath.Join(dir, "qmp.sock"),
		runtime.GOOS,
		runtime.GOARCH,
	); argsErr != nil {
		return argsErr
	}

	base, err := fetch(ctx, r.cfg.CacheDir, spec.Image)
	if err != nil {
		return err
	}

	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return mkdirErr
	}

	overlay := filepath.Join(dir, "root.qcow2")
	if _, runErr := r.run(
		ctx,
		r.cfg.ImgCommand,
		false,
		"create",
		"-f",
		"qcow2",
		"-F",
		"qcow2",
		"-b",
		base,
		overlay,
	); runErr != nil {
		return runErr
	}

	// Never shrink a base image: inspect its virtual size first.
	info, err := r.run(ctx, r.cfg.ImgCommand, false, "info", "--output=json", overlay)
	if err != nil {
		return err
	}

	var size struct {
		VirtualSize int64 `json:"virtual-size"`
	}

	if decodeErr := json.Unmarshal(info, &size); decodeErr != nil {
		return decodeErr
	}

	if int64(spec.RootGiB)*1024*1024*1024 > size.VirtualSize {
		if _, runErr := r.run(
			ctx,
			r.cfg.ImgCommand,
			false,
			"resize",
			overlay,
			strconv.Itoa(spec.RootGiB)+"G",
		); runErr != nil {
			return runErr
		}
	}

	port, err := freePort(ctx)
	if err != nil {
		return err
	}

	m := machine{
		Spec:    *spec,
		Port:    port,
		Network: networkUnix,
		Address: filepath.Join(r.cfg.StateDir, fmt.Sprintf("%x.sock", sha256.Sum256([]byte(spec.Name)))[:21]),
	}

	if runtime.GOOS == osWindows {
		qmpPort, portErr := freePort(ctx)
		if portErr != nil {
			return portErr
		}

		m.Network, m.Address = "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(qmpPort))
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "machine.json"), data, 0o600); err != nil {
		return err
	}

	return r.start(ctx, spec.Name)
}

func (r *Runtime) load(name string) (*machine, string, error) {
	dir, err := r.directory(name)
	if err != nil {
		return nil, "", err
	}

	// #nosec G304 -- validated store-owned state path.
	data, err := os.ReadFile(filepath.Join(dir, "machine.json"))
	if err != nil {
		return nil, dir, err
	}

	var m machine
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, dir, err
	}

	return &m, dir, nil
}

func (r *Runtime) start(ctx context.Context, name string) error {
	m, dir, err := r.load(name)
	if err != nil {
		return err
	}

	if qmp(ctx, m, "query-status") == nil {
		return nil
	}

	if m.Network == networkUnix {
		_ = os.Remove(m.Address)
	}

	args, err := r.arguments(&m.Spec, dir, m.Port, m.Network+":"+m.Address, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	_, err = r.run(ctx, r.cfg.SystemCommand[m.Spec.Arch], runtime.GOOS == osWindows, args...)
	if err != nil && r.cfg.AllowTCG {
		args, err = r.arguments(&m.Spec, dir, m.Port, m.Network+":"+m.Address, runtime.GOOS, "")
		if err == nil {
			_, err = r.run(ctx, r.cfg.SystemCommand[m.Spec.Arch], runtime.GOOS == osWindows, args...)
		}
	}

	if err != nil {
		return fmt.Errorf("QEMU acceleration/launch failed (enable ec2.qemu.allow_tcg for emulation): %w", err)
	}

	return nil
}

func (r *Runtime) Action(ctx context.Context, name, action string) error {
	unlock := r.lock(name)
	defer unlock()
	m, dir, err := r.load(name)
	if action == actionDelete && os.IsNotExist(err) {
		if dir == "" {
			return err
		}

		return os.RemoveAll(dir)
	}

	if err != nil {
		return err
	}

	switch action {
	case "start":
		return r.start(ctx, name)
	case "restart":
		return qmp(ctx, m, "system_reset")
	case "stop", actionDelete:
		statusErr := qmp(ctx, m, "query-status")
		if statusErr != nil {
			if _, pidErr := os.Stat(filepath.Join(dir, "qemu.pid")); pidErr == nil {
				return fmt.Errorf(
					"cannot contact owned QEMU with a pidfile; retaining state for recovery: %w",
					statusErr,
				)
			}
		}

		if statusErr == nil {
			command := "system_powerdown"
			if action == actionDelete {
				command = "quit"
			}

			if err := qmp(ctx, m, command); err != nil {
				return err
			}

			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for qmp(ctx, m, "query-status") == nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-deadline.C:
					if killErr := killOwned(dir); killErr != nil {
						return killErr
					}

					if action == actionDelete {
						return os.RemoveAll(dir)
					}

					return nil
				case <-ticker.C:
				}
			}
		}

		if action == actionDelete {
			if m.Network == networkUnix {
				_ = os.Remove(m.Address)
			}

			return os.RemoveAll(dir)
		}

		return nil
	default:
		return fmt.Errorf("unsupported QEMU action %q", action)
	}
}

func (r *Runtime) Ready(ctx context.Context, name string) (port int, ready bool, err error) {
	unlock := r.lock(name)
	defer unlock()
	m, _, err := r.load(name)
	if err != nil {
		return 0, false, err
	}

	if qmp(ctx, m, "query-status") != nil {
		return m.Port, false, nil
	}

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(
		ctx,
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Port)),
	)
	if err != nil {
		return m.Port, false, nil
	}

	defer func() { _ = conn.Close() }()
	if deadlineErr := conn.SetDeadline(time.Now().Add(time.Second)); deadlineErr != nil {
		return 0, false, deadlineErr
	}

	banner := make([]byte, 255)
	n, err := conn.Read(banner)
	return m.Port, err == nil && strings.HasPrefix(string(banner[:n]), "SSH-2.0-"), nil
}

func qmp(ctx context.Context, m *machine, command string) error {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, m.Network, m.Address)
	if err != nil {
		return err
	}

	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	var greeting map[string]any
	if err := decoder.Decode(&greeting); err != nil {
		return err
	}

	if greeting["QMP"] == nil {
		return fmt.Errorf("invalid QMP greeting")
	}

	for _, cmd := range []string{"qmp_capabilities", command} {
		if err := encoder.Encode(map[string]string{"execute": cmd}); err != nil {
			return err
		}

		for {
			var response map[string]json.RawMessage
			if err := decoder.Decode(&response); err != nil {
				return err
			}

			if response["error"] != nil {
				return fmt.Errorf("QMP %s: %s", cmd, response["error"])
			}

			if response["return"] != nil {
				break
			}
		}
	}

	return nil
}

// lock serializes a single guest while other guests can download, boot and stop independently.
func (r *Runtime) lock(name string) func() {
	value, _ := r.mu.LoadOrStore(name, new(sync.Mutex))
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// killOwned is used only after a successful connection to this guest's private
// QMP endpoint and an expired ACPI shutdown deadline. Never signal enumerated PIDs.
func killOwned(dir string) error {
	// #nosec G304 -- validated owned directory; QEMU creates this pidfile.
	data, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
	if err != nil {
		return err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid QEMU pidfile")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	defer func() { _ = process.Release() }()
	return process.Kill()
}
