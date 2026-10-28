package docker

import (
	"slices"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/kafka"
)

func TestRunArgs(t *testing.T) {
	for _, s := range []*kafka.Spec{{Plain: true}, {TLS: true}, {Plain: true, TLS: true}} {
		s.Name, s.Volume, s.ARN, s.Owner = "broker", "broker-data", "cluster-arn", "store-owner"
		args := runArgs(s, 19092, 19094)
		for _, want := range []string{
			Image, "512M", "dev-container", "1", "tinystack.kafka=cluster-arn",
			"tinystack.kafka.store=store-owner", "broker-data:/var/lib/redpanda/data",
		} {
			if !slices.Contains(args, want) {
				t.Fatalf("missing %s in %q", want, args)
			}
		}

		// The pinned image's entrypoint splits whitespace inside individual arguments.
		for _, arg := range args {
			if strings.ContainsAny(arg, " \t\r\n") {
				t.Fatalf("entrypoint would split argument %q", arg)
			}
		}

		joined := strings.Join(args, " ")
		if strings.Contains(joined, "plain://127.0.0.1:19092") != s.Plain ||
			strings.Contains(joined, "tls://127.0.0.1:19094") != s.TLS {
			t.Fatal(joined)
		}

		if strings.Contains(joined, "server-key.pem") != s.TLS {
			t.Fatal(joined)
		}
	}
}
