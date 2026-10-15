# Concepts

The vocabulary of UMP, and how the pieces fit together. Each term links to
the document that goes into depth.

## Servers and references

A **server** is anything ump can install: a dedicated server or a cloud
VM, seen through a provider, or a **manual server** added by hand (`ump
server add`) when no provider lists it. Commands take a **server
reference**:

| Form | Example | Meaning |
|---|---|---|
| `provider/id` | `hetzner-robot/123456` | unambiguous: the provider type and its native ID |
| `id` | `123456` | the provider's ID alone |
| `name` | `web-01` | the name as known at the provider |
| `ip` | `203.0.113.10` | the primary IPv4 or IPv6 address; an address inside the server's IPv6 prefix matches too |

Names need not be unique: `ump server list` lists every match, a command
that acts on one server asks which one is meant on a terminal and fails
with the candidates without one, so a script retries with `provider/id` or
`--account`. The provider APIs are the source of truth for what exists;
ump adds what they do not know (a price set by hand, your attributes,
the manual servers) from its configuration file, caches listings to
show what changed (`ump server list --diff`), and so serves as the
[inventory](inventory.md) of the fleet.

The server's name at the provider is the name ump uses everywhere,
including the hostname it sets at installation, so `ump server rename`
requires a valid hostname.

## Accounts and providers

A **provider** is an integration with a hosting API: Hetzner Robot,
Hetzner Cloud, OVHcloud, DigitalOcean, Linode, Vultr. An **account** is one set of credentials for one
provider, under a name of your choice (`ump config isp`); several
accounts of the same type are fine. Each provider type declares its
**capabilities** (inventory, describe, rename, rdns, power, rescue), and
a command that needs one the provider lacks says so instead of failing
half-way. See [provider accounts](providers.md).

## The rescue system

The **provider's rescue system** is the Linux a provider can boot a
server into over its API, with an SSH key. Every provider's differs, so
ump uses it for one thing only: to get root over SSH once. From there the
server downloads **UMP's rescue image**, a Debian built by this project
that is one kernel and one initramfs, and `kexec`s into it. It runs
entirely from RAM with no disk mounted, and it is the same at every
provider. When the running system already lets UMP in as root (UMP's key
in `authorized_keys`), the `kexec` happens from there and no reboot
through the provider is needed: the **direct way**, as opposed to the
**provider's way**. `ump server rescue` lands you in a root shell there,
`ump server reinstall` installs from there. See [the rescue
image](../images/rescue/README.md).

## The storage

