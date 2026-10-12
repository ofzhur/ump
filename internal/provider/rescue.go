package provider

import "context"

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
