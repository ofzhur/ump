# ump commands: what each one touches

For ump v0.8.0. The full reference is `ump <command> --help` and
docs/cli.md of the ump repository. This sheet says, per command, what it
reaches and what it can break.

## Conventions

- **Server references**: a name, a provider ID, an IP address (an
  address in the server's IPv6 prefix too) or `provider/id`. A name that
  matches several servers is asked about on a terminal and fails with the
  candidates without one; `provider/id` or `--account NAME` picks one.
  Prefer `provider/id` in commands that change or erase a server.
- **Output**: listings take `-o table|wide|json|yaml` (`server list`
  also `name` and `ansible`); JSON is stable and meant for scripts.
  Commands that act take `-o json|yaml` for their result.
- **No terminal**: ump never prompts when stdin is not a terminal. A
  command that would ask (reinstall, multiinstall, rescue, reboot, stop,
  unrescue, provision without `--hosts`, deletes) fails with exit code 3
  and says what was needed; `--yes` answers the question.
- **Global flags**: `--config FILE|DIR`, `--account NAME` (repeatable),
  `-v` (logs each provider API request to stderr: method, URL with any
  password in it redacted, status, time; never headers such as
  `Authorization`, never bodies).
- **Waits**: 15 minutes by default (`wait:` in `config.yaml`, or
  `UMP_WAIT`); a wait that runs out is exit code 5.

## Touches

**API** = the provider's API only. **Running system** = logs into or
interrupts what the server runs now. **Rescue boot** = the server leaves
its running system for the UMP rescue system (RAM; disks untouched).
**Disks** = erases them. **State** = ump's own files (`state.yaml`,
profiles, the listing cache), local or in the shared backend.

