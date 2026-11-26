# Provision: the playbooks on the servers

`ump provision` runs the infrastructure repository's playbooks (every
`playbooks/*.yml`, in file-name order) on the fleet or on the hosts of
`--hosts`, each host logged in to as its administrator with UMP's key.
It first writes the host files of every server ump installed and records
unattended installations that ended. One run at a time: the fleet's lock.

Run it from the repository's root. Tier: Change (it changes servers);
on production hosts, show the check first.

## 1. What would change

```sh
ump provision --hosts HOSTS -- --check --diff
```

`HOSTS` is an Ansible host pattern (`web-01`, `web`, `web-01,db-01`,
`'all:!lab-*'`). Check mode still contacts every host and runs tasks
marked `check_mode: false`. `--playbook NAME` (file name without
`.yml`) narrows the playbooks. Exit 2 before anything: the layout, a
playbook that does not parse, a name clash, `--hosts` matching nothing.

## 2. Ask

Show the human the hosts, the playbooks and the diff. Without `--hosts`
the run is the whole fleet: say so.

## 3. Run

```sh
ump provision --hosts HOSTS --yes -o json
```

`-o json` gives per host `ok`, `changed`, `unreachable`, `failed`,
`skipped`, `rescued`, `ignored`, then `failed`, `unreachable` and
`exit_status`.

## 4. Failures

Exit 6 names the hosts that failed or were unreachable: read Ansible's
output for them, fix the cause in the repository (a commit, through its
review) or on the host through its runbook, then

```sh
ump provision --hosts FAILED-HOSTS --yes
```

Exit 3 after a wait: another run holds the fleet's lock (`ump config
state locks` says who). `ump config state unlock fleet` only when that
ump died (ask first).

## 5. Groups

`ump server set NAME --group monitoring` adds a group, `--ungroup`
takes one away; nothing runs until the next `ump provision`, which
writes the host file with them.
