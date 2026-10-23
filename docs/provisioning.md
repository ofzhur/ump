# Provisioning

An installation by ump ends with the operating system ready to use: the
disks, the users and their keys, the network, the firewall, and the
[userdata](userdata.md) of the profile, which ran before the first boot.
Provisioning is the next step: it turns that system into what the server
is for (a web server, a database, a monitoring node). ump does it with
Ansible, by push, from your infrastructure repository: it puts the
server into the repository's inventory, in **groups**, and runs the
playbooks of those groups for it.

```console
$ cd ~/src/infra                                   # the infrastructure repository
$ ump server reinstall web-01 --group web           # install, then provision: common, web
$ ump server reinstall db-01 --group db,backup      # several groups, in order
$ ump server provision web-01 --group monitoring    # add a group to an installed server
$ ump server provision web-01                       # run its run list again
$ ump server reinstall lab-01 --no-provision        # install only
```

## Installation and provisioning

| | Installation | Provisioning |
|---|---|---|
| What | the OS, disks, users, network, firewall, userdata | what the server's groups say: users after day one, services, their configuration |
| Defined by | the [profile](profiles.md) | the playbooks of the groups, in your repository and in the collection [ofzhur.ansible](https://github.com/ofzhur/ansible) |
| When | before the first boot | after the first boot, and again with `ump server provision` |
| Run by | `ump server reinstall` | `ump server reinstall`, `ump server provision`, or `ansible-playbook` from the repository |

Every `ump server reinstall` provisions, unless `--no-provision`: the
server is then the operating system plus the userdata, nothing more,
which is the right end for a machine you configure by hand, and for
NixOS, which is configured by its modules: the collection's playbooks
need Python and support Ubuntu, Debian and Arch Linux.

## Groups and run lists

Every server ump installs is in the group `common`. `--group` adds
groups (repeatable, or comma-separated); a group name is an Ansible
group name: lower-case letters, digits and `_`, not starting with a
digit; `all`, `ungrouped` and `localhost` are Ansible's own, `common`
is implied. Nor can the hostname be `localhost` or `127.0.0.1`: plays
and tasks for `localhost` would run on the server.

A server's **run list** is its groups in order, `common` first, and each
group is a playbook:

- `playbooks/GROUP.yml` of the repository, when it has one;
- else the collection's `ofzhur.ansible.GROUP`.

```console
$ ump server reinstall web-01 --group web,monitoring
...
Run list after the first boot: common, web, monitoring (ofzhur.ansible.common, playbooks/web.yml, playbooks/monitoring.yml).
```

ump runs the whole run list in one `ansible-playbook` limited to the
server. A group without a playbook is an error, found before anything
happens (below).

**Groups are best treated as immutable.** A server gets its groups when
it is installed, and a server that should become something else is
reinstalled with the new ones: every group starts from a fresh system,
and a server's state follows from its profile and groups alone. Adding
a group later (`ump server provision NAME --group GROUP`) suits groups
built for it, such as monitoring or backup agents; removing one takes a
reinstall.

ump records the groups in its [state](state.md), without `common`: a
successful `ump server reinstall` sets them (none without `--group`;
with `--no-reboot` too), `ump server provision` adds to them; a dry run
or a failed installation changes nothing. The listings show them as in
the inventory, `common` first: the `GROUPS` column of `ump server list`,
`groups` in its `-o json` and in `ump server show`; a server ump did not
install has none.

## The infrastructure repository

ump runs from the root of the repository. The layout it expects:

```
infra/
├── ansible.cfg                the collection's template, config/push/ansible.cfg
├── inventory/
│   ├── groups.yml             the static group tree; keeps the inventory valid before the first server
│   ├── group_vars/            common.yml, web.yml, ...
│   ├── host_vars/             overrides per host
│   └── hosts/                 written by ump: one file per server
├── playbooks/                 web.yml, db.yml, ...: the playbooks of your own groups
└── collections/ansible_collections/ofzhur/ansible    the collection, a git submodule
```

Setting it up:

```sh
git submodule add -b main https://github.com/ofzhur/ansible.git collections/ansible_collections/ofzhur/ansible
cp collections/ansible_collections/ofzhur/ansible/config/push/ansible.cfg .
mkdir -p inventory playbooks
printf 'all:\n  children:\n    common:\n' > inventory/groups.yml
printf '.cache/\n' > .gitignore
```

Ansible reads an empty inventory directory as a broken inventory, hence
`groups.yml`. The template reads collections from the repository only,
so a playbook uses `ansible.builtin` and what is installed under
`collections/`; its fact cache is `.cache/`, which stays out of git.

Before `ump server reinstall` touches anything (before the question, the
rescue system and the server's lock), it checks:

1. **The layout:** `ansible.cfg` and `inventory/` in the current
   directory, `ansible-playbook` in `PATH` (from `ansible-core`).
   Without them it stops with exit status 2 and names what is missing;
   `--no-provision` needs none of them. A world-writable directory
   stops it too: Ansible ignores an `ansible.cfg` there, and ump, which
   names it in `ANSIBLE_CONFIG`, does not take it either.
2. **The run list:** `ansible-playbook --syntax-check` on the playbooks,
   which also reads the inventory. A group without a playbook, a
   playbook that does not parse or a broken inventory stop it with exit
   status 2 and Ansible's output.
3. **The names:** a host and a group of the same name make the group's
   plays match only the host (Ansible resolves a pattern that names a
   host as that host). A hostname that is a group of the run list or of
   the inventory (`ansible-inventory --list`), or a group of the run list
   that is another host of the inventory, stops it with exit status 2;
   so does the hostname `localhost` or `127.0.0.1`.
4. **The host file:** `inventory/hosts/HOSTNAME.yml` or `HOSTNAME.yaml`
   that ump did not write for this server stops it with exit status 2:
   one without `ump_ref` (written by hand), or one of another server.

`--dry-run` makes the same checks, shows the run list with the plan and
writes nothing.

## Host files

After the installation (the new system is up and recorded) ump writes
the server's host file, `inventory/hosts/HOSTNAME.yml`, read by the
collection's inventory plugin `ofzhur.ansible.host`:

```yaml
# web-01: written by ump (ump server reinstall, ump server provision).
# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.
plugin: ofzhur.ansible.host
groups: [common, web]
vars:
  ansible_host: 203.0.113.10
  ansible_user: ump
  ump_name: web-01
  ump_ref: hetzner-cloud/234567
  ump_provider: hetzner-cloud
  ump_account: main
  ump_location: fsn1
  ump_product: cx22
  ump_os: ubuntu-26.04
  ump_profile: ubuntu-26.04
  ump_installed_at: "2026-10-14T11:58:30Z"
  ump_attributes:
    owner: alice
```

- The file name is the hostname of the installation (`--hostname`, else
  the server's name). `ansible_host` is the IPv4 address, else the IPv6
  one; `ansible_user` the administrator of the installation (`ump` with
  the published profiles). `ump_attributes` are your
  [attributes](inventory.md#attributes). Variables without a value
  (`ump_location`, `ump_product`, ...) are left out.
- ump writes the file under a dot name, which Ansible ignores, and
  renames it into place. A file of another name for the same server (an
  old hostname) is removed. A file of the hostname without `ump_ref`
  is never overwritten: remove it, and keep what it said in
  `inventory/host_vars/HOSTNAME.yml`.
- Values are data, never templates. Overrides go to
  `inventory/host_vars/HOSTNAME.yml`, group variables to
  `inventory/group_vars/`, group trees (`prod` with the children `web`
  and `db`) to `inventory/groups.yml`.
- Commit the files: the inventory is the repository's, and Ansible runs
  from it without ump.
- ump does not remove the host file of a server that is gone: `git rm`
  it. With the last one, remove `inventory/hosts/` too: Ansible fails on
  an empty directory, and so does ump's syntax check.

A server installed with `--no-provision` from the repository gets its
host file too: ump installed it, so it belongs in the inventory. The
names and the host file are checked first, as above, which takes
`ansible-inventory` in `PATH`; without it (or in a world-writable
directory), and from anywhere else, the server is in no inventory until
`ump server provision` runs from the repository.

## How ump runs Ansible

```console
$ ansible-playbook ofzhur.ansible.common playbooks/web.yml --limit web-01 --private-key ~/.config/ump/staging_ed25519
```

- In the repository's root, with `ANSIBLE_CONFIG` set to its
  `ansible.cfg`, whatever the environment says.
- As the server's administrator (the host file's `ansible_user`) with
  UMP's key; the administrator has sudo without a password, which
  `become` uses. Variables of the inventory (`ansible_user`,
  `ansible_ssh_private_key_file`) win over ump's.
- With the keys given only: `ANSIBLE_SSH_EXTRA_ARGS` (and
  `ANSIBLE_SFTP_EXTRA_ARGS`, `ANSIBLE_SCP_EXTRA_ARGS`) set to `-o
  IdentitiesOnly=yes`, so that ssh offers UMP's key (or the inventory's
  `ansible_ssh_private_key_file`) and not every key of your agent before
  it, which sshd's `MaxAuthTries` ends with "Too many authentication
  failures". Each is set only where neither the environment nor
  `ansible.cfg` sets it.
- The new system's host key is accepted on first contact, a changed one
  never: unless the environment or `ansible.cfg` set common SSH
  arguments of their own, ump sets `ANSIBLE_SSH_COMMON_ARGS=-o
  StrictHostKeyChecking=accept-new`. Ansible puts these after
  `ansible.cfg`'s `ssh_args`, and ssh takes the first value of an
  option, so the repository's own setting stays in force. A reinstall
  removes the old host key from `~/.ssh/known_hosts` before.
- Ansible's output goes to stderr; stdout stays for the result of
  `ump server reinstall -o json` (or `yaml`), which has `groups` (as in
  the inventory), `run_list`, `playbooks` and `provisioned`.

A run list that does not complete is exit status 6: the server is
installed and recorded, and in the inventory. Fix what Ansible reports
and run `ump server provision NAME`.

## The commands

**`ump server reinstall NAME [--group GROUP]... [--no-provision]`**
installs, then provisions: the checks above first, after the first boot
the host file and the run list.

- `--no-provision`: no layout needed, nothing runs; the host file is
  still written where the layout and `ansible-inventory` are there,
  after the checks of the names and the host file.
- `--no-reboot`: the installation is recorded, with its groups, but the
  new system is not up yet, so there is no host file and no run; run
  `ump server provision NAME` after the reboot.
- `--dry-run`: the checks and the plan, nothing written.

**`ump server provision NAME [--group GROUP]...`** works on a server ump
installed (the [state](state.md) has its installation), from the
repository: the same checks, then the groups added to the state, the
host file written and the run list run, under the server's lock.
Without `--group` it runs the run list the server has: a converge. A
server ump did not install is refused (exit status 2): there is nothing
to put into the inventory; reinstall it.

## The collection

[ofzhur.ansible](https://github.com/ofzhur/ansible) is the Ansible side
of ump, written once and used by every infrastructure repository that
includes it:

- `ofzhur.ansible.host`: the inventory plugin for the host files;
- `config/push/ansible.cfg`: the template of `ansible.cfg` for a push
  from a control machine (your laptop, a bastion, CI);
- the playbook `common`, the run list of every server, which imports
  `base`;
- `base`, which runs the role `users`: the users and groups of the
  group variables, with their keys and sudo. Its README says how.

A repository replaces a playbook of the collection with its own of the
same name: `playbooks/common.yml` in the repository takes the place of
`ofzhur.ansible.common` (and can import it).

## Facts and secrets

Playbooks need more than their own server: a web server needs the
address of its database, a monitoring node the list of what to watch,
every server the certificates and passwords of its services.

With a push, Ansible runs on one machine (a CI runner, a bastion, or
your laptop), connects to the servers of the run, gathers their facts
and hands each role what it needs; the template's fact cache keeps the
facts of servers outside the run between runs. Secrets are needed only
there, while the playbook runs: they live encrypted in the
infrastructure repository ([git-crypt](https://github.com/AGWA/git-crypt),
[sops](https://github.com/getsops/sops) or `ansible-vault`) and are
decrypted on the control machine, so the servers end up holding almost
nothing but what their services use. This is the more secure setup, and
the simplest: the repository and a key that opens it is all there is.
Never write a token or a private key into a profile or userdata: both
end up in git and on the server.

## Later: pull and a control plane

Not implemented yet:

- **`ansible-pull`:** the server converges itself from the repository on
  a timer, also after drift, and nothing has to reach it from outside.
  The profile's `provision` section is reserved for it (`type:
  ansible-pull`, recorded in `/etc/ump/install`); today it changes
  nothing. A pull needs more than a push: read access to the repository
  from every server (a read-only deploy key), an exchange for the facts
  of the other servers (an S3-compatible bucket), and a secrets store
  the servers can read their own secrets from (Vault or OpenBao with a
  policy per group). Every server then holds a key to the repository and
  credentials for the exchange: give each the narrowest access its
  groups need.
- **A control plane:** the same push on a schedule, from a control
  machine, against the whole inventory: the servers converge as with
  `ansible-pull`, while keys and logs stay in one place.

Until then, a pull works by hand: a userdata entry installs Ansible and
a systemd timer that runs `ansible-pull` against the repository, with a
`local.yml` that runs against `localhost` and reads what the server
knows about itself from `/etc/ump/install`.

**NixOS** needs neither, and is installed with `--no-provision`: its
configuration is the system, and the collection's playbooks do not
apply to it. Keep the modules of each group in the repository and
deploy them with `nixos-rebuild --target-host`, `deploy-rs` or
`colmena`.

## Help

Larger setups, migrations of an existing fleet to profiles and groups,
or a provisioning design of your own: help is available from the author
of UMP on a commercial basis, through the contact form at
[zhurko.de/contact](https://zhurko.de/contact/). See also [next
steps](next-steps.md).
