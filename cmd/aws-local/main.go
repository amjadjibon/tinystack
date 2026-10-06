package main

import (
	"os"

	"github.com/amjadjibon/tinystack/internal/localcli"
)

func main() {
	os.Exit(run())
}

func run() int {
	endpoint := localcli.Endpoint()
	args := localcli.EnsureEndpointArg(os.Args[1:], endpoint)
	env := localcli.BaseEnv(endpoint, map[string]string{
		"AWS_PAGER": "",
	})
	return localcli.Execute("aws", args, env)
}
