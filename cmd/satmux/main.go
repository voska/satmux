// Command satmux makes high-latency links usable. See README.md.
package main

import (
	"os"

	"github.com/voska/satmux/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(version, os.Args[1:]))
}
