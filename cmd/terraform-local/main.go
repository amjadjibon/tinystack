package main

import (
	"fmt"
	"os"

	"github.com/amjadjibon/tinystack/internal/localcli"
)

func main() {
	os.Exit(run())
}

func run() int {
	endpoint := localcli.Endpoint()
	workDir := localcli.FindTerraformWorkDir(os.Args[1:])

	cleanup, err := localcli.SetupTerraformOverride(workDir, endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tinystack: failed to configure terraform provider override: %v\n", err)
		return 1
	}
	defer cleanup()

	env := localcli.BaseEnv(endpoint, nil)
	return localcli.Execute("terraform", os.Args[1:], env)
}
