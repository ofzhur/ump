# Providers (draft)

Providers connect UMP to the APIs that know about machines. They are kept
deliberately small: list machines, act on power and boot state, and
report provider-side network information. Everything else happens in the
staging environment and is the same for every provider.

See [architecture](architecture.md#providers) for the interfaces.

## Capability matrix

The table shows the **intended** use of each provider's API. Details are
to be verified against the current APIs when each integration is
implemented.

| Provider | Kind | inventory | power | rescue | create | rdns | network-info | virtual-media | Entry method |
|---|---|:-:|:-:|:-:|:-:|:-:|:-:|:-:|---|
| `hetzner-robot` | bare metal | ✓ | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
| `hetzner-cloud` | cloud | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – | rescue |
| `ovh-dedicated` | bare metal | ✓ | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
| `scaleway-baremetal` | bare metal | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – | rescue |
| `aws-ec2` | cloud | ✓ | ✓ | – | ✓ | – | ✓ | – | kexec |
| `gcp-compute` | cloud | ✓ | ✓ | – | ✓ | – | ✓ | – | kexec |
| `redfish` | own hardware | ✓ (static) | ✓ | – | – | – | – | ✓ | virtual-media |
| `ipmi` | own hardware | ✓ (static) | ✓ | – | – | – | – | – | pxe |
| `libvirt` / `proxmox` | lab / private cloud | ✓ | ✓ | – | ✓ | – | – | ✓ | virtual-media |
| `ssh` | anything with SSH | ✓ (static) | – | – | – | – | – | – | kexec, manual |

`ssh` is the universal fallback: a static list of hosts, no API. With
`kexec` it can reinstall any running Linux machine that is reachable over
SSH as root. It is also the provider used by the integration test suite.

## Priorities

1. **`hetzner-robot`** — port of the earlier tool's integration (inventory, rescue,
   reset, rename, rDNS, SSH key registration).
2. **`ssh` + `kexec`/`manual` entry** — needed for testing in QEMU and
   immediately useful on its own.
3. **`hetzner-cloud`** — the first cloud, proves that bare metal and cloud
   go through the same pipeline.
4. **`redfish`** — own hardware.
5. Others as needed.

## Notes per provider

### Hetzner Robot

- Rescue requires the SSH key to be registered at the account first; the
  provider registers UMP's staging key if its fingerprint is missing.
- When the server is switched off, a hardware reset does nothing — a power
  reset is needed (known from the earlier tool).
- Provider network info: main IPv4, additional single IPs, subnets and the
  IPv6 /64.
- Boot order on Hetzner dedicated servers usually has PXE first; this
  is how rescue works. The bootloader step must not change that.

### Hetzner Cloud

- Rescue can be enabled via the API with SSH keys, followed by a reset.
- IPv4 is configured as `/32` with an on-link default gateway; the network
  model must express on-link routes.
- The stock images ship cloud-init. UMP installs its own image instead, so
  provider metadata cannot later override what UMP configured.

### Clouds without rescue (AWS, GCP, ...)

- Create the instance from any small stock image (or let Terraform do it),
  then use `kexec`: UMP connects to the stock OS, kexecs into its staging
  environment in RAM and installs onto the root volume.
- Network on these platforms is DHCP-based; the profile's network mode is
  `dhcp` there. A provider can set this default.
- Some platforms need specific drivers in the image (ENA/NVMe on AWS,
  gVNIC on GCP); the OS family driver ensures the kernel and initramfs
  include them.

### Redfish

- Insert the UMP staging ISO as virtual media, set one-time boot to
  virtual CD, power-cycle. After installation: eject, reset boot override.
- The staging ISO must get UMP's SSH key and network configuration. The
  ISO's kernel command line or a small config image carries both, because
  there is usually no DHCP on management-only networks.

## Writing a provider

1. Create a package under `internal/provider/<name>`.
2. Implement `Provider` plus the optional capability interfaces.
3. Map provider objects to UMP's `Machine` (ID, name, addresses, location,
   product, status, rescue state).
4. Register the type and its configuration schema.
5. Add tests with recorded API responses.

Provider-specific quirks stay inside the provider package. If a quirk
leaks into the pipeline, the interface is missing something.
