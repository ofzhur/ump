# Security

ump erases disks, holds provider credentials and an SSH key that opens
every machine it installed, so a flaw in it can matter a lot. Please
report one privately rather than in a public issue.

## Reporting a vulnerability

- **On GitHub:** [report a vulnerability](https://github.com/ofzhur/ump/security/advisories/new)
  (the repository's Security tab, "Report a vulnerability").
- **By e-mail:** Oleg Zhurko <oleg@zhurko.de>.

Say what is affected (the command, the provider or the image, the
version from `ump version`), how to reproduce it, and what an attacker
gains. Leave out real tokens, passwords, keys and server addresses; a
placeholder is enough.

You get an answer within a few days. A confirmed issue is fixed in a
new release, and the advisory names you unless you prefer otherwise.

## Supported versions

Fixes go into the latest release only; `curl -fsSL
https://ump.zhurko.de/dl/get.sh | sh` updates to it, and ump says once a
day when a newer one is there.

## What counts

Anything that lets someone other than the operator read the secrets ump
keeps (the [home](docs/state.md), its backends), act on servers through
ump, run code on a machine during the rescue or the installation, or
end up with an installed system that is more open than its profile says
(users, keys, the firewall). Weaknesses of the providers' own APIs and
rescue systems are out of scope, but tell us if ump makes them worse.
