# ISPs

What each provider type does in detail: its credentials and IDs, prices,
renaming, power, reverse DNS, the way into a rescue system, and what
`ump server show` adds. How accounts are set up is in [provider
accounts](providers.md); the vocabulary is in [concepts](concepts.md).
This page is part of [platforms](platforms.md), next to [operating
systems](oses.md).

| Type | Provider | Rescue system for ump | Without SSH access, reinstall |
|---|---|---|---|
| [`hetzner-robot`](#hetzner-robot-hetzner-robot) | Hetzner dedicated servers | yes | goes through the provider's rescue system |
| [`hetzner-cloud`](#hetzner-cloud-hetzner-cloud) | Hetzner Cloud | yes | goes through the provider's rescue system |
| [`ovh`](#ovhcloud-ovh) | OVHcloud dedicated servers, VPS, Public Cloud | yes (a VPS: no key) | goes through the provider's rescue system; a VPS is rebuilt with UMP's key |
| [`digitalocean`](#digitalocean-digitalocean) | DigitalOcean droplets | no | stops: the droplet must let ump in beforehand |
| [`linode`](#linode-linode) | Linode (Akamai Cloud Computing) | yes: rescue mode (Finnix), set up through Lish | goes through the provider's rescue system |
| [`vultr`](#vultr-vultr) | Vultr instances and bare metal | no | Vultr reinstalls the machine with UMP's key |
| [`manual`](#manual-servers-manual) | servers added by hand | no | stops: the server must let ump in beforehand |

## Hetzner Robot (`hetzner-robot`)

- Dedicated servers. The server name is a free label at Hetzner, not
  required to be unique; `ump server rename` sets it and refuses a name
  another listed server has (`--force` renames anyway).
- Status: the power state (`running`, `off`) where Robot reports it,
  asked per server (`GET /reset/<number>`, a few at a time) because
  Robot's server list has only the contract's status; a server whose
  power state Robot cannot tell, or one not asked because Robot refused
  a request (it limits its API per hour), shows that status (`ready`,
  `in process`), and a cancelled server shows `cancelled`.
- Rescue: UMP's key (and `--ssh-key` files) must be registered at the
  account; the provider registers keys it does not know by fingerprint,
  then arms the rescue system and resets the server. Robot prints the
  rescue system's host key fingerprints, and ump shows them.
- Power: `reboot` asks the system to reboot (ACPI), `--hard` is a
  hardware reset, `--power-cycle` cuts the power; a server that is
  switched off is switched on instead, because a reset does nothing
  then. `stop` presses the power button (ACPI shutdown), `--hard` holds
  it; `start` presses it.
- Reverse DNS is set for any address of the server (primary IP,
  additional IPs, subnets, the IPv6 /64), and any name is accepted.
- Firewall: before a rescue or a reinstall, ump reads the server's Robot
  firewall and warns when it is active and lets no UDP answers in. The
  Robot firewall is stateless: without a rule for them, the answers of
  resolvers outside Hetzner (UMP's systems use public ones) are dropped,
  so DNS lookups time out until systemd-resolved falls back to TCP (a
  minute or more after every boot) and downloads fail meanwhile. An
  input rule `udp`, source port `53`, `accept` (or Hetzner's template
  rule for UDP to ports 32768-65535) lets them in.
- `ump server show` adds the contract (product, traffic, paid until,
  cancellation), addresses, features, power state and reset types, and
  the rescue state.

## Hetzner Cloud (`hetzner-cloud`)

- One account is one project; its API token is the credential. Server
  names must be unique within the project and valid hostnames; the API's
  uniqueness error is reported plainly.
- Rescue: the rescue system is enabled with the project's SSH keys (UMP's
  is registered when missing) and the server reset; Hetzner always issues
  a root password too. Actions are followed until they finish.
- Power: soft reboot, hard reset, power cycle, start and stop all exist.
- Reverse DNS works for the IPv4 and for single addresses inside the
  IPv6 /64; deleting an entry restores Hetzner's default name.
- `ump server show` adds the server type, image, location, networks
  (PTR records, floating IPs, private networks, cloud firewalls), traffic
  and protection.

## OVHcloud (`ovh`)

One account covers dedicated servers, VPS and Public Cloud instances,
because one set of API keys does. A dedicated server's ID is its service
name (`ns1234.ip-1-2-3.eu`) and its name the display name when one is
set; instances carry their project as labels; VPS are labelled
`family: vps`.

- OVHcloud's API front end now and then refuses a TLS handshake
  (`remote error: tls: record overflow`) and takes the next one; ump
  sends such a request again, up to twice, after 1 and 3 seconds. The
  request had not gone out, so this is safe for every call (every
  provider's client does it).
- Credentials: an application key and secret, plus a consumer key with
  access rules. The wizard requests a consumer key with exactly the rules
  the commands need and shows the validation URL; a consumer key made
  elsewhere needs the same rules, and a missing one shows up as
  `NOT_GRANTED_CALL` with the path. A key that may not see one family
  (say, no Public Cloud) is tolerated: that family is skipped. A consumer
  key made before VPS support lacks the `/vps` rules and shows no VPS;
  `ump config isp add` requests a new key with them.
- Rename: Public Cloud instances and VPS can be renamed; dedicated servers
  cannot through the API (the display name is read-only there), and ump
  says so.
- Power: a dedicated server knows one reboot (a hardware reset) and cannot
  be switched on or off through the API; instances and VPS reboot, start
  and stop. Every change to a VPS is a task at OVH, and OVH refuses the
  next action while one runs, so ump waits for the tasks.
- Rescue: a dedicated server takes one rescue SSH key; an instance is put
  into rescue mode by OVH itself (it reboots the instance and returns a
  password); a VPS rescue takes no key, so `ump server rescue` refuses the
  provider's way for a VPS and says so (the direct way, a kexec from the
  running system that lets UMP in as root, works). `ump server reinstall`
  still works on a VPS: OVH rebuilds it with its newest stock Debian
  image and UMP's key, and ump continues from there; the whole reinstall
  is unattended and takes a few minutes longer. Rescue stays the boot
  option on OVH dedicated servers until `ump server unrescue`; `reinstall`
  does that for you.
- Reverse DNS: OVH requires the name to resolve to the address before it
  accepts the entry, so create the forward record first.
- `ump server show` adds, for a dedicated server, the boot option, the
  addresses, the service dates and the hardware; for an instance, the
  flavor, image, addresses, SSH key and project.

## DigitalOcean (`digitalocean`)

One account is one DigitalOcean team; a personal access token is the
credential. A droplet's ID is its droplet ID, its name the droplet
name, its location the region (`fra1`, `nyc3`, ...), its product the
size (`s-1vcpu-1gb`), and its tags become labels. The IPv4 is the
public one (the private VPC address is shown by `ump server show`).

> **For now, ump installs a droplet only when it can already log in.**
> The droplet has to let ump in over SSH before `ump server reinstall`,
> as root or as a sudo user, with one of your keys (in `~/.ssh` or your
> ssh-agent) or UMP's own (`ump config key show`). Pick such a key when
> you create the droplet: DigitalOcean puts SSH keys on a droplet only
> at creation, and neither its API nor its rebuild nor its recovery ISO
> adds one later (both give the droplet back the keys it was created
> with). Without that access ump stops before it changes anything. A
> way into droplets that are locked out may come in a later release.

- Price: the size's monthly list price in USD, which the droplet record
  carries (`"source": "provider"`).
- Status: DigitalOcean's own: `active`, `off`, `new`, `archive`.
- Rename uses the rename action; ump waits until it is done.
- Power: reboot (ACPI), power cycle for a hard reset and a power cycle
  alike, shutdown (ACPI) and power off, power on; every action is
  followed to completion.
- Reverse DNS: DigitalOcean has no API for it. It sets a droplet's PTR
  from the droplet's name when that is a fully qualified domain name,
  so `ump server rename web-01 web-01.example.net` is how to set it;
  `ump server rdns` says the provider type has none.
- Rescue: DigitalOcean has no rescue system in its API (its recovery
  ISO is switched on in the Control Panel only, and takes the
  droplet's creation keys too), so `ump server rescue` and `ump server
  reinstall` go in over SSH into the running droplet alone, with the
  access described above. When it gets in with one of your keys, ump
  adds UMP's key to that account, so that UMP's key works by itself from
  then on. `ump server rescue` jumps into the UMP rescue system only
  with `--force`, since nothing but the console brings the droplet back
  when the jump fails; it never rebuilds or erases. ump says before
  erasing anything that no provider rescue stands behind the droplet; a
  system that does not boot is recovered with the recovery console or a
  rebuild in the Control Panel.
- `ump server show` adds the image, tags, features, VPC and volumes,
  the size's CPUs, memory, disk and transfer with the price, the
  region's name, and the addresses with netmask and gateway.

## Linode (`linode`)

One account is one Linode (Akamai Cloud Computing) account; a personal
access token is the credential, with the scopes Linodes and IPs
read/write and Images read only (a missing scope shows up as a 403
that says so). An instance's ID is its Linode ID, its name the label,
its location the region (`de-fra-2`, `us-east`, ...), its product the
type (`g6-nanode-1`), and its tags become labels. The IPv4 shown is the
first public one (Linode lists private addresses in the same field);
the IPv6 is the SLAAC address.

- Price: the type's monthly list price in USD, the region's own where
  Linode charges more there (`"source": "provider"`); a retired type has
  none, set it by hand then.
- Rename sets the label: 3 to 64 letters, digits, `-`, `_` and `.`,
  starting with a letter. The hostname inside the system stays.
- Power: one reboot (a soft and a hard reboot are the same call), boot,
  and shutdown, which asks the system to stop and powers it off when it
  does not in time; a power cycle is a shutdown and a boot.
- Reverse DNS works for the public IPv4 addresses, the SLAAC address and
  addresses in the instance's IPv6 ranges; deleting an entry restores
  Linode's default (`*.ip.linodeusercontent.com`). Linode checks that
  the name resolves to the address, so create the forward record first.
- Rescue: Linode's rescue mode, which boots Finnix from RAM with the
  instance's disks attached, for one boot. Finnix takes no SSH key, so
  ump sets it up through Lish, Linode's console gateway:
  1. UMP's key goes into your profile's Lish keys (Cloud Manager:
     profile → LISH Console Settings), next to the keys there, and stays
     for the next time;
  2. the instance is booted into rescue mode with the disks of its
     configuration;
  3. ump logs in to `lish-<region>.linode.com` as your Linode user with
     UMP's key, waits for Finnix's root prompt on the console, and types
     root's `authorized_keys` and the start of sshd.

  From there it is a rescue system like Hetzner's: `ump server rescue`
  loads the UMP rescue image from it, and `ump server reinstall` installs
  through it, without `--force` and without touching the disks first.
  About a minute and a quarter on a nanode, measured on 2026-10-17. The
  next reboot boots the instance's configuration again, so `unrescue`
  has nothing to disarm, and `rescue --no-reboot` is refused: rescue
  mode cannot be armed for a later boot. When the running system lets UMP in over SSH,
  that way is taken first, as everywhere. Lish must not be disabled in
  the profile, and the token must be allowed to change the profile;
  either refusal is reported. A system that does not boot is recovered
  the same way, or with Lish by hand.
- Booting what ump installed: Linode boots an instance with its own
  GRUB 2 by default, which reads `grub.cfg` from a disk that is one bare
  file system, and its helpers change files on that file system at boot.
  A system ump installs has partitions and its own GRUB, so after the
  installer ump switches the instance's configuration profiles to
  Direct Disk with the helpers that touch the disk off (distro,
  modules.dep, devtmpfs automount), and reboots through the API, since a
  reboot from inside keeps the old configuration. Without the switch the
  new system never starts and the console stays empty. A Linode stock
  image needs Linode's GRUB 2 mode again: set it back in Cloud Manager
  before rebuilding with one.
- `ump server show` adds the image, hypervisor, group, tags, watchdog
  and backups, the type's CPUs, memory, disk and transfer with the
  price, the region's name and country, and the addresses (public and
  private IPv4 with their reverse names, the SLAAC address, the IPv6
  ranges).

## Vultr (`vultr`)

One account covers the cloud instances and the bare metal servers of a
Vultr account; the personal API key is the credential. Its access
control is a list of addresses allowed to use it, empty at first, so
add the addresses ump runs from; otherwise Vultr answers "Unauthorized
IP address: ..." with the address it saw, and ump passes that on. An instance's or a
server's ID is Vultr's UUID, its name the label (the hostname when
there is no label), its location the region (`fra`, `ewr`, ...), its
product the plan, and its tags become labels.

- Price: the plan's monthly list price in USD, from the API
  (`"source": "provider"`); a plan Vultr no longer offers has none, set
  it by hand then.
- Status: an instance shows its power state (`running`, `stopped`) once
  active, else Vultr's status (`pending`, `suspended`, `resizing`); a
  bare metal server shows Vultr's status.
- Rename sets the label; the hostname inside the system stays.
- Power: one reboot (a soft and a hard reboot are the same call), start,
  and stop, which halts: the API has no ACPI shutdown. A power cycle is a
  halt and a start. The power state of a bare metal server is not in
  the API.
- Reverse DNS works for instances, the IPv4 and single addresses of the
  IPv6 network; deleting an IPv4 entry restores Vultr's default
  (`*.vultrusercontent.com`). Bare metal servers have no reverse DNS in
  the API; set it in the customer portal.
- Rescue: Vultr has no rescue system in its API. `ump server rescue` and
  `ump server reinstall` go in over SSH into the running system (as root
  or a sudo user, with UMP's key or yours) and load the rescue image
  with kexec. When the running system does not let UMP in, `ump server
  reinstall`, which erases the machine anyway, has Vultr reinstall the
  machine's own OS first: UMP's key is added to the account's SSH keys
  (as `ump`, ready for the next deploys), the machine's user data is
  replaced by a cloud-config that opens root to that key, and Vultr's
  reinstall runs it on the first boot; ump waits up to 15 minutes for
  an instance (45 for a bare metal server) and goes on from there.
  `ump server rescue` jumps into the UMP rescue system only with `--force`, since
  nothing but the provider's console brings the server back when it
  fails; it never rebuilds or erases. ump says before erasing anything
  that no provider rescue stands behind the machine; a system that does
  not boot is recovered with Vultr's web console or a reinstall in the
  customer portal.
- `ump server show` adds the server's status, OS, tags and features, the
  plan's CPUs, memory, disk and bandwidth with the price, the region's
  city and country, and the network (netmask, gateway, the IPv6 network,
  the private address and the firewall group of an instance, the MAC
  address of a bare metal server).

## Manual servers (`manual`)

Not a provider at all, but the way machines no provider lists get into
the inventory: `ump server add NAME --ipv4 ADDRESS [--ipv6 ...]
[--location ...] [--product ...] [--kind bare-metal|cloud] [--note ...]`
puts a server into the [state](state.md#the-state-document) (`manual:`), and the account
`manual` of this type lists them next to the others, with the status
`manual`. `ump server remove NAME` forgets one. Prices and attributes
work as for any server; power control, renaming and reverse DNS do not
exist, and `ump server rescue` and `ump server reinstall` work only
through SSH: when the running system lets UMP in (as root or a sudo
user, with UMP's key or yours), the rescue image is loaded and entered
with kexec. Nothing stands behind that: a failed installation or a
system that does not boot is yours to recover, with a console or the
provider's own means, and ump says so before erasing anything; `ump
server rescue` makes the jump only with `--force`. See
[inventory](inventory.md#servers-added-by-hand).
