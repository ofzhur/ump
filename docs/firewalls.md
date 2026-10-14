# Firewalls

A firewall says what an installed server lets in: an input policy and a
list of allowed protocols and ports. The installer writes it as
`/etc/nftables.conf` and enables `nftables.service`, so the rules are
there from the first boot, before any service starts.

```console
$ ump config firewall list
NAME      POLICY   ALLOW                           STATE   DESCRIPTION
default   drop     icmp, ssh                       ok      ICMP and SSH, nothing else
open      accept   everything                      ok      no firewall
web       drop     icmp, ssh, tcp/80, tcp/443      ok      ICMP, SSH, HTTP and HTTPS
```

## Where firewalls live

Named firewalls are the entries of `firewalls.yaml` in the profiles
directory, `~/.config/ump/profiles/firewalls.yaml`; the first use
downloads the published [`firewalls.yaml`](../profiles/firewalls.yaml)
there (see [profiles](profiles.md#where-profiles-live)). A
[profile](profiles.md) names one in its `firewall` field, or carries a
firewall of its own as a mapping; a profile without the field installs no
firewall. `ump server reinstall --firewall NAME` picks a named one for one
installation.

```yaml
firewalls:
  default:
    description: ICMP and SSH, nothing else
    policy: drop
    allow: [icmp, ssh]
```

## What the rules do

With `policy: drop`, the ruleset accepts on **input** the loopback
interface, established and related connections (the answers to the
server's own connections) and the allowed entries, and drops everything
else. **Output** is always accepted. **Forward** is accepted too, with one
exception: a new connection that comes in on an external interface
(`ext*`, see [interface names](profiles.md#interface-names)) is held to
the same allowed entries, matched on the port it was addressed to
(`ct original proto-dst`, because the port a container publishes is
rewritten by DNAT before the forward hook).

So containers and virtual machines on the server reach out freely, and a
port Docker publishes with `-p 8080:80` is reachable from outside only
when `tcp/8080` is allowed, whatever rules the container engine adds:
its chains and UMP's are both evaluated, and a drop in either is final.

`policy: accept` means no firewall: nothing is written and `nftables` is
not enabled. The image ships `nftables`; the installer installs it when a
firewall needs it and the image lacks it.

## Fields

```yaml
policy: drop                  # drop: only what is allowed gets in; accept: no firewall
allow:                        # the entries, each one of:
  - icmp                      #   ICMP (also: ping)
  - ssh                       #   TCP port 22
  - tcp/80                    #   a TCP port
  - udp/53                    #   a UDP port
  - "tcp/8000-8100"           #   a range
  - {proto: tcp, port: 443, from: 10.0.0.0/8}   # with a source address or network
description: ...              # optional; shown by ump config firewall list
```

| Field | Meaning |
|---|---|
| `policy` | `drop` (the default when omitted) or `accept`. |
| `allow` | The entries that get in. Each is a string (`icmp` or `ping`, `ssh`, `tcp/PORT`, `udp/PORT`, `tcp/FROM-TO`, `udp/FROM-TO`) or a mapping with `proto` (`icmp`, `tcp`, `udp`), `port` (a port or a range, for `tcp` and `udp`) and optionally `from` (an address or a network in CIDR notation). Ports are 1–65535. |
| `description` | One line, shown by `ump config firewall list`. |

Quote a range in YAML (`"tcp/8000-8100"`), as the plain form would still
parse but reads badly. `icmp` has no port. The installer checks the
rendered ruleset with `nft -c` where it can, so a rule nftables rejects
stops the installation with the message instead of failing the first
boot.

## The published firewalls

| Name | Policy | Allows | For |
|---|---|---|---|
| `default` | drop | `icmp`, `ssh` | every server; the default profile names it |
| `web` | drop | `icmp`, `ssh`, `tcp/80`, `tcp/443` | web servers and reverse proxies |
| `open` | accept | everything | no firewall, for servers behind another one or with a container engine managing its own |

## Managing firewalls

```
ump config firewall                               # = list (alias: fw)
ump config firewall list                          # the firewalls, their policy and what they allow
ump config firewall get NAME                      # print one
ump config firewall create [NAME] [--from NAME]   # copy an entry (default: default), open the file in your editor; asks on a terminal
ump config firewall edit [NAME]                   # your editor on firewalls.yaml, then a check of every firewall
ump config firewall delete NAME [--yes]
```

`create` and `delete` edit `firewalls.yaml` in place; its comments
survive. `edit` checks every firewall when the editor returns and keeps
the file either way, naming the problem. The command has the aliases
`fw` and `firewalls`.

## Using a firewall

```yaml
# in a profile: by name
firewall: web

# or inline
firewall:
  policy: drop
  allow: [icmp, ssh, tcp/443, {proto: tcp, port: 9100, from: 10.0.0.0/8}]
```

```console
$ ump server reinstall web-01 --firewall web
$ ump server reinstall web-01 --firewall open     # no firewall this time
```

On the installed system, the rules are `/etc/nftables.conf`, loaded by
`nftables.service` at boot; `nft list ruleset` shows what is active, and
`/etc/ump/install` records the policy (`firewall=drop`). To change the
rules later, edit the file and restart the service, or let your
configuration management own it from there: UMP writes it once, at
installation.

## Examples

A mail server, with the submission and IMAP ports and ICMP:

```yaml
mail:
  description: SSH, SMTP, submission, IMAPS
  policy: drop
  allow: [icmp, ssh, tcp/25, tcp/465, tcp/587, tcp/993]
```

A WireGuard gateway whose administration ports are reachable from the
internal network only:

```yaml
vpn:
  description: WireGuard from everywhere, SSH and node exporter from the internal network
  policy: drop
  allow:
    - icmp
    - udp/51820
    - {proto: tcp, port: 22, from: 10.0.0.0/8}
    - {proto: tcp, port: 9100, from: 10.0.0.0/8}
```

A Docker host that publishes a range of ports:

```yaml
docker:
  description: SSH and the published container ports
  policy: drop
  allow: [icmp, ssh, tcp/80, tcp/443, "tcp/8000-8100"]
```
