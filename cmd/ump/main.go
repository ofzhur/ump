// Command ump is the Unus Mundus Provisioner: one operating system, in the
// same shape, on any machine. See https://github.com/ofzhur/ump.
package main

import (
	"os"

	"github.com/ofzhur/ump/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