| Command | Touches | Notes |
|---|---|---|
| `ump version` | nothing | daily, ump says when a newer release exists |
| `server list [REF...] [-o ...] [--diff] [--since T] [--cached] [--refresh]` | API (read); state (listing cache) | a server every account of its type leaves out and its account answers 404 for is forgotten, with its private addresses (stderr says so); `--cached` asks no API and forgets nothing; exit 4 = an account could not be listed (the others are listed) |
| `server list -o ansible` | as above | the Ansible inventory of the servers ump installed |
| `server show REF... [-o json]` | API (read) | everything the provider reports, in sections |
| `server rdns get [REF...]` | API (read) | |
| `server rdns set REF [PTR] [--ip ADDR]`, `rdns delete` | API (public reverse DNS) | external-facing; OVHcloud and Linode need the forward record first |
| `server rename REF NEW` | API (the provider's label) | the next installation takes it as hostname; reverse DNS is not touched; at DigitalOcean an FQDN also sets reverse DNS |
| `server set REF ...` | state | profile, price, attributes, groups, private addresses; groups and addresses reach the inventory at the next `ump provision` |
| `server add NAME --ipv4 ...`, `server remove NAME` | state | servers no provider lists (account `manual`) |
| `server start REF [--wait]` | API (power on) | |
| `server reboot REF [--hard\|--power-cycle] [--wait] [--yes]` | API; running system (restarts) | ACPI by default; `--hard` resets, `--power-cycle` cuts the power |
| `server stop REF [--hard] [--yes]` | API; running system (shuts down) | OVHcloud dedicated servers cannot be switched off through the API |
| `server rescue REF [--yes] [-- CMD]` | running system (stops); rescue boot | never erases; with `-- CMD` runs CMD in the rescue system and exits with its status; `-o json` prints the provider's rescue password where it issues one; refused without `--force` at providers with no rescue system of their own (DigitalOcean, Vultr, manual servers), where a failed jump leaves only the console |
| `server unrescue REF [--reboot] [--wait] [--yes]` | API; running system (`--reboot` resets) | disarms the provider's rescue system so the next boot is from disk; the way back after a rescue or a dry-run |
| `server reinstall REF --dry-run [--no-provision]` | running system (stops); rescue boot | checks, prints the installer's plan and the playbooks, leaves the server in the UMP rescue system (`Dry run done; NAME stays in the UMP rescue system`); nothing erased or recorded |
| `server reinstall REF [--yes]` | rescue boot; **disks** (all erased); state; repository (host file); Ansible on the fleet or `--hosts` | at OVHcloud VPS and Vultr the provider may first rebuild the server with its stock image; `--no-provision` installs only; `--no-reboot` leaves it in the rescue system |
| `server reinstall REF --unattended` | **disks** at the next network boot (PXE machines only) | arms the installation; `--no-wait` returns once armed |
| `server multiinstall REF... \| --plan FILE [--parallel N]` | as `reinstall`, for every server named, then one provisioning run | every check comes before any server is touched; `--dry-run` brings every server into the rescue system |
| `provision [--hosts PATTERN] [--playbook NAME] [-- ansible-args]` | repository (host files); Ansible on the hosts; state (records ended unattended installations) | without `--hosts` the whole fleet and a question; `-- --check --diff` still contacts every host; arguments after `--` may not be `--limit` or `--inventory` |
| `ssh REF [--user NAME] [--staging] [-- CMD]` | running system (a login as the administrator) | read-only commands only unless a runbook says otherwise; `--staging` for rescue systems (UMP's key, no host key check) |
| `config isp list\|get\|types` | nothing (reads the configuration) | shows where each secret comes from, never its value |
| `config isp test [NAME...]` | API (one call per account) | |
| `config isp add\|edit\|remove` | state (accounts, secret files) | human only: credentials |
| `config os\|disk-layout\|firewall\|network\|user\|userdata list\|get` | nothing | `config os list` marks the default with `*`; `config user list` shows sudo and keys |
| `config os create\|edit\|delete\|update\|default`, same verbs for `disk-layout`, `firewall`, `network`, `userdata` | state | future installations only; `update` refreshes the published entries and keeps yours |
| `config user create\|edit\|delete` | state (users and their keys) | human only: access |
| `config key list\|get` | nothing | `get` prints UMP's public key and fingerprints |
| `config key add\|edit\|remove` | state (`keys.yaml`) | human only: access |
| `config key rescue REF...`, `--clear` | state (keys of every future rescue system) | human only: access; without arguments it shows them (Observe) |
| `config key roll [--no-servers] [--drop-old]` | every server ump installed (their keys); state | human only |
| `config pxe list\|get` | nothing | |
| `config pxe create\|edit\|delete` | state; the PXE servers' `site.json` (each ends with `apply`) | with `mode: install`, `apply` arms an unattended installation: **disks** at the next boot |
| `config pxe apply [--yes]` | the PXE servers; **disks** of machines with `mode: install` at their next boot | |
| `config state view\|test\|locks` | nothing (reads; `test` contacts the backend) | |
| `config state pull\|push [--force]` | state (mirror and backend) | `--force` overwrites one side |
| `config state unlock SERVER\|fleet` | state (a lock) | only for a lock an ump left behind when it died: `locks` says who holds it |
| `config state setup\|set\|migrate\|clear` | where the state lives | human only |
| `pxe status [--log NAME]` | nothing (reads the backend) | |
| `pxe disarm NAME...` | state (an armed boot) | |
| `pxe serve`, `pxe unit` | a long-running server on this host (HTTP, optional TFTP) | |

## Leaving the rescue system

After `rescue`, a `--dry-run`, a failed installation or `--no-reboot`:

```sh
ump server unrescue NAME --reboot --yes   # disarm the provider's rescue system, reset: boots from disk
ump server reinstall NAME ...             # or install now: a server already in the UMP rescue system is used as it is
```

The UMP rescue system runs from RAM, so any reset leaves it. Hetzner
clears its rescue flag once its rescue system booted; OVHcloud dedicated
servers keep rescue as their boot option until `unrescue`.

## JSON for scripts

```sh
ump server list -o json                                   # an array of machines
ump server list -o json | jq -r '.[] | select(.status=="off") | "\(.provider)/\(.id)"'
ump server show web-01 -o json | jq '.[0].details'
ump server list --diff -o json                            # accounts, changes, unchanged
ump server reinstall NAME --dry-run --no-provision --yes -o json   # the plan as data
ump provision --hosts web-01 --yes -o json                # the recap per host
```

Machine fields: `provider`, `account`, `id`, `name`, `kind`, `ipv4`,
`ipv6`, `location`, `product`, `status`, `labels`, and from the state
`os`, `installed_at`, `groups`, `profile`, `layout`,
`private_networks`, `price`, `attributes`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error, including a failed installation (the server stays in the rescue system) |
| 2 | usage or validation error |
| 3 | aborted: the question was answered no or left, or there was no terminal and no `--yes` |
| 4 | provider API error, or an account that could not be listed |
| 5 | the server did not answer in time (rescue system, reboot, new system) |
| 6 | provisioning failed: Ansible did not complete; the hosts that failed or were unreachable are named (after a reinstall, the server is installed and recorded) |
