package main

import (
	"github.com/autonomousops-co/accretion/internal/cmd"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// See scripts/version.sh for how the value is derived. The default is what a
// plain `go build` with no ldflags produces.
var version = "dev"

func main() {
	cmd.Execute(version)
}
