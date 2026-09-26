package main

import (
	"os"

	"github.com/ravenmk2/muxcat/internal/cli"
	// Register built-in connectors.
	_ "github.com/ravenmk2/muxcat/internal/connector/etcd"
	_ "github.com/ravenmk2/muxcat/internal/connector/mysql"
	_ "github.com/ravenmk2/muxcat/internal/connector/openobserve"
	_ "github.com/ravenmk2/muxcat/internal/connector/redis"
	_ "github.com/ravenmk2/muxcat/internal/connector/sqlite"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
