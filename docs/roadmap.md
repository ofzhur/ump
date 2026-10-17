# Roadmap

## Where it stands

As of October 2026, ump does what the [README](../README.md) and the
[quickstart](quickstart.md) describe:

- **Providers:** Hetzner Robot, Hetzner Cloud and OVHcloud (dedicated
  servers, VPS, Public Cloud instances), with listing, details, rename,
  reverse DNS, power, rescue and reinstall for all of them; Linode
  (instances) the same, its rescue mode set up for SSH through the Lish
  console; Vultr (instances, bare metal) with the same except a
  provider rescue, so it is installed over SSH, after Vultr's own
  reinstall with UMP's key when SSH does not let UMP in; DigitalOcean
  (droplets) with listing, details, rename, power and an install over
  SSH, which needs SSH access to the droplet beforehand; the account
  wizard and the non-interactive form, secret references, the listing
  cache with `--diff`, `--since` and `--cached`.
- **The rescue system:** UMP's own rescue image for amd64 and arm64,
  entered by `kexec` from a running system that lets UMP in as root or
  from the provider's rescue system; `ump server rescue` and `unrescue`.
- **Installation:** `ump server reinstall` from profiles, with named and
  inline disk layouts (partitions, md, LVM with striping, mirroring and
  parity), users and keys, userdata scripts, nftables firewalls that also hold
  the ports containers publish, uniform interface names, the kernel
  command line, UEFI and BIOS boot; `--dry-run`; the Ubuntu 26.04 base
  image; `ump ssh`.
- **Configuration objects:** `ump config os`, `disk-layout` and
  `firewall` with list, get, create, edit and delete each; the
  published defaults downloaded on first use.
- **Distribution:** `get.sh` and the binaries for Linux, macOS and
  Windows on the storage, `make release`.

## Next

- **The provision step.** `provision.type: ansible-pull` and
  `ansible-bootstrap` are recorded on the installed system and reported,
  but not run yet. Running them after the first boot, and a plain
  `script` step, come next.
- **More images.** Ubuntu, Debian and NixOS are there; Talos and
  cloud-init images may follow as userdata types.
- **Layouts.** LUKS with remote unlock, btrfs subvolumes, swap on a plain
  partition.
- **Network.** IPv6 addresses from the provider's network information
  where the rescue system cannot see them; bonds and VLANs.
- **A way into locked-out servers.** At DigitalOcean (and for servers
  added by hand) ump installs only what it can log into: DigitalOcean
  puts SSH keys on a droplet only at creation, and its rebuild and
  recovery ISO take those same keys. An automatic way in for droplets
  that let nobody in (for example through a custom image, or the
  recovery console) may come in a later release.
- **Several servers at once.** `ump server reinstall` over a list of
  servers with a parallel limit and a per-server progress view.
- **More providers.** Scaleway bare metal (rescue), clouds without a
  rescue system such as AWS and GCP (the direct way from a stock image),
  own hardware through Redfish virtual media or PXE, and a static list of
  SSH hosts as the universal fallback. The provider interface is in
  place; see [adding a provider type](providers.md#adding-a-provider-type).
- **Cloud instances.** Creating and destroying instances for providers
  that have them, for convenience; Terraform remains the usual way.
- **Configuration management.** Handing an installed machine over to the
  tool that keeps it configured (Ansible, Salt, Puppet, NixOS's own
  deployments), with the facts ump knows about it (provider, profile,
  addresses, interfaces, attributes) as its inventory; the provision step
  of the profile is where it starts. It becomes the third kind of
  [platform](platforms.md), next to providers and operating systems.
- **The skill.** Packaged operating knowledge for AI agents that run a
  fleet with ump, Terraform and Ansible, with an approval step that stays
  human. [The design outline](skill.md) is written; the tool-side
  pieces it needs (a plan with a hash, a separate approval command, an
  inventory export) are not.

## Open questions

- **A separate approval for destructive runs.** Today a reinstall asks on
  a terminal or takes `--yes`. An agent workflow wants a plan recorded
  with a hash, approved by a human in a separate step and consumed by the
  install; whether that is a TTY-only command, a signed comment or a CI
  environment gate is open.
- **Stock images as an alternative to UMP's own.** The base image gives
  identical content everywhere; a provider's stock cloud image would be
  less work to keep current. Possibly both, with the profile choosing.
- **ZFS.** Popular on bare metal; licensing and distribution support
  differ. Possibly as an optional feature of a Debian or Ubuntu image.
- **License.** Apache License 2.0, since v0.3.0.
