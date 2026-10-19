# Provisioning

> **Work in progress.** The roles and the commands below exist: ump
> checks the roles, shows them with the plan and records them with the
> server. Running a strategy is not implemented yet, and ump says so
> wherever it records roles. The Ansible repositories this page refers
> to will be linked here when they are published.

An installation by ump ends with the operating system ready to use: the
disks, the users and their keys, the network, the firewall, and the
[userdata](userdata.md) of the profile, which ran before the first boot.
Nothing else. Provisioning is the next step: it turns that system into
what the server is for (a web server, a database, a monitoring node) by
**roles**, applied by a **strategy** the profile names.

```console
$ ump server reinstall web-01 --role web                # install, then provision as web
$ ump server reinstall db-01 --roles base,postgres      # several roles
$ ump server provision web-01 --role monitoring         # add a role to an installed server
```

## Installation and provisioning

| | Installation | Provisioning |
|---|---|---|
| What | the OS, disks, users, network, firewall, userdata | the services and configuration of the server's roles |
| Defined by | the [profile](profiles.md) | the roles, in your repository (Ansible) |
| When | before the first boot | after the first boot, and again whenever the roles change |
| Run by | `ump server reinstall` | `ump server reinstall --role`, `ump server provision`, or the server itself (`ansible-pull`) |

Without `--role`, `ump server reinstall` provisions nothing: the server
is the operating system plus the userdata, nothing more. That is the
right end for a machine you configure by hand, with tools of your own,
or with NixOS modules in its userdata.

## Roles

A role names what a server is: `web`, `postgres`, `monitoring`, `base`.
Its content lives in your infrastructure repository (an Ansible role, or
a group of them); ump only knows its name. A server can have several
roles; a name is lower-case letters, digits, `-`, `_` and `.`.

**Roles are best treated as immutable.** A server gets its roles when it
is installed (`ump server reinstall NAME --role ...`), and a server that
should become something else is wiped and reinstalled with the new
roles. That keeps every role starting from a fresh system, makes a
server's state follow from its profile and roles alone, and is cheap:
an installation takes a few minutes.

**Adding a role later** is what `ump server provision` is for, and it
suits roles built for it: a `base` role every server gets (users added
after day one, hardening, monitoring and backup agents), which can be
applied to a running server and again at any time. Removing a role
takes a reinstall; a role is not undone in place.

ump records a server's roles with it in its [state](state.md):

- a successful `ump server reinstall` sets them to the roles given, or
  to none without `--role` (the disks were wiped); a dry run or a failed
  installation changes nothing;
- `ump server provision` adds to them;
- `ump server show` lists them, `ump server list -o wide` has a `ROLES`
  column, and `-o json` a `roles` list, which is what an inventory groups
  servers by.

## The strategies

The profile's `provision` section names how roles are applied:

```yaml
provision:
  type: ansible-bootstrap     # none | ansible-bootstrap | ansible-pull
  # the other keys belong to the strategy, see below
```

`none` (the default of every published profile) applies nothing; roles
given to a server with it are recorded only. The first strategies are
Ansible's two ways of reaching a machine: ump runs it (`ansible-bootstrap`),
or the server runs it itself (`ansible-pull`). Both read an inventory in
UMP's style: every server ump knows, with its address, administrator,
roles, attributes, account, location and installed OS, grouped by role
(`role_web`), by attribute and by account.

### a) `ansible-bootstrap` with an exported inventory

You run ump at the root of your infrastructure ("infra", "devops")
repository. ump writes the inventory there in UMP's style, and Ansible
reads it with UMP's inventory plugin, then runs your playbook for the
new server and its roles, as `ansible-playbook` always does.

```yaml
provision:
  type: ansible-bootstrap
  inventory: export           # ump writes the inventory into the repository
  playbook: site.yml
```

```
infra/
├── ansible.cfg               enables UMP's inventory plugin
├── inventory/ump.yml         written by ump: the servers, their roles and facts
├── site.yml                  maps the role_* groups to your roles
└── roles/                    web, postgres, base, ...
```

The exported file is plain YAML: it can be committed, reviewed and
diffed, and Ansible runs from it without ump, on any machine that has
the repository.

### b) `ansible-bootstrap` with ump as the inventory

The same, with no file in between: UMP's inventory plugin calls ump
whenever Ansible needs the inventory, so it is always what ump knows at
that moment (from its listing cache, so a run costs no extra API calls).
Ansible then needs ump and its [state](state.md) wherever it runs: on
your machine, or in CI with a shared state backend.

