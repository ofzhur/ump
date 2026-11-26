# Install a server that holds no data

For a new bare-metal server, a new cloud VM or a PXE machine that holds
nothing anyone needs: nothing on its disks to keep, nothing it serves.
If either is not certain, use reinstall-active-server.md.

Tiers (SKILL.md section 3): steps 1 to 3 Observe; step 4 Change (the
server goes into the rescue system); step 6 Destroy, with the human's
approval of step 5.

## 1. Find the server

```sh
ump server list --refresh                     # is it there, under which account and name?
ump server show provider/id                   # product, disks where the provider tells, rescue, contract
```

Use `provider/id` from here on: a name can match several servers. If the
server is not listed, the account is missing or the order is not
delivered yet; tell the human.

## 2. Settle the name, the profile and the groups

- **Name**: the provider's name becomes the hostname. If it does not
  follow the fleet's naming, propose `ump server rename provider/id
  NEW-NAME` (Change) or `--hostname`; ask.
- **Profile**: the one set for the server (`ump server list -o wide`
  shows it), else the default (`ump config os list`, marked `*`), or
  the one the human named. Read it: `ump config os get PROFILE`; its
  layout `ump config disk-layout get LAYOUT`; its firewall `ump config
  firewall get NAME`; its users `ump config user get NAME`.
- **Groups**: the Ansible groups the server goes into (`--group web`),
  from the task or the repository's conventions.
- Do not add users or keys (SKILL.md rule 5). If the profile's users are
  not what the server needs, stop and ask the human to change the profile.

## 3. Check the repository

From the infrastructure repository's root: `ansible.cfg`, `inventory/`,
`playbooks/` exist. Without a repository, plan with `--no-provision` and
say that the server will not be configured beyond the base system.

## 4. The dry-run

```sh
ump server reinstall provider/id --profile PROFILE --group GROUP --hosts HOSTNAME --dry-run --yes
```

It brings the server into the UMP rescue system and prints the plan:
machine (architecture, firmware, CPUs, memory), every disk with its
partitions, arrays and volumes, the network, the users, the userdata,
the firewall, the playbooks. Read it:

- every disk listed is one you expect to be erased, and none is missing;
- the layout fits (RAID where the human asked for it);
- the hostname, the address, the groups are right;
- the users are the profile's.

The server now waits in the rescue system. Exit 2 means a check failed
(the message names it); nothing was changed beyond the rescue boot.

## 5. Ask

Show the human, in one message: the server (`provider/id`, name,
address, product), the profile, the dry-run's plan (disks, layout,
hostname, users, firewall), the groups and playbooks, the command you
will run, and that every disk of the server is erased. Wait for an
explicit yes for this server.

## 6. Install

```sh
ump server reinstall provider/id --profile PROFILE --group GROUP --hosts HOSTNAME --yes -o json
```

The same flags as the dry-run. The server is already in the rescue
system, so ump uses it as it is. ump installs, reboots, waits for SSH
as the administrator with UMP's key, records the installation, writes
`inventory/hosts/HOSTNAME.yml` and runs the playbooks on `--hosts`.

## 7. Verify

```sh
ump server list provider/id                   # OS and installed_at
ump ssh HOSTNAME -- hostnamectl
ump ssh HOSTNAME -- lsblk
```

The JSON result says `provisioned: true`; the recap line of the host has
nothing failed or unreachable. Exit 1: the installer failed, the server
is in the rescue system (troubleshooting.md). Exit 5: the new system did
not answer (host-down.md). Exit 6: installed and recorded, not
provisioned (provision.md).

## 8. Report

What ran, the OS and layout, the hostname and groups, the host file
written (to commit), anything left for a human (SKILL.md section 6).
