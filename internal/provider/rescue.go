package provider

import (
	"context"
	"time"
)

// CapRescue is implemented by providers whose machines can boot a rescue
// system that UMP reaches over SSH (see Rescue).
const CapRescue Capability = "rescue"

// PublicKey is an SSH public key to put into the rescue system.
type PublicKey struct {
	// Name is used when the key has to be registered at the provider.
	Name string
	// Data is the authorized_keys line: type, base64 blob, comment.
	Data string
	// Fingerprint is the MD5 fingerprint in colon-hex form, which is how
	// the providers identify registered keys.
	Fingerprint string
}

// RescueOptions configure the rescue system.
type RescueOptions struct {
	Keys []PublicKey
	// Identity is the path of UMP's private key, for a provider that sets
	// the rescue system up through a console gateway reached over SSH
	// (Linode's Lish).
	Identity string
	// NoReboot asks to only arm the rescue system for the next boot; a
	// provider whose rescue system starts with a reboot refuses it.
	NoReboot bool
}

// RescueInfo describes an enabled rescue system.
type RescueInfo struct {
	// Address is where SSH answers once the machine booted the rescue
	// system; the machine's primary IPv4 unless the provider says otherwise.
	Address string `json:"address,omitempty"`
	// User is the login name in the rescue system.
	User string `json:"user,omitempty"`
	// Password is the password the provider issued, if any.
	Password string `json:"password,omitempty"`
	// Note is a provider hint for the operator, e.g. where a password went.
	Note string `json:"note,omitempty"`
	// Reboots is set when the provider reboots the machine into the
	// rescue system itself, so the caller must not reboot it again.
	Reboots bool `json:"reboots,omitempty"`
	// Ready is set when EnableRescue returns only once the rescue system
	// answers on SSH, so the caller must not wait for the machine to go
	// down first.
	Ready bool `json:"-"`
	// HostKeys are the fingerprints of the rescue system's SSH host keys
	// when the provider reports them, for the operator to compare.
	HostKeys []string `json:"host_keys,omitempty"`
}

// Rescue is implemented by providers that can boot a machine into a
// rescue system. EnableRescue only arms it for the next boot; the caller
// reboots the machine.
type Rescue interface {
	Provider
	RescueEnabled(ctx context.Context, m Machine) (bool, error)
	EnableRescue(ctx context.Context, m Machine, opts RescueOptions) (*RescueInfo, error)
	DisableRescue(ctx context.Context, m Machine) error
}

// StockInstall is the result of StockInstaller.InstallStock.
type StockInstall struct {
	// Image is the provider's image that was installed.
	Image string
	// Address is where SSH answers once the machine runs it.
	Address string
	// Wait is how long the machine may take to let UMP in after the
	// call returned; zero means the caller's default.
	Wait time.Duration
	// Keys says which keys root takes on the new system when they are
	// not UMP's (the provider re-adds the keys the machine was created
	// with); empty means UMP's.
	Keys string
}

// StockInstaller is implemented by providers that can put one of their
// own stock Linux images on a machine with UMP's key for root (or, where
// the provider takes no key, the keys the machine was created with),
// which erases the machine. It is the way in for machines whose rescue system
// takes no key, or whose provider has none, when the running system
// does not let UMP in: ump server reinstall, which erases the machine
// anyway, may use it; nothing else may.
type StockInstaller interface {
	InstallStock(ctx context.Context, m Machine, keys []PublicKey) (*StockInstall, error)
}

// DiskBooter is implemented by providers that boot a machine with their
// own kernel or boot loader unless told otherwise (Linode's GRUB 2 mode
// reads the configuration from a bare file system). A system ump
// installed brings its own boot loader on partitioned disks, so after an
// installation the machine is switched to boot from its disks. The
// switch takes effect with a reboot through the provider's API, not
// with one from inside the machine.
type DiskBooter interface {
	BootFromDisk(ctx context.Context, m Machine) error
}

// KeylessRescue is implemented by providers whose rescue system, for some
// machines, cannot be given an SSH key (the password goes by mail), so
// that the caller can say so before enabling it.
type KeylessRescue interface {
	// RescueTakesNoKey reports whether the machine's rescue system can
	// only be entered with a password UMP does not get.
	RescueTakesNoKey(m Machine) bool
}

// Preflight is implemented by providers whose settings can stand in the
// way of a rescue system or an installation, such as a firewall at the
// provider. Preflight returns one warning per finding, none when nothing
// stands in the way.
type Preflight interface {
	Preflight(ctx context.Context, m Machine) ([]string, error)
}
