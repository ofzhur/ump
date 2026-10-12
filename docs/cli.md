# CLI (draft)

This is the intended command structure. It will change during
implementation; the point is to agree on the shape.

## Conventions

- Commands are `ump <noun> <verb>`, except for the central workflow verbs
  (`plan`, `install`, `ssh`), which are top-level because they are used
  most.
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

```
ump
├── plan       <machine> --profile <name>          show what install would do
├── approve    <run-id>                            approve a plan (interactive, TTY only)
├── install    <machine>... --profile <name>       install the OS
├── ssh        <machine> [-- command]              ssh into a machine
├── facts      <machine>                           inspect hardware (enters staging env if needed)
│
├── machine
│   ├── list                                       all machines of all accounts
│   ├── show     <machine>
│   ├── reboot   <machine> [--hard|--power-cycle]
│   ├── power    <machine> on|off
│   ├── rename   <machine> <name> [--rdns]
│   ├── rescue   <machine> enable|disable [--flavour ...]
│   ├── import   --from terraform [file]           names and labels of machines Terraform created
│   ├── create   --account <name> --type <type> --location <loc>   (clouds)
│   └── destroy  <machine>                                          (clouds)
│
├── provider
│   ├── types                                      supported provider types and capabilities
│   ├── list                                       configured accounts
│   ├── add      <type> <name> [flags]
│   ├── remove   <name>
│   └── test     <name>                            check credentials and API access
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
├── config
│   ├── init                                       create a starter config
│   ├── path                                       show config locations
│   └── edit
│
├── skill
│   ├── install  [--user|--project] [--with-permissions]
│   └── path
│
├── doctor                                         tools, versions, credentials reachable, skill version
├── agent                                          (internal) the target-side role, started over SSH by install
└── version
```

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
# set up a provider account; secrets are read from the environment
$ ump provider add hetzner-robot main \
      --login-env HROBOT_USER --password-env HROBOT_PASS
$ ump provider test main

# what do I have, and what changed since last time?
$ ump machine list
$ ump machine list -o json | jq '.[] | select(.status=="ready")'

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
