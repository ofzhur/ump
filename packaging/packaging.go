// Package packaging holds the systemd units of the PXE server as the deb
// and rpm packages install them (nfpm.yaml takes the files beside this
// one), for ump pxe unit to print: one source for both.
package packaging

import (
	_ "embed"
	"strings"
)

// Service is ump-pxe@.service, the PXE server of the site its instance
// names; Check is ump-pxe-check@.service, its check with the same
// settings.
var (
	//go:embed ump-pxe@.service
	Service string
	//go:embed ump-pxe-check@.service
	Check string
)

// Binary is where the packages install ump, and what the units run.
const Binary = "/usr/bin/ump"

// WithBinary is a unit that runs ump from another path (a hand install's).
func WithBinary(unit, path string) string {
	return strings.Replace(unit, "\nExecStart="+Binary+" ", "\nExecStart="+path+" ", 1)
}