The **storage** is one base URL that serves everything a server fetches:
the rescue image and its entry script, the installer, the published
profiles, the OS images, plus `get.sh` and the ump binaries. The default
is `https://ump.zhurko.de/dl`; any web server or object store with plain
`GET` does. See [configuration](configuration.md#storage).

## Profiles, layouts, firewalls, users, userdata and keys

A **profile** says what a server becomes: the OS image, the disk layout,
the users, the userdata that runs in the new system, the firewall, the kernel command line, the
step after the first boot. A **disk layout** is a list of parts
(partitions, md arrays, logical volumes) applied to whatever disks a
server has; a **firewall** is an input policy and the allowed ports; a
**user** is an account with sudo, groups, a shell and key references; a
**userdata** entry is a script that runs in the new system before its
first boot (a declarative configuration later, for the systems that
take one).
Layouts, firewalls and users are named entries in `layouts.yaml`,
`firewalls.yaml` and `users.yaml`, referred to from profiles by name, or
written inline in one. All of it is YAML in `~/.config/ump/profiles/`;
the published entries are templates that `ump config os update`
replaces, what you create under names of your own is kept, and every
file is checked when edited. **Keys** are the public keys a user's
references resolve to: UMP's own, your `~/.ssh` files, the agent, or
keys kept under names in `~/.config/ump/keys.yaml`. See
[profiles](profiles.md), [disk layouts](disk-layouts.md),
[firewalls](firewalls.md), [users](users.md), [keys](keys.md).

## The image and the installer

The **OS image** is a complete root file system as a `tar.zst` with a
manifest that pins its checksum, built by this project from Ubuntu 26.04
for amd64 and arm64 ([the Ubuntu base image](../images/ubuntu/README.md)).
The **installer** is a script fetched from the storage and run in the
rescue system with the resolved profile as a JSON **specification**: it
plans the layout against the disks, wipes them, builds partitions, md and
LVM, unpacks the image and configures what depends on the machine
([the installer](../install/README.md)). The specification stays on the
installed system as `/etc/ump/spec.json`, and `/etc/ump/install` records
what was installed.

## UMP's key and the administrator

ump has an SSH key of its own. It goes into every rescue system ump boots
and, through the `ump` key reference, to the **administrator** of an
installed system: the profile's first sudo user, `ump` by default. The
administrator is who ump logs in as afterwards (`ump ssh`, the check
after an installation, the direct way back into the rescue system), and
who you log in as; root has no SSH login unless the profile lists it.
No passwords are set anywhere. See [keys](keys.md) and
[users](users.md#the-administrator-and-root).

## How a reinstall runs

1. **Resolve.** Find the server at its provider; load the profile
   (`--profile`, the one set for the server, or the default profile), its
   layout, firewall and users; resolve the key references on your machine. Ask for
   confirmation.
2. **Enter.** Bring the server into UMP's rescue system: as it is when it
   already runs it, by the direct way when the running system lets UMP
   in as root, else through the provider's rescue system. UMP's key and
   yours go in.
3. **Install.** Fetch the installer from the storage and run it there with
   the specification: plan, wipe, partition, md, LVM, file systems, unpack
   the image, hostname, netplan with uniform interface names, kernel
   command line, users and keys, userdata, firewall, boot loader. `--dry-run`
   stops after the plan.
4. **Reboot and verify.** Disable the provider's rescue flag where it
   persists, reboot, wait until SSH answers as the administrator with
   UMP's key, and report the system that came up.

Steps 3 and 4 are identical at every provider; that is where the
homogeneity comes from.

## Safety

Installing erases every disk of a server. ump makes that hard to do by
accident: `--dry-run` shows the plan without touching a disk; a
reinstall, a reboot, a stop and a rescue ask for confirmation on a
terminal and require `--yes` without one; ump never prompts when stdin
is not a terminal but fails with exit code 3 and says what was needed;
the installer refuses to run outside the UMP rescue system; a layout
whose targets match no disk, or an array with too few members, stops the
installation before any disk is touched; `ump server rescue` never takes
a way in that would erase a machine (only `reinstall` may, on an OVH VPS,
since it erases the machine anyway). Credentials are references, never
plaintext in the configuration, and `ump config isp view` redacts what
is.

## Where state lives

ump keeps only local, disposable state: `~/.config/ump/` (the
configuration, profiles, secrets, UMP's key) and `~/.cache/ump/` (listing
snapshots). Deleting the cache is always safe. See
[configuration](configuration.md#where-things-live).

## Design in brief

- **One static Go binary**, built for Linux, macOS and Windows; the
  provider clients are hand-written over REST, so the binary does not
  carry an SDK's dependency tree. On the server, the rescue image and a
  shell installer do the work; ump drives them over SSH with the local
  `ssh` client and its own key.
- **The same path everywhere.** Providers differ in how a server gets
  root over SSH once. Everything after that, the kexec into UMP's rescue
  image and the installer, is one code path for bare metal and clouds.
- **Data, not tool, lives on the storage.** The rescue image, the OS
  images, the installer and the published profiles are fetched by URL
  with pinned checksums; the binary carries the CLI alone.
- **Providers are small.** List, describe, rename, reverse DNS, power,
  rescue, behind interfaces checked with type assertions; a quirk that
  leaks out of a provider package means the interface is missing
  something. See [adding a provider type](providers.md#adding-a-provider-type).
- **Tests without hardware.** Provider packages are tested against
  recorded API responses, the CLI against a fake provider, the rescue
  image in QEMU, and `make check` runs gofmt, vet, the tests and the
  script syntax checks.
