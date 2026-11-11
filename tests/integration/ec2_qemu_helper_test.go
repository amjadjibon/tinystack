package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
