# Providers (draft)

Providers connect UMP to the APIs that know about machines. They are kept
deliberately small: list machines, act on power and boot state, and
report provider-side network information. Everything else happens in the
staging environment and is the same for every provider.

See [architecture](architecture.md#providers) for the interfaces.

## Capability matrix

The table shows the **intended** use of each provider's API. Details are
to be verified against the current APIs when each integration is
implemented. Implemented today: `inventory` (`ump get servers`),
`describe` (`ump describe server`) and `rename` (`ump rename server`)
for `hetzner-robot`, `hetzner-cloud` and `ovh`.

| Provider | Kind | inventory | rename | power | rescue | create | rdns | network-info | virtual-media | Entry method |
|---|---|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|---|
| `hetzner-robot` | bare metal | ✓ | ✓ | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
| `hetzner-cloud` | cloud | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – | rescue |
| `ovh` | bare metal + cloud | ✓ | instances | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
| `scaleway-baremetal` | bare metal | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – | rescue |
| `aws-ec2` | cloud | ✓ | ✓ | ✓ | – | ✓ | – | ✓ | – | kexec |
| `gcp-compute` | cloud | ✓ | – | ✓ | – | ✓ | – | ✓ | – | kexec |
| `redfish` | own hardware | ✓ (static) | – | ✓ | – | – | – | – | ✓ | virtual-media |
| `ipmi` | own hardware | ✓ (static) | – | ✓ | – | – | – | – | – | pxe |
| `libvirt` / `proxmox` | lab / private cloud | ✓ | ✓ | ✓ | – | ✓ | – | – | ✓ | virtual-media |
| `ssh` | anything with SSH | ✓ (static) | – | – | – | – | – | – | – | kexec, manual |

`ssh` is the universal fallback: a static list of hosts, no API. With
`kexec` it can reinstall any running Linux machine that is reachable over
SSH as root. It is also the provider used by the integration test suite.

## Priorities

1. **`hetzner-robot`** — inventory, rescue, reset, rename, rDNS, SSH key
   registration.
2. **`ssh` + `kexec`/`manual` entry** — needed for testing in QEMU and
   immediately useful on its own.
3. **`hetzner-cloud`** — the first cloud, proves that bare metal and cloud
   go through the same pipeline.
4. **`redfish`** — own hardware.
5. Others as needed.

## Notes per provider

### Hetzner Robot

- The server name is a free label, not required to be unique; `ump rename
  server` sets it with `POST /server/{number}` (form-encoded, like every
  Robot write).
- Rescue requires the SSH key to be registered at the account first; the
  provider registers UMP's staging key if its fingerprint is missing.
- When the server is switched off, a hardware reset does nothing — a power
  reset is needed.
- Provider network info: main IPv4, additional single IPs, subnets and the
  IPv6 /64.
- Boot order on Hetzner dedicated servers usually has PXE first; this
  is how rescue works. The bootloader step must not change that.

### Hetzner Cloud

- Server names must be unique within the project and valid hostnames;
  `ump rename server` uses `PUT /servers/{id}` and reports the API's
  `uniqueness_error` plainly.
- Rescue can be enabled via the API with SSH keys, followed by a reset.
- IPv4 is configured as `/32` with an on-link default gateway; the network
  model must express on-link routes.
- The stock images ship cloud-init. UMP installs its own image instead, so
  provider metadata cannot later override what UMP configured.

### OVHcloud

- One account covers both dedicated servers (`/dedicated/server`) and
  Public Cloud instances (`/cloud/project/*/instance`), because one set of
  API keys does. Instances carry the project as labels (`project`,
  `project_id`); a dedicated server's ID is its service name
  (`ns1234.ip-1-2-3.eu`), its name the IAM display name when one is set.
- Credentials are an application key and secret plus a consumer key. The
  wizard requests a consumer key (`POST /auth/credential`) with the access
  rules the implemented commands need, `GET /dedicated/server*`,
  `GET /cloud/project*` and `PUT /cloud/project/*/instance/*`, and shows
  the validation URL; a key that may not see one of the two families is
  tolerated, that family is skipped. A key made elsewhere needs the same
  rules; a missing one shows up as `NOT_GRANTED_CALL` with the path.
- `ump rename server` renames Public Cloud instances (`instanceName`).
  Dedicated servers cannot be renamed through the API: the display name
  is read-only in every public API model, the v2 IAM resource included,
  so UMP says so instead of pretending; set it in the control panel.
- The endpoint is `ovh-eu`, `ovh-ca` or `ovh-us` (or a URL).

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
2. Declare a `provider.Type`: the name used in `config.yaml`, a
   description for the wizard, the settings as `Fields` (label, help,
   secret, required, choices, defaults — the wizard and `ump config add`
   are driven by them) and a `New` function that builds an instance from
   resolved settings. A `Complete` hook can fill settings interactively
   (OVH uses it to request a consumer key).
3. Implement `Provider` plus the optional capability interfaces
   (`Inventory` first; `Describer` returns the provider's extra facts as
   named sections for `ump describe server`).
4. Map provider objects to UMP's `Machine` (ID, name, addresses, location,
   product, status, labels). Lower-case locations and statuses; keep
   product names as the provider spells them.
5. Register the type in `internal/provider/all`.
6. Add tests against an `httptest` server with recorded API responses.

The API clients are hand-written over REST rather than taken from SDKs:
UMP needs a handful of calls per provider, the clients stay a few hundred
lines, and the binary does not pick up an SDK's dependency tree (the
official Hetzner Cloud SDK, for instance, pulls in Prometheus and
protobuf). Provider-specific quirks stay inside the provider package. If a
quirk leaks into the pipeline, the interface is missing something.
