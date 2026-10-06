package elasticache

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProbeRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr != nil {
				return
			}
			if strings.Contains(line, "PING") {
				_, _ = conn.Write([]byte("+PONG\r\n"))
				return
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx := context.Background()
	if err := probeRedis(ctx, "127.0.0.1", int64(addr.Port), "", 2*time.Second); err != nil {
		t.Fatalf("probeRedis failed: %v", err)
	}
}

func TestProbeMemcached(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr != nil {
				return
			}
			if strings.HasPrefix(line, "version") {
				_, _ = conn.Write([]byte("VERSION 1.6.22\r\n"))
				return
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx := context.Background()
	if err := probeMemcached(ctx, "127.0.0.1", int64(addr.Port), 2*time.Second); err != nil {
		t.Fatalf("probeMemcached failed: %v", err)
	}
}

func TestRedisConfigSet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr != nil {
				return
			}
			if strings.Contains(line, "maxmemory-policy") {
				_, _ = conn.Write([]byte("+OK\r\n"))
				return
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx := context.Background()
	if err := redisConfigSet(ctx, int64(addr.Port), "", "maxmemory-policy", "allkeys-lru"); err != nil {
		t.Fatalf("redisConfigSet failed: %v", err)
	}
}

func TestRedisReplicaOf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, rErr := reader.ReadString('\n')
			if rErr != nil {
				return
			}
			if strings.Contains(line, "REPLICAOF") {
				_, _ = conn.Write([]byte("+OK\r\n"))
				return
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx := context.Background()
	if err := redisReplicaOf(
		ctx,
		"127.0.0.1",
		int64(addr.Port),
		"",
		"10.0.0.1",
		6379,
		2*time.Second,
	); err != nil {
		t.Fatalf("redisReplicaOf failed: %v", err)
	}
}
