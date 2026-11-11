package qemu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
)

func TestImageCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image" {
			http.NotFound(w, r)
			return
		}

		calls.Add(1)
		_, _ = w.Write([]byte("test qcow2"))
	}))
	defer server.Close()
	image := config.VMImage{
		URL:    server.URL + "/image",
		Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("test qcow2"))),
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := fetch(ctx, dir, image); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}

	image.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := fetch(ctx, dir, image); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("mismatch: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial image remains: %v %v", entries, err)
	}

	cached := filepath.Join(dir, entries[0].Name())
	if err := os.WriteFile(cached, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	image.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("test qcow2")))
	if _, err := fetch(ctx, dir, image); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt cache: %v", err)
	}

	if _, err := os.Stat(cached); !os.IsNotExist(err) {
		t.Fatal("corrupt entry retained", err)
	}
}

func TestArguments(t *testing.T) {
	firmware := filepath.Join(t.TempDir(), "edk2-aarch64-code.fd")
	if err := os.WriteFile(firmware, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default().EC2.QEMU
	cfg.Firmware = firmware
	r := New(&cfg)
	for _, tc := range []struct{ os, host, guest, want string }{
		{"darwin", "arm64", "arm64", "hvf"},
		{"darwin", "amd64", "x86_64", "hvf"},
		{"windows", "amd64", "x86_64", "whpx"},
		{"linux", "arm64", "arm64", "kvm"},
	} {
		accel, err := accelerator(tc.os, tc.host, tc.guest, false, true)
		if err != nil || accel != tc.want {
			t.Fatalf("%+v: %s %v", tc, accel, err)
		}

		// Linux command generation checks the real /dev/kvm; allow emulation in this test only.
		r.cfg.AllowTCG = tc.os == "linux"
		args, err := r.arguments(
			&ec2.VMSpec{Name: "tinystack-ec2-test", Arch: tc.guest, Port: 32123, CPUs: 2, MemoryMiB: 1024},
			t.TempDir(),
			32124,
			"tcp:127.0.0.1:32125",
			tc.os,
			tc.host,
		)
		if err != nil {
			t.Fatal(err)
		}

		line := strings.Join(args, " ")
		for _, expected := range []string{
			"hostfwd=tcp:127.0.0.1:32124-:22", "ds=nocloud-net;s=http://10.0.2.2:32123/", "format=qcow2",
		} {
			if !strings.Contains(line, expected) {
				t.Fatalf("missing %s: %s", expected, line)
			}
		}

		if (tc.os != "windows") != strings.Contains(line, "-daemonize") {
			t.Fatal(line)
		}

		if tc.guest == "arm64" && !strings.Contains(line, firmware) {
			t.Fatal(line)
		}
	}

	if _, err := accelerator("linux", "amd64", "arm64", false, true); err == nil {
		t.Fatal("cross arch accepted")
	}

	if _, err := accelerator("linux", "amd64", "x86_64", false, false); err == nil {
		t.Fatal("missing KVM accepted")
	}

	if got, err := accelerator("windows", "arm64", "arm64", true, false); err != nil || got != "tcg" {
		t.Fatal(got, err)
	}
}

// A Go executable re-exec fake works without Python or a shell, including Windows.
func TestMain(m *testing.M) {
	if os.Getenv("TINYSTACK_FAKE_QEMU") == "1" {
		os.Exit(fakeCommand())
	}

	os.Exit(m.Run())
}

func fakeCommand() int {
	args := os.Args[1:]
	recordFake(args)
	if len(args) == 0 {
		return 2
	}

	switch args[0] {
	case "create":
		if err := os.WriteFile(args[len(args)-1], nil, 0o600); err != nil {
			return 2
		}

		return 0
	case "info":
		fmt.Println(`{"virtual-size":1073741824}`)
		return 0
	case "resize":
		return 0
	}

	value := func(flag string) string {
		for i, arg := range args {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}

		return ""
	}

	if os.Getenv("TINYSTACK_FAKE_QEMU_CHILD") != "1" && value("-daemonize") == "" {
		// -daemonize has no value; scan it explicitly below.
		daemon := false
		for _, arg := range args {
			if arg == "-daemonize" {
				daemon = true
			}
		}

		if daemon {
			cmd := exec.CommandContext(context.Background(), os.Args[0], args...)
			cmd.Env = append(os.Environ(), "TINYSTACK_FAKE_QEMU_CHILD=1")
			if cmd.Start() != nil {
				return 2
			}

			go func() { _ = cmd.Wait() }()
			return 0
		}
	}

	endpoint := strings.Split(value("-qmp"), ",")[0]
	network, address, _ := strings.Cut(endpoint, ":")
	listener, err := net.Listen(network, address)
	if err != nil {
		return 2
	}

	defer listener.Close()
	netarg := value("-netdev")
	if _, tail, ok := strings.Cut(netarg, "hostfwd=tcp:"); ok {
		hostport, _, _ := strings.Cut(tail, "-:22")
		ssh, err := net.Listen("tcp", hostport)
		if err != nil {
			return 2
		}

		defer ssh.Close()
		go func() {
			for {
				c, e := ssh.Accept()
				if e != nil {
					return
				}

				_, _ = c.Write([]byte("SSH-2.0-fake\r\n"))
				c.Close()
			}
		}()
	}

	pid := value("-pidfile")
	if err := os.WriteFile(pid, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		return 2
	}

	defer os.Remove(pid)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return 0
		}

		encoder, decoder := json.NewEncoder(conn), json.NewDecoder(conn)
		_ = encoder.Encode(map[string]any{"QMP": map[string]any{}})
		stop := false
		for {
			var command map[string]string
			if decoder.Decode(&command) != nil {
				break
			}

			recordFake([]string{command["execute"]})
			_ = encoder.Encode(map[string]any{"return": map[string]any{}})
			if command["execute"] == "quit" || command["execute"] == "system_powerdown" {
				stop = true
				break
			}
		}

		conn.Close()
		if stop {
			return 0
		}
	}
}

