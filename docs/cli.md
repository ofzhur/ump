# CLI

The command structure follows kubectl. `ump get servers` and `ump config`
are implemented; the rest of this document is the intended shape and will
be converted to the same style as commands are built.

## Conventions

- Commands follow the kubectl shape, `ump <verb> <resource>`: `ump get
  servers` lists, `ump config` configures. The central workflow verbs
  (`plan`, `install`, `ssh`) are top-level because they are used most.
  Resource-first forms in the draft below (`machine reboot`, `profile
  show`, ...) will become `ump reboot server`, `ump describe profile` and
  so on when they are implemented.
- Machines are addressed by a **machine reference**: `provider/id`, a
  name or an IP address (see [concepts](concepts.md#machine)). Several
  references or `--selector` can be given where it makes sense.
- Every listing supports `-o table|wide|json|yaml`. JSON output is stable
  and meant for scripting.
- Destructive commands ask for confirmation. `--yes` skips the prompt but
  needs a guard flag (see [architecture](architecture.md#safety)).
- Global flags: `--config <file>`, `--account <name>`, `-v/--verbose`,
  `--no-color`.
- UMP never prompts when stdin is not a TTY. It fails with exit code 3 and
  names the approval that is missing (see `ump approve`).

## Overview

Implemented today:

```
ump
├── get
│   └── servers  [ref...] [-o table|wide|json|yaml|name] [--no-headers]
│                                                  all servers of all accounts (--account to restrict)
├── config                                         interactive wizard: add an account, test it, save it
│   ├── view                                       the configuration, secrets redacted
│   ├── path                                       where the configuration lives
│   ├── types                                      provider types and their settings
│   ├── add      <type> <name> --set k=v ...       non-interactive form (CI, agents)
│   ├── remove   <name>
│   └── test     [<name>...]                       check credentials and API access
└── version
```

Draft of the rest:

```
ump
├── plan       <machine> --profile <name>          show what install would do
├── approve    <run-id>                            approve a plan (interactive, TTY only)
├── install    <machine>... --profile <name>       install the OS
├── ssh        <machine> [-- command]              ssh into a machine
├── facts      <machine>                           inspect hardware (enters staging env if needed)
│
├── machine
│   ├── show     <machine>
│   ├── reboot   <machine> [--hard|--power-cycle]
│   ├── power    <machine> on|off
│   ├── rename   <machine> <name> [--rdns]
│   ├── rescue   <machine> enable|disable [--flavour ...]
│   ├── import   --from terraform [file]           names and labels of machines Terraform created
│   ├── create   --account <name> --type <type> --location <loc>   (clouds)
│   └── destroy  <machine>                                          (clouds)
│
├── profile
│   ├── list
│   ├── show     <name>                            fully resolved (inheritance applied)
│   └── validate [<name>]
│
├── layout
│   ├── list
│   ├── show     <name>
│   └── validate [<name>]
│
├── image
│   ├── list
│   ├── show     <name>
│   ├── fetch    <name>                            pre-download and verify
│   └── build    <recipe>                          build an image from a recipe (later)
│
├── inventory
│   ├── export   [--format ansible|json|hosts] [--group-by env,role,provider,location,profile]
│   ├── ansible  --list | --host <name>            Ansible's script-inventory protocol, behind an exec wrapper
│   └── check                                      provider machines vs. terraform outputs vs. ansible inventory
│
├── repo
│   ├── init     [dir]                             write the reference repository skeleton
│   └── audit    [dir] [-o markdown|json]          repository health report (see skill.md)
│
├── run
│   ├── list                                       past runs
│   ├── show     <run-id>                          plan, steps, result
│   └── logs     <run-id> [-f]
│
├── skill
│   ├── install  [--user|--project] [--with-permissions]
│   └── path
│
├── doctor                                         tools, versions, credentials reachable, skill version
├── agent                                          (internal) the target-side role, started over SSH by install
└── version
```

## `ump get servers`

```
ump get servers [NAME|ID|IP|PROVIDER/ID ...] [--account <name>...]
                [-o table|wide|json|yaml|name] [--no-headers]
```

Lists dedicated servers and cloud instances from every configured account
(`--account` restricts it), queried in parallel. Positional arguments
select single servers by name, provider ID, IP address (an address inside
the machine's IPv6 prefix matches too) or `provider/id`. An account whose
API fails is reported on stderr, the others are still listed, and the exit
code is 4. A reference that matches nothing gives exit code 1.

`-o json` is a plain array of machines with stable field names:

```json
[
  {
    "provider": "hetzner-robot",
    "account": "hrobot",
    "id": "1234567",
    "name": "web-01",
    "kind": "bare-metal",
    "ipv4": "203.0.113.10",
    "ipv6": "2001:db8:1:2::/64",
    "location": "fsn1-dc7",
    "product": "AX41-NVMe",
    "status": "ready",
    "labels": {"env": "prod"}
  }
]
```

`status` is the provider's own status, lower-cased (`ready`, `running`,
`off`, `cancelled`, ...); `location` is the datacenter or region,
lower-cased; `labels` are present where the provider has them. `-o wide`
adds the IPv6, product and kind columns; `-o name` prints `provider/id`
per line. `defaults.output` in the configuration sets the default format.

## `ump config`

Without a subcommand, `ump config` is an interactive wizard: it asks for
the provider type, an account name and the credentials, asks where the
secrets should be kept (files under `~/.config/ump/secrets/` readable only
by you, environment variables, or a command such as `pass show ...`),
tests the credentials with one API call and writes the account to the
configuration. For OVHcloud it can request a consumer key for you and hand
you the validation URL. Without a terminal it refuses with exit code 3.

`ump config add <type> <name> --set KEY=VALUE ...` is the non-interactive
form. A value is plain text or a secret reference: `env:NAME`,
`file:PATH`, `command:CMD`. The credentials are tested before saving
unless `--no-test` is given; `ump config types` lists the settings of
every provider type. `view` shows the configuration with plaintext
secrets redacted (references are shown as they are), `test` checks every
account, `remove` deletes an account together with the secret files the
wizard created for it.

## Workflow commands

### `ump plan`

```
ump plan <machine> --profile <name> [--layout <name>] [--image <name>]
         [--entry rescue|kexec|virtual-media|manual] [--hostname <tmpl>]
         [-o text|json]
```

Resolves the profile against the machine and prints the plan. If facts
are not cached for the machine, `plan` needs to inspect it — for a
running machine that means entering the staging environment, which
reboots it. `plan` asks before doing that, and `--facts <file>` lets you
plan from previously collected facts instead.

### `ump install`

```
ump install <machine>... --profile <name> [plan flags]
            [--approved <run-id> | --yes --confirm-machine <ref>] [--parallel N]
            [--skip-handoff] [--keep-rescue] [--dry-run]
ump install --resume <run-id>
```

Runs the full pipeline (see [architecture](architecture.md#the-install-pipeline)).
`--dry-run` goes up to and including `plan` and then prints the
commands the agent would run. Without a TTY, an install needs either an
approval from `ump approve` or `--yes` with a guard.

### `ump approve`

```
ump approve <run-id> [--ttl 1h]
```

Shows the plan recorded by `ump plan` (or by an `install` that stopped at
the confirmation step) and records an approval bound to the plan's hash.
Refuses to run without a TTY: this is the step a human does, and an agent
in a non-interactive shell cannot do it for itself. The approval expires,
and is void if the facts or the profile change before the install. Built
for the [skill](skill.md) workflow; useful on its own for a four-eyes
process.

### `ump ssh`

```
ump ssh <machine> [--user <name>] [--staging] [-- command...]
```

Resolves the machine to an address and opens an SSH session, or runs a
command. `--staging` uses UMP's staging key and `root`, for when the
machine is in rescue.

### `ump facts`

```
ump facts <machine> [--refresh] [-o yaml|json]
```

Shows the hardware facts. Uses the cache from the last run unless
`--refresh` is given.

## Examples

```console
# set up provider accounts interactively, or without a terminal:
$ ump config
$ ump config add hetzner-robot main --set login=env:HROBOT_USER --set password=env:HROBOT_PASS
$ ump config test main

# what do I have?
$ ump get servers
$ ump get servers -o wide --account main
$ ump get servers -o json | jq '.[] | select(.status=="ready")'

# reinstall one server
$ ump plan web-01 --profile base
$ ump install web-01 --profile base

# reinstall three servers from CI, without prompts
$ ump install web-01 web-02 web-03 --profile base \
      --yes --confirm-machine web-01,web-02,web-03 --parallel 3

# take over a cloud VM that terraform created, using kexec
$ ump install hetzner-cloud/7654321 --profile base --entry kexec

# install a box in the rack through its Redfish controller
$ ump install rack1-07 --profile storage --entry virtual-media

# a machine that is not behind any API: boot something with SSH yourself
$ ump install 192.0.2.50 --profile base --entry manual

# agent-driven flow: the agent plans, a human approves, the agent installs
$ ump plan prod-web-03 --profile web -o json       # agent
$ ump approve run-7f3a                             # human, in a terminal
$ ump install prod-web-03 --approved run-7f3a      # agent

# Ansible inventory straight from the providers
$ ump inventory export --format ansible --group-by env,role,provider
```

Everything after `ump get servers` in the examples is draft.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error |
| 2 | usage / validation error |
| 3 | aborted by user or guard mismatch |
| 4 | provider API error |
| 5 | machine unreachable / timeout |
| 6 | installation failed on the target |
| 7 | verification or handoff failed (the OS is installed) |
