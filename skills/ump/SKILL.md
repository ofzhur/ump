---
name: ump
description: Operate servers with UMP (ump, the Unus Mundus Provisioner) - list the servers of every provider account, show one, install or reinstall an operating system on a bare-metal server, cloud VM or PXE machine, boot it into a rescue system, reboot, stop or start it, set its reverse DNS, provision it with the Ansible playbooks of an infrastructure repository after the installation, and keep the fleet inventory (what changed, prices, attributes, groups, the Ansible inventory). Use when asked to add, install, reinstall, rescue, power or inspect a server that ump manages, to run ump provision, or when a task mentions ump, ump.zhurko.de or a server at Hetzner, OVHcloud, Vultr, Linode or DigitalOcean that ump lists.
license: Apache-2.0
metadata:
  ump-version: ">=0.8.0 <0.9"
---

# Operating servers with UMP

`ump` is a single binary that lists the servers of every configured
provider account and puts the same operating system, in the same shape,
on any of them, from a profile. An installation erases every disk of the
server. This skill says what to run, in which order, and what never to
run without a human.

Written for ump v0.8.0. Everything here uses commands that exist in that
version; the CLI reference is `ump <command> --help` and
https://github.com/ofzhur/ump/blob/main/docs/cli.md.

## 1. Orient

Before the first action in a session:

```sh
ump version                        # which ump; the skill is written for 0.8.x
ump config isp list                # the provider accounts, where their secrets come from (never the values)
ump config state view              # where the state lives: ~/.config/ump, or a shared backend
ump server list                    # every server of every account; exit 4 = an account could not be listed
ump config os list                 # the profiles, the default marked with *
```

If `ump` is not installed, or an account is missing, stop and tell the
human: installing ump, adding accounts and entering credentials are
theirs (`ump config isp add` is a wizard that asks for secrets).

Find the infrastructure repository if the task provisions: `ump server
reinstall` and `ump provision` run Ansible from the current directory
and need `ansible.cfg`, `inventory/` and `playbooks/` there
(references/ownership.md).

## 2. Rules

1. **Observe freely, change with a plan, destroy only with a human's
   approval of that plan.** The tiers are in section 3.
2. **No prompts.** Without a terminal ump never asks: a command that
   would ask fails with exit code 3, and `--yes` answers the question.
   Pass `--yes` only to a command whose tier allows it now, never to get
   past a refusal.
3. **Erasing disks is never casual.** `ump server reinstall` and
   `multiinstall` erase every disk of every server they name, and
   `--unattended` and `ump config pxe apply` arm the same for the next
   boot. Run them only for servers the human approved by name, after
   showing the plan (section 4), one server at a time unless the
   approval names a batch.
