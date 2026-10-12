package provider

import "context"

// CapPower is implemented by providers that can reboot a machine and
// switch its power (see Power).
const CapPower Capability = "power"

// RebootMode says how a machine is rebooted.
type RebootMode string

const (
	// RebootSoft asks the operating system to reboot (ACPI).
	RebootSoft RebootMode = "soft"
	// RebootHard resets the machine without asking the operating system.
	RebootHard RebootMode = "hard"
	// RebootPowerCycle cuts the power and switches it on again.
	RebootPowerCycle RebootMode = "power-cycle"
)

// PowerState is what the provider reports about the machine's power.
type PowerState string

const (
	PowerOn      PowerState = "on"
	PowerOff     PowerState = "off"
	PowerUnknown PowerState = "unknown"
)

// Power is implemented by providers that can reboot a machine and switch
// its power. Calls return when the provider has accepted the action; a
// machine takes its time to come back.
type Power interface {
	Provider
	PowerState(ctx context.Context, m Machine) (PowerState, error)
	Reboot(ctx context.Context, m Machine, mode RebootMode) error
	PowerOn(ctx context.Context, m Machine) error
	// PowerOff shuts the machine down through the operating system, or
	// cuts the power when hard is set.
	PowerOff(ctx context.Context, m Machine, hard bool) error
}