```yaml
provision:
  type: ansible-bootstrap
  inventory: plugin           # Ansible asks ump
  playbook: site.yml
```

### c) `ansible-pull`

The server applies its roles itself: ump exports the inventory in UMP's
style into the repository, commits, pushes and merges it (to the branch
the servers pull), and sets the server up to run `ansible-pull` against
the repository from a cron job (a systemd timer where there is one).
Every server converges by itself, also after drift, and nothing has to
reach the servers from outside.

```yaml
provision:
  type: ansible-pull
  repo: git@github.com:example/infra.git
  branch: main
  playbook: local.yml
  schedule: 30min             # how often the server pulls
  facts: s3://example-infra-facts/   # the facts exchange (S3, R2 or any S3-compatible store)
```

The servers need read access to the repository over SSH: the key of a
[user](users.md) of the profile, or a deploy key, registered with the
repository as read-only. Unlike a push, a pull also needs an exchange
for the facts and secrets the servers cannot get from each other (see
[facts and secrets](#facts-and-secrets) below). Never write a token or a
private key into a profile or userdata: both end up in git and on the
server; fetch it at the first boot from your secret store instead.

### A control plane for a) and b)

The push strategies run when ump runs them. To converge a fleet
regularly without `ansible-pull` on every server, run the same playbook
from a control machine on a schedule: a small Ansible role, published
with UMP's inventory plugin, sets up a cron job on a control machine (a
bastion, a CI runner) that runs the bootstrap playbook against the whole
inventory. The servers then behave as with `ansible-pull`, while keys
and logs stay in one place. This setup is meant to be open to anyone, as
the rest of UMP is.

### Which one

| | a) export | b) plugin | c) pull |
|---|---|---|---|
| Who runs Ansible | you or CI, from the repository | you or CI, with ump at hand | every server, on a timer |
| Inventory | a file in the repository, reviewed and diffed | always current, from ump | a file in the repository, merged by ump |
| Needs | SSH from where Ansible runs to the servers | the same, plus ump and its state | read access to the repository from every server |
| Good at | reviewed changes, CI without ump | interactive work, fleets that change often | large fleets, servers nobody can reach, converging after drift |
| Fails | loudly, in the run | loudly, in the run | quietly, on the server, unless monitored |
| Facts | gathered over SSH by the control machine, for every server at once | the same | each server knows its own; the others' come from an exchange |
| Secrets on the servers | almost none: they stay with the control machine | the same | what the server needs to pull, read the exchange and run its roles |

A common combination: `ansible-pull` (or the control plane) for a `base`
role on every server, and a push for the roles that change services,
rolled out on purpose and watched.

### Facts and secrets

Roles need more than their own server: a web server needs the address
of its database, a monitoring node the list of what to watch, every
server the certificates and passwords of its services. Where that
comes from is the real difference between the strategies.

**Push (a, b): exchanged over the control plane.** Ansible runs on one
machine (a CI runner, a bastion, or your laptop), connects to every
server of the run, gathers their facts and hands each role what it needs
from the others in the same run. Secrets are needed only there, while
the playbook runs: they live encrypted in the infrastructure repository
([git-crypt](https://github.com/AGWA/git-crypt), [sops](https://github.com/getsops/sops)
or `ansible-vault`) and are decrypted on the control machine, so the
servers end up holding almost nothing but what their services use. This
is the more secure setup, and the simplest: the repository and a key
that opens it is all there is.

**Pull (c): an exchange the servers share.** Every server runs Ansible
for itself and sees only its own facts, so the facts of the others, and
the secrets, have to come from somewhere the servers can all reach:

- a **facts exchange**, where every server writes its own facts after a
  run and reads the others' before the next; an S3 bucket, Cloudflare
  R2, or any S3-compatible store is enough, and cheap;
- a **secrets store** the servers can read their own secrets from:
  HashiCorp Vault or OpenBao with a policy per role, or, for a small
  fleet, the same kind of bucket, with the secrets encrypted per server
  or per role and credentials that can read only them;
- **access to the repository** for every server (a read-only deploy
  key), and the credentials of the exchange, put on the server at the
  installation from your secret store, never written into a profile.

The basic setup for a pull is therefore three things:

1. a bucket (S3, R2 or another S3-compatible store) for the facts
   exchange, with credentials the servers use;
2. access to the infrastructure repository on GitHub (or any git host)
   for UMP's commits of the inventory (UMP's key, or a deploy key with
   write access) and for the servers to pull (a read-only deploy key);
   UMP's private key itself never goes onto a server, since it opens
   every machine;