4. **`--dry-run` is not harmless.** It brings the server into the UMP
   rescue system (a kexec from the running system, or a reset into the
   provider's rescue system), so the running system stops serving; the
   disks are untouched and the server stays in the rescue system until
   `ump server unrescue NAME --reboot`. On a server that serves
   anything, a dry-run is a Change that needs the same go as a reboot.
5. **Access is the human's decision.** Never add, change or remove
   users or keys: not in profiles (`users`), not in `users.yaml`
   (`ump config user ...`), not in `keys.yaml` (`ump config key
   add|edit|remove`), not with `ump config key rescue REF` or `roll`, not
   with `--user` or `--ssh-key` on a reinstall (`--user` replaces the
   users, each with sudo; `--ssh-key` adds keys to the administrator).
   Use the profile as the human wrote it, and show its users in the plan.
6. **Secrets stay out of your context.** The state (`~/.config/ump/`, or
   its mirror under `~/.cache/ump/homes/`) holds the accounts' secret
   files, the backend's credentials and UMP's private key
   (`staging_ed25519`) in clear text. Never read, print, copy or edit
   those files; use ump commands, which show where a secret comes from,
   not its value. `ump server rescue -o json` prints a provider's rescue
   password where the provider issues one: do not use `-o json` there
   unless you need it, and never repeat the password.
7. **No improvisation on servers.** Change a server through ump, the
   repository's playbooks or a runbook, not with hand-written commands
   over `ump ssh`. Diagnostic commands over `ump ssh` and in the rescue
   system are read-only.
8. **Name the blast radius.** Without `--hosts`, the provisioning after a
   reinstall and `ump provision` run every playbook on the whole fleet;
   pass `--hosts <hostname>` for the server at hand unless the task is
   the fleet.

## 3. Tiers

| Tier | Commands | You may |
|---|---|---|
| **Observe** | `version`; `server list` (with `-o`, `--diff`, `--since`, `--cached`, `--refresh`); `server show`; `server rdns get`; `config <entity> list\|get`; `config isp list\|get\|types\|test`; `config key list\|get`, `config key rescue` without arguments; `config state view\|test\|locks`; `pxe status`; `ump ssh NAME -- <read-only command>` | run them freely |
| **Change** | `server rename`, `rdns set\|delete`, `set`, `add`, `remove`; `server start`, `reboot`, `stop`, `rescue`, `unrescue`; `reinstall --dry-run`, `multiinstall --dry-run`; `provision` (also with `-- --check --diff`); `config os\|disk-layout\|firewall\|network\|userdata create\|edit\|delete\|update\|default`; `config pxe create\|edit\|delete` of a machine whose mode is not `install`; `config state pull\|push\|unlock`; `pxe disarm`, `pxe serve` | after showing what it does and getting a go from the human; reboot, stop, rescue and dry-run of a server that serves count as an outage |
| **Destroy** | `server reinstall`, `server multiinstall` (without `--dry-run`, also `--unattended`); `config pxe apply` and any `config pxe` save that arms a machine with `mode: install` | only after the human approved the plan of section 4 for these servers by name, in this session |
| **Human only** | accounts and credentials (`config isp add\|edit\|remove`); users and keys (rule 5: `config user ...`, `config key add\|edit\|remove\|roll`, `config key rescue REF`, `--user`, `--ssh-key`, profile `users`); where the state lives (`config state setup\|set\|migrate\|clear`); a reinstall of a server that holds data (references/runbooks/reinstall-active-server.md) | prepare the exact command and hand it over |

What each command touches, in detail: references/ump-cli.md.

## 4. The plan and the approval

ump has no plan file and no approval command yet: `ump plan`, `ump
approve`, `ump inventory`, `ump repo`, `ump machine`, `ump skill` and
`ump doctor`, which the design outline (docs/skill.md) describes, do not
exist. Today's substitutes:

| Needed | Today |
|---|---|
| a plan | `ump server show NAME` (what the provider says), `ump config os get PROFILE` (image, layout, users, firewall), and the output of `ump server reinstall NAME --dry-run --hosts HOSTNAME`: every partition, array and volume per disk, the network, the users, the userdata, the firewall and the playbooks |
| an approval | the human's explicit yes in the conversation to that plan, for these servers by name; nothing you produced yourself counts |
| the inventory | the host files ump writes (`inventory/hosts/HOSTNAME.yml`), or `ump server list -o ansible` |
| a drift check | `ump server list --diff`, `--since 7d` |

An approval is void when the facts changed before the installation (a
different server behind the name, other disks, another profile): show
the plan again.

## 5. Pick the procedure

| The request | Read |
|---|---|
| install a server that holds no data yet (new, or empty) | references/runbooks/install-fresh-server.md |
| reinstall a server that holds data or serves | references/runbooks/reinstall-active-server.md |
| install several servers at once | references/runbooks/several-servers.md |
| converge servers with the playbooks, check what Ansible would change | references/runbooks/provision.md |
| a server is down, does not answer, did not come back | references/runbooks/host-down.md |
| what is there, what changed, prices, attributes, groups, a server no provider lists | references/runbooks/inventory-and-drift.md |
| which tool or file owns a fact | references/ownership.md |
| a command failed (exit code, message) | references/troubleshooting.md |

## 6. Report

End with:

- what ran, per server: the command lines (without secrets), the exit
  codes, the OS and layout installed, the hostname, the groups;
- what was verified: `ump server list NAME` shows the OS and
  `installed_at`, `ump ssh NAME -- hostnamectl` answers, the
  provisioning recap (`Provisioned N hosts, M changed`, nothing failed
  or unreachable);
- what changed in the repository (host files written; committing them
  is the repository owner's step unless the task says otherwise);
- what is left for a human: servers left in the rescue system, failed or
  unreachable hosts, approvals not given, credentials or keys to add.
