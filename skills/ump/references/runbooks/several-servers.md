# Install several servers at once

`ump server multiinstall` installs each server the way `reinstall`
does, side by side, and ends in one provisioning run. Every server it
names has every disk erased. Use it only for servers that each qualify
for install-fresh-server.md (they hold no data); a server that holds
data goes through reinstall-active-server.md on its own.

## 1. The servers and their plan file

Servers that share everything can be named on the command line
(`multiinstall web-01 web-02 --group web`). Otherwise write a plan file,
one entry per server:

```yaml
# fleet.yaml
defaults:
  profile: web
  group: [web]
servers:
  hetzner-cloud/1001: {}
  hetzner-cloud/1002:
    layout: lvm-raid1
  hetzner-robot/2345:
    profile: db
    group: [db]
```

The keys are the flags' names (`profile`, `layout`, `firewall`, `os`,
`hostname`, `private-network`, `private-ip`, `timezone`, `disks`, `set`,
`userdata`, `group`, `ipv6`, `mitigations`, `via`, `storage`; also
`user` and `ssh-key`, which grant access and are the human's). The file
is read strictly: an unknown key, a duplicate or a wrong type is refused
with its line.

## 2. Dry-run (Change: every server goes into the rescue system)

```sh
ump server multiinstall --plan fleet.yaml --hosts 'web-01,web-02,db-01' --dry-run --yes
```

Every check (references, profiles, hostnames, private addresses, the
repository, locks) comes before any server is touched; a failure is exit
code 2 with nothing changed. Then each server is brought into the rescue
system and its plan printed, side by side.

## 3. Ask (Destroy)

Show the human the table (server, provider, OS, layout, hostname,
private address, groups) and each plan; say that every disk of each is
erased. Ask for a yes naming the batch. Keep `--parallel` low (2 or 3)
for bare metal unless the human says otherwise: a failure is easier to
follow.

## 4. Install

```sh
ump server multiinstall --plan fleet.yaml --hosts 'web-01,web-02,db-01' --parallel 2 --yes -o json
```

A failed installation does not stop the others; its server stays where
it stopped (after a failed installer, in the rescue system). The summary
says per server `installed` and `provisioned` and why not. Exit 1: an
installation failed; 6: the provisioning did not complete; 2 or 3: it
could not run.

## 5. Verify and report

Per server as in install-fresh-server.md step 7; then the report lists
each server's outcome and what is left (servers in the rescue system,
hosts to provision again).
