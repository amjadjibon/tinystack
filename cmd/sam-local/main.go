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
	env := localcli.BaseEnv(endpoint, map[string]string{
		"AWS_PAGER":         "",
		"SAM_CLI_TELEMETRY": "0",
	})
	return localcli.Execute("sam", os.Args[1:], env)
}
