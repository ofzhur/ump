# Who owns which fact

Each fact has one owner; everything else reads it from there. Edit a fact
only at its owner, and only through the owner's commands or files.

| Fact | Owner | Where it lives | Change it with |
|---|---|---|---|
| Which servers exist, their addresses, location, product, power state, contract | the provider | the provider's API (and console) | the provider; ordering and cancelling are a human's |
| The name at the provider (the machine name, the next installation's hostname) | the provider, set through ump | the provider | `ump server rename` |
| Reverse DNS of public addresses | the provider, set through ump | the provider | `ump server rdns set\|delete` |
| Provider accounts and their credentials | ump's state | `config.yaml` and `secrets/` of the home | `ump config isp ...` (human only) |
| What a server becomes: image, disk layout, firewall, users and keys, userdata, kernel switches, private networks | ump's profiles | `profiles/`, `layouts.yaml`, `firewalls.yaml`, `users.yaml`, `userdata.yaml`, `networks.yaml`, `keys.yaml` of the home | `ump config os\|disk-layout\|firewall\|network\|userdata ...`; users and keys: the human |
| UMP's own SSH key | ump's state | `staging_ed25519` of the home | `ump config key roll` (human only) |
| What ump knows beyond the provider: the profile a server is reinstalled with, prices set by hand, attributes, groups, private addresses, what was installed where and when, servers added by hand | ump's state | `state.yaml` of the home | `ump server set\|add\|remove`; installations write it |
| The machines booted over the network (PXE), their sites and modes | ump's state | `pxe.yaml` of the home | `ump config pxe ...` |
| Everything above the base system: services, their configuration, users after day one | the infrastructure repository | `ansible.cfg`, `inventory/groups.yml`, `inventory/group_vars/`, `inventory/host_vars/`, `playbooks/`, `roles/`, `collections/` | a commit to the repository; run with `ump provision` |
| The servers ump installed, as Ansible hosts | ump, written into the repository | `inventory/hosts/HOSTNAME.yml` (or a storage that `inventory/hosts.yml` points to, or `inventory/ump.yml`: ump is the inventory) | ump writes them at a reinstall and at `ump provision`; never by hand |
| Per-host overrides | the infrastructure repository | `inventory/host_vars/HOSTNAME.yml` | a commit |

## The home

The **home** (the state) is everything ump keeps: `~/.config/ump`, or a
shared backend (a directory, SSH, S3, R2, HTTP, Consul, Vault) that
`ump config state view` names, mirrored under `~/.cache/ump/homes/`.
With a shared backend, everyone who uses it acts as one UMP, with the
same key. The home holds secrets in clear text (account secret files,
the backend's credentials, UMP's private key): never read or print its
files; use the commands.

The listing cache (`~/.cache/ump/listings/`) holds the history of what
the providers listed, the newest 50 snapshots per account; deleting it
is always safe.

## The seams

- **Provider → ump**: `ump server list` asks every account and is the
  one listing over all providers.
- **ump → Ansible**: after an installation, ump writes the server's host
  file (`ansible_host`, `ump_admin`, `ump_ref`, `ump_os`,
  `ump_private_networks`, groups: `common` and those of `--group`) and
  runs the playbooks. `ump server list -o ansible` gives the same
  inventory as JSON. A host file is overwritten by ump: overrides go to
  `inventory/host_vars/`.
- **The join key** is the server's name: the name at the provider, the
  hostname ump installs and the Ansible inventory hostname are the same
  string (unless `--hostname` gave another).

## The infrastructure repository

ump runs Ansible from the current directory and expects:

```
infra/
├── ansible.cfg                    from the collection: config/push/ansible.cfg
├── inventory/
│   ├── groups.yml                 the group tree; keeps the inventory valid before the first server
│   ├── group_vars/  host_vars/
│   └── hosts/                     written by ump, one file per server (or hosts.yml / ump.yml)
├── playbooks/                     00-base.yml, 10-web.yml, ...: every one runs, in file-name order
└── collections/ansible_collections/ofzhur/ansible   the collection, a git submodule
```

Without it, a reinstall needs `--no-provision`, and `ump provision`
refuses with exit code 2 naming what is missing. Host files ump wrote
are committed by whoever owns the repository.
