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
  command line, UEFI and BIOS boot; `--dry-run`; Ubuntu 26.04, 24.04
  and 22.04 and Debian 13 and 12 from UMP's base images, NixOS 25.11,
  Arch Linux and CachyOS (experimental); `ump ssh`.
- **Provisioning:** Ansible from the infrastructure repository: `ump
  server reinstall` (`--group`) puts the server into the inventory, as
  a host file in the repository or in a central storage, or with ump as
  the inventory (`ump server list -o ansible`), and `ump provision`
  (also after every reinstall) runs the repository's playbooks over the
  fleet, each play on the hosts its `hosts:` names, one run at a time,
  every host logged in to as its administrator; the collection
  [ofzhur.ansible](https://github.com/ofzhur/ansible) brings the users
  and keys, and its fact cache `ofzhur.ansible.facts` keeps the facts
  in a central storage ([provisioning](provisioning.md)).
- **Configuration objects:** `ump config os`, `disk-layout`,
  `firewall`, `network`, `user` and `userdata` with list, get, create,
  edit and delete each, and `key`; the published defaults downloaded on
  first use.
- **Private networks:** every server ump installs gets an address in a
  private network (`default`, `10.0.0.0/24`), the lowest free one or
  one given by hand, kept across reinstalls and never another server's,
  in the state and the inventory (`ump_private_address`)
  ([private networks](networks.md)).
- **Distribution:** `get.sh` and the binaries for Linux, macOS and
  Windows on the storage, `make release`.

## Next

- **Mesh roles.** Roles in the collection that connect the servers over
  their private addresses (WireGuard), from the inventory's
  `ump_private_*` variables, with the interfaces configured on the
  servers ([mesh roles](networks.md#mesh-roles)).
- **Provider uplinks.** The providers' private networks (OVHcloud
  vRack, Hetzner vSwitch and Cloud Networks, Vultr, Linode and
  DigitalOcean VPCs) as uplinks of the private network, so that servers
  of one provider and location talk over it and the mesh tunnels only
  between providers or locations ([provider
  uplinks](networks.md#provider-uplinks)).
- **Servers that pull.** The collection's role that sets up
  `ansible-pull` on a timer, with a deploy key, the facts in a central
  storage (`ofzhur.ansible.facts`) and the server's own secrets, so
  that a server provisioned by ump stays configured
  ([provisioning](provisioning.md#servers-that-pull)).
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
  addresses, interfaces, groups, attributes) as its inventory; Ansible
  is there ([provisioning](provisioning.md)). It becomes the third kind of
  [platform](platforms.md), next to providers and operating systems.
  [Next steps](next-steps.md) describes the repository it is meant for.
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