func TestProcessLifecycle(t *testing.T) {
	t.Setenv("TINYSTACK_FAKE_QEMU", "1")
	cfg := config.Default().EC2.QEMU
	cfg.SystemCommand = map[string]string{"x86_64": os.Args[0]}
	cfg.StateDir = t.TempDir()
	r := New(&cfg)
	name := "tinystack-ec2-owner-i-test"
	dir, err := r.directory(name)
	if err != nil {
		t.Fatal(err)
	}

	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}

	// Use TCP for the fake to avoid platform Unix socket path length limits.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port, err := freePort(ctx)
	if err != nil {
		t.Fatal(err)
	}

	machine := &machine{
		Spec:    ec2.VMSpec{Name: name, Arch: "x86_64"},
		Network: "tcp",
		Address: fmt.Sprintf("127.0.0.1:%d", port),
	}

	data, _ := json.Marshal(machine)
	if writeErr := os.WriteFile(filepath.Join(dir, "machine.json"), data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	_, err = r.run(ctx, os.Args[0], true, "-qmp", "tcp:"+machine.Address, "-pidfile", filepath.Join(dir, "qemu.pid"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = qmp(context.Background(), machine, "quit") })
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for qmp(ctx, machine, "query-status") != nil {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}

	if err := r.Action(ctx, name, "restart"); err != nil {
		t.Fatal(err)
	}

	if err := r.Action(ctx, name, "stop"); err != nil {
		t.Fatal(err)
	}

	if err := r.Action(ctx, name, "delete"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("state not removed", err)
	}

	if runtime.GOOS == "windows" {
		t.Log("Windows detached helper completed")
	}
}

func recordFake(args []string) {
	path := os.Getenv("TINYSTACK_FAKE_QEMU_LOG")
	if path == "" {
		return
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}

	defer file.Close()
	data, _ := json.Marshal(args)
	_, _ = file.Write(append(data, '\n'))
}
