package elasticache

import (
	"bufio"
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

func probeNode(ctx context.Context, engine, host string, port int64, timeout time.Duration) error {
	switch engine {
	case EngineRedis, EngineValkey:
		return probeRedis(ctx, host, port, timeout)
	case EngineMemcached:
		return probeMemcached(ctx, host, port, timeout)
	default:
		return fmt.Errorf("unknown engine %q", engine)
	}
}

func probeRedis(ctx context.Context, host string, port int64, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
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

func redisConfigSet(ctx context.Context, host string, port int64, param, value string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	cmd := fmt.Sprintf(
		"*3\r\n$6\r\nCONFIG\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
		len(param), param, len(value), value,
	)
	if _, err = conn.Write([]byte(cmd)); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
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
