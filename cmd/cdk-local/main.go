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
	env := localcli.BaseEnv(endpoint, localcli.CDKEnv())
	return localcli.Execute("cdk", os.Args[1:], env)
}
