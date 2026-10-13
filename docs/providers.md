# Providers (draft)

Providers connect UMP to the APIs that know about machines. They are kept
deliberately small: list machines, act on power and boot state, and
report provider-side network information. Everything else happens in the
staging environment and is the same for every provider.

See [architecture](architecture.md#providers) for the interfaces.

## Capability matrix

The table shows the **intended** use of each provider's API. Details are
to be verified against the current APIs when each integration is
implemented. Implemented today, for `hetzner-robot`, `hetzner-cloud`
and `ovh`: `inventory` (`ump server list`), `describe`, `rename`, `rdns`
(`ump get|set|delete rdns`), `power` (`ump reboot|start|stop server`)
and `rescue` (`ump rescue|unrescue server`). Not every provider supports every feature: each provider
declares its capabilities (`ump config isp types` lists them), and a command
that needs one the provider lacks says so instead of failing half-way.

| Provider | Kind | inventory | rename | power | rescue | create | rdns | network-info | virtual-media | Entry method |
|---|---|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|---|
| `hetzner-robot` | bare metal | ✓ | ✓ | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
| `hetzner-cloud` | cloud | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – | rescue |
| `ovh` | bare metal + VPS + cloud | ✓ | VPS, instances | ✓ | ✓ | – | ✓ | ✓ | – | rescue |
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
- Reverse DNS is account-wide (`GET /rdns`); UMP matches entries to a
  server by its addresses (primary IP, additional IPs, subnets, the
  IPv6 /64). Entries are created with `PUT /rdns/{ip}` and updated with
  `POST /rdns/{ip}`; any name is accepted.
- Rescue requires the SSH key to be registered at the account first; the
  provider registers keys whose fingerprint is missing (`POST /key`) and
  arms the rescue system with `POST /boot/{number}/rescue`; Robot issues
  a password only when no key is given.
- When the server is switched off, a hardware reset does nothing, so
  `ump server reboot` presses the power button instead. `stop` is a short press
  (ACPI shutdown) or, with `--hard`, a long press; `start` a short press.
  The `man` reset type (a technician) is never used.
- Provider network info: main IPv4, additional single IPs, subnets and the
  IPv6 /64.
- Boot order on Hetzner dedicated servers usually has PXE first; this
  is how rescue works. The bootloader step must not change that.

### Hetzner Cloud

- Server names must be unique within the project and valid hostnames;
  `ump server rename` uses `PUT /servers/{id}` and reports the API's
  `uniqueness_error` plainly.
- Reverse DNS is set with the `change_dns_ptr` action for the IPv4 and
  for single addresses inside the IPv6 /64; removing an entry restores
  Hetzner's default (`static.….clients.your-server.de`).
- Rescue is enabled with the `enable_rescue` action and project SSH keys
  (registered by fingerprint when missing), followed by a reset; Hetzner
  always issues a root password. Actions are followed until they finish.
- The API returns the place as a top-level `location` (`fsn1`) and
  `datacenter: null`; UMP reads both for older responses.
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
  `GET /cloud/project*`, `PUT /cloud/project/*/instance/*`, `GET /ip/*`,
  `POST /ip/*/reverse`, `DELETE /ip/*/reverse/*`, `PUT /dedicated/server/*`,
  `POST /dedicated/server/*/reboot` and the instance `reboot`,
  `rescueMode`, `start` and `stop` actions, and shows the validation URL; a key that may not see one of the two families is
  tolerated, that family is skipped. A key made elsewhere needs the same
  rules; a missing one shows up as `NOT_GRANTED_CALL` with the path.
- `ump server rename` renames Public Cloud instances (`instanceName`).
  Dedicated servers cannot be renamed through the API: the display name
  is read-only in every public API model, the v2 IAM resource included,
  so UMP says so instead of pretending; set it in the control panel.
- A dedicated server has one reboot (`POST /reboot`, a hardware reset
  as a task) and no power on/off through the API. Rescue means setting
  the `rescue` boot option (`PUT` with `bootId`) and one `rescueSshKey`;
  without a key OVH mails the password to the rescue mail address.
  Instances reboot soft or hard, start and stop, and `rescueMode` puts
  them into rescue (OVH reboots them itself and returns the password).
- Reverse DNS lives on IP blocks (`/ip/{block}/reverse`): for a
  dedicated server UMP uses the blocks routed to it (`/ips`), for an
  instance the `/32` of each IPv4 and the `/64` of each IPv6. OVH
  requires the name to resolve to the address before it accepts the
  entry. The instance path is untested against the live API.
- VPS (`/vps`) are listed next to dedicated servers and Public Cloud
  instances, labelled `family: vps`: reboot, start, stop, rename (the
  display name), reverse DNS on the `/32` and `/64` of their addresses,
  and rescue through the netboot mode (`rescue` until `unrescue`, the
  machine boots it at the next reboot). The VPS API takes no rescue SSH
  key and OVH mails the password, so UMP cannot log into a VPS rescue
  system by itself; `ump server rescue --no-image` with the mailed password is
  the way in. A consumer key made before VPS support lacks the `/vps`
  rules and shows no VPS; `ump config isp add` requests a new key with them.
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
   secret, required, choices, defaults — the wizard and `ump config isp add`
   are driven by them) and a `New` function that builds an instance from
   resolved settings. A `Complete` hook can fill settings interactively
   (OVH uses it to request a consumer key).
3. Implement `Provider` plus the optional capability interfaces
   (`Inventory` first; `Describer` returns the provider's extra facts as
   named sections for `ump server show`).
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
