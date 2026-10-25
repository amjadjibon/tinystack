package docker

import (
	"slices"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/rds"
)

func TestMongoReadinessCommand(t *testing.T) {
	for _, version := range []string{"3.6", "4.0", "5.0"} {
		shell := "mongo"
		if version == "5.0" {
			shell = "mongosh"
		}

		args := mongoCommand(
			&rds.Spec{Image: "mongo:" + version, Name: "test", Username: "master", Password: "password"},
			"db.adminCommand({ping:1})",
		)
		if args[2] != shell || !slices.Contains(args, "--authenticationDatabase") ||
			!slices.Contains(args, "password") {
			t.Fatal(args)
		}
	}
}
