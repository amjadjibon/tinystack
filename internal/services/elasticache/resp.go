package elasticache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	errProbeFailed   = errors.New("engine probe failed")
	errConfigSetFail = errors.New("redis config set failed")
)

func probeNode(
	ctx context.Context,
	engine string,
	host string,
	port int64,
	password string,
	timeout time.Duration,
) error {
	switch engine {
	case EngineRedis, EngineValkey:
		return probeRedis(ctx, host, port, password, timeout)
	case EngineMemcached:
		return probeMemcached(ctx, host, port, timeout)
	default:
		return fmt.Errorf("unknown engine %q", engine)
	}
}

func probeRedis(ctx context.Context, host string, port int64, password string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	reader := bufio.NewReader(conn)

	if password != "" {
		authCmd := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password)
		if _, err = conn.Write([]byte(authCmd)); err != nil {
			return err
		}
		line, rErr := reader.ReadString('\n')
		if rErr != nil {
			return rErr
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "+OK") {
			return fmt.Errorf("%w: auth failed: %s", errProbeFailed, strings.TrimSpace(line))
		}
	}

	if _, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	if !strings.Contains(line, "PONG") {
		return fmt.Errorf("%w: unexpected response %q", errProbeFailed, strings.TrimSpace(line))
	}

	return nil
}

func probeMemcached(ctx context.Context, host string, port int64, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err = conn.Write([]byte("version\r\n")); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	if !strings.HasPrefix(line, "VERSION") {
		return fmt.Errorf("%w: unexpected response %q", errProbeFailed, strings.TrimSpace(line))
	}

	return nil
}

func redisExec(
	ctx context.Context,
	host string,
	port int64,
	password string,
	args []string,
	timeout time.Duration,
) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	reader := bufio.NewReader(conn)

	if password != "" {
		authCmd := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password)
		if _, err = conn.Write([]byte(authCmd)); err != nil {
			return err
		}
		line, rErr := reader.ReadString('\n')
		if rErr != nil {
			return rErr
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "+OK") {
			return fmt.Errorf("redis auth failed: %s", strings.TrimSpace(line))
		}
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&buf, "$%d\r\n%s\r\n", len(a), a)
	}

	if _, err = conn.Write(buf.Bytes()); err != nil {
		return err
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "+OK") {
		return fmt.Errorf("%w: %s", errConfigSetFail, trimmed)
	}

	return nil
}

func redisConfigSet(
	ctx context.Context,
	port int64,
	password, param, value string,
) error {
	return redisExec(ctx, localhost, port, password, []string{"CONFIG", "SET", param, value}, 2*time.Second)
}

func redisReplicaOf(
	ctx context.Context,
	host string,
	port int64,
	password string,
	masterHost string,
	masterPort int64,
	timeout time.Duration,
) error {
	if masterHost == "" || strings.EqualFold(masterHost, "NO") {
		return redisExec(ctx, host, port, password, []string{"REPLICAOF", "NO", "ONE"}, timeout)
	}
	return redisExec(
		ctx,
		host,
		port,
		password,
		[]string{"REPLICAOF", masterHost, strconv.FormatInt(masterPort, 10)},
		timeout,
	)
}