3. the exchange's address and credentials injected into each server at
   the installation, which ump will do from the profile's `provision`
   section and your secret references.

More moving parts mean more to secure: every server holds a key to the
repository and credentials for the exchange, so give each the narrowest
access its roles need, and rotate them like any other secret.

## Until the strategies run

The pieces work by hand today:

- **An inventory from ump.** `ump server list -o json` has every
  server with its roles and attributes; this script makes it an Ansible
  inventory, grouped by role, attribute, account and OS:

  ```bash
  #!/usr/bin/env bash
  # inventory/ump.sh: the fleet as ump lists it, as an Ansible inventory
  set -euo pipefail
  [ "${1:-}" = --host ] && { echo '{}'; exit 0; }
  ump server list -o json | jq '
    def group: gsub("[^A-Za-z0-9_]"; "_");
    reduce (.[] | select(.ipv4 != null)) as $m ({_meta: {hostvars: {}}};
      ._meta.hostvars[$m.name] = {
        ansible_host: $m.ipv4, ansible_user: "ump",
        ump_ref: "\($m.provider)/\($m.id)", ump_account: $m.account,
        ump_os: $m.os, ump_location: $m.location, ump_roles: ($m.roles // []),
        ump_attributes: ($m.attributes // {})}
      | reduce ((($m.roles // [])[] | "role_\(.)"),
                (($m.attributes // {}) | to_entries[] | "\(.key)_\(.value)"),
                "account_\($m.account)", "os_\($m.os // "unknown")"
                | group) as $g
          (.; .[$g].hosts += [$m.name]))'
  ```

  ```console
  $ ansible-inventory -i inventory/ump.sh --graph
  $ ansible-playbook -i inventory/ump.sh site.yml --limit web-01
  ```

  Ansible logs in as the administrator (`ump`, sudo without a password)
  with your own key, which the published user carries (`keys: [ump,
  local]`); in CI, give the pipeline its own [named key](keys.md) in the
  users of your profiles rather than UMP's.

- **Pull from the first boot.** A userdata entry installs Ansible and a
  timer that runs `ansible-pull`; the first run happens at the first
  boot:

  ```yaml
  # in userdata.yaml (ump config userdata create ansible-pull), listed in the profile after ubuntu
  ansible-pull:
    description: converge with the repository every 30 minutes
    type: bash
    script: |
      #!/bin/bash
      set -euo pipefail
      apt-get install -y --no-install-recommends ansible-core git      # Arch: pacman -S --noconfirm --needed ansible-core git
      cat > /etc/systemd/system/ansible-pull.service <<'UNIT'
      [Unit]
      Description=Converge with the infrastructure repository
      Wants=network-online.target
      After=network-online.target

      [Service]
      Type=oneshot
      ExecStart=/usr/bin/ansible-pull -U https://git.example.com/infra.git -C main \
        -d /var/lib/ansible-pull -i localhost, local.yml
      UNIT
      cat > /etc/systemd/system/ansible-pull.timer <<'UNIT'
      [Timer]
      OnBootSec=2min
      OnUnitActiveSec=30min
      RandomizedDelaySec=5min

      [Install]
      WantedBy=timers.target
      UNIT
      systemctl enable ansible-pull.timer
  ```

  `local.yml` runs against `localhost` and picks its roles from what the
  server knows about itself, such as the record ump left in
  `/etc/ump/install` (profile, OS, layout, interfaces, and the provision
  section of the profile as `provision=...`, `provision_repo=...`).

- **NixOS** needs neither: its configuration is the system. Keep the
  modules of each role in the repository and deploy them with
  `nixos-rebuild --target-host`, `deploy-rs` or `colmena`.

## Repositories

- **UMP's Ansible collection:** the inventory plugin (for a, b and c),
  the control-plane role, and the facts exchange for `ansible-pull`
  (an S3-compatible bucket). Link to follow.
- **An example infrastructure repository** laid out for the three
  strategies. Link to follow.

## Help

Larger setups, migrations of an existing fleet to profiles and roles,
or a provisioning design of your own: help is available from the author
of UMP on a commercial basis, through the contact form at
[zhurko.de/contact](https://zhurko.de/contact/). See also [next
steps](next-steps.md).
