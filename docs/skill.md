# The UMP skill

> **Design outline.** The skill described here does not exist yet, and
> neither do the `ump` commands it relies on (`ump plan`, `ump approve`,
> `ump inventory`, `ump repo`, `ump machine import`, `ump skill`,
> `ump doctor`). This document records the intent so that the tool grows
> toward it; the [roadmap](roadmap.md) says where it stands. Everything
> that exists today is in the [CLI reference](cli.md).

UMP is meant to ship in two parts that are designed together:

| | **`ump`, the tool** | **the UMP skill** |
|---|---|---|
| What it is | A deterministic CLI plus the on-target agent | Packaged operating knowledge for an AI agent, in the Agent Skills format |
| What it encodes | *Mechanism*: how an OS gets onto a machine | *Procedure*: what to do when, with which tool, in which order — and what never to do |
| Who uses it | People at a terminal, scripts, CI — and agents | Agents, and the people who direct them |
| What it knows about | Providers, machines, images, layouts, profiles | The whole infrastructure repository: Terraform, UMP, Ansible, runbooks, conventions |

The tool stays small and sharp. Everything that is judgement rather than
mechanism — how a machine goes from order to service, which tool owns
which fact, what to check every week, how to take over a repository that
has grown for years — is written down once, in a form an agent can load
and follow and a human can read.

The target user is generic: anyone who runs a mix of cloud and bare
metal, has an agent (Claude Code or another one that supports skills) and
a repository of Terraform and Ansible. They install the skill, point the
agent at the repository, and most of the infrastructure work is done for
them — within a safety model that keeps the irreversible decisions human.

## Why a skill

Most infrastructure work is toil of a particular shape: read the
conventions, edit declarative files, run a plan or check mode, compare
two states, follow a runbook, write a report. Agents are good at this —
*if* the conventions are written down, the plan and check modes exist,
and the tools refuse to do dangerous things casually.

They are bad at three things: knowing a team's conventions without being
told, resisting the urge to improvise shell commands on production, and
judging blast radius. The skill supplies the conventions. UMP's design —
plan before install, guards, approval separated from execution — removes
most of the room for improvisation. The safety model below bounds the
blast radius.

That is also why "90% of infra work" is a realistic aim only for a
repository that is **organized** first: one owner per fact, generated
seams instead of hand-copied data, plan/check modes for every change,
secrets out of git. On a snowflake repository the agent is mostly useful
for the audit. The skill therefore has two jobs, in this order:

1. **Organize** — bring a repository to the reference layout and
   conventions, incrementally.
2. **Operate** — run the day-to-day work on top of it.

## Format and distribution

The skill uses the Agent Skills format: a directory with a `SKILL.md`
(YAML frontmatter with `name` and `description`, then instructions) and
optional `references/`, `scripts/` and `assets/`. The description is what
makes an agent pick the skill up; `SKILL.md` is read in full when it does;
references are loaded only when a workflow needs them. `SKILL.md` stays
short (a few hundred lines) and points outward.

```
skills/ump/
├── SKILL.md                    rules, the decision tree, pointers — read every time
├── references/
│   ├── ownership.md            which tool owns which fact; the seams between them
│   ├── conventions.md          reference repository layout, naming, pinning, secrets, CI
│   ├── ump-cli.md              command cheat sheet: JSON shapes, exit codes, error codes
│   ├── terraform.md            how the skill expects Terraform to be used
│   ├── ansible.md              how the skill expects Ansible to be used
│   ├── runbooks/               one file per workflow (see below)
│   ├── adoption.md             audit checklist and migration phases for an existing repository
│   └── troubleshooting.md      failure signatures of ump / terraform / ansible → next step
└── assets/
    └── templates/              infra PR description, runbook, ADR, health report
```

The skill ships **no scripts**. Everything executable it relies on is a
subcommand of `ump` — `ump repo audit`, `ump repo init`,
`ump inventory check` — because `ump` is a single
binary: no interpreter is needed on the operator's machine, the logic is
tested, versioned and redacts secrets like the rest of the tool, it cannot
drift from the CLI, and an agent cannot "fix" a helper script by editing
it. The skill itself is embedded in the binary; `ump skill install` writes
it out.

Sketch of the entry point:

```markdown
---
name: ump
description: Operate hybrid infrastructure (cloud VMs and bare metal) with UMP,
  Terraform and Ansible. Use when asked to add, provision, reinstall or
  decommission servers, write or run Terraform or Ansible in an infrastructure
  repository, check for drift, audit or reorganize an infra repo, rotate users
  or SSH keys, or respond to a host that is down.
license: Apache-2.0
metadata:
  ump-version: ">=0.3 <1.0"
---

# Operating hybrid infrastructure with UMP

1. Orient: run `ump doctor`; find the repository root and detect its layout
   (reference layout, partial, or foreign → references/adoption.md).
2. Ownership: never edit a fact in the wrong place → references/ownership.md.
3. Rules: observe freely; change through plan + PR; destroy only with a
   human approval you did not produce yourself (full text below).
4. Pick the runbook for the request → references/runbooks/<name>.md.
5. Report: what changed, what was verified, what is left for a human.
```

The skill lives in this repository under `skills/ump/` and is embedded
into the binary at build time, because it describes the CLI's commands and
output. `ump skill install [--user|--project]` writes it into the agent's
skill directory; `ump doctor` warns when the installed copy and the CLI
disagree on versions. The body uses only files and shell commands — no
agent-specific tool names — so it ports between agents that support the
format.

## Who owns what

The single most important thing the skill knows. Each fact has exactly
one owner; everything else is derived.

| Concern | Owner | Lives in | Consumed by |
|---|---|---|---|
| Cloud resources: instances, volumes, networks, firewalls, load balancers, DNS | Terraform | `terraform/` + remote state | UMP (import), Ansible (inventory) |
| Bare metal contracts, rescue, power, rDNS | the provider | provider API, driven by `ump` | UMP |
| Which machines exist, their addresses, location, product, state | provider APIs | — | `ump server list`, `ump inventory export` |
| Per-machine intent: profile, environment, role, overrides | the machine overlay | `ump/machines.yaml` (+ provider labels where they exist) | UMP, Ansible groups |
| OS, disks, boot, base network, base packages | UMP profiles | `ump/` | the agent on the target |
| Users and SSH keys | user sets | `ump/users/*.yaml` | UMP at install time; the Ansible `users` role afterwards — the same file |
| Everything above the base OS | Ansible | `ansible.cfg`, `inventory/`, `playbooks/`, `roles/` | machines |
| Procedures | runbooks | `docs/runbooks/` and the skill's references | humans and agents |

The seams between the owners are **generated, never hand-copied**:

```
terraform output -json ──▶ ump machine import --from terraform    names, labels and addresses of
                                                                   machines Terraform created
ump server reinstall … --group <role> ──▶ inventory/hosts/<host>.yml  the host in common and its groups,
                                                                   with address, administrator, facts
                                         (or the same in a storage, through inventory/hosts.yml;
                                          or ump server list -o ansible, through inventory/ump.yml)
ump provision [--hosts <pattern>]    ──▶ ansible-playbook playbooks/*.yml on the fleet, each play on
                                         the hosts its hosts: names, each host as its administrator
```

The join key across all three tools is the **machine name**,
`<env>-<role>-<nn>` (`prod-web-03`): the Terraform resource name, the name
at the provider, the UMP hostname and the Ansible inventory hostname are
the same string. Providers without labels (Hetzner Robot) carry the
environment and role in the name; the overlay can say the rest.

### The machine overlay

Providers cannot carry everything, and some machines need one-off
settings. `ump/machines.yaml` holds per-machine intent next to the
profiles:

```yaml
defaults:
  profile: base

match:                        # rules for machines not listed explicitly
  - name: "^prod-web-"
    profile: web
    labels: { env: prod, role: web }
  - name: "^staging-"
    labels: { env: staging }

machines:
  prod-db-01:
    profile: database
    labels: { env: prod, role: db }
    overrides:
      layout: nvme-raid10-lvm
      network:
        extra_ipv4: [203.0.113.40/32]
```

This also answers the earlier open question about machine-level
overrides: they live here, in git, reviewed like everything else.

## The reference repository

What "organized" means. New repositories start with `ump repo init`;
existing ones are moved toward it in steps (see
[Taking over an existing repository](#taking-over-an-existing-repository)).

```
infra/
├── README.md                     what runs where, how to operate it (kept current by the skill)
├── Taskfile.yml                  the handful of entry points humans and agents use:
│                                 task plan / apply / check / install -- <machine>
├── mise.toml                     pinned versions of terraform, ansible, ump, sops
├── .claude/skills/ump/           the skill, pinned to the ump version (or installed per user)
│
├── terraform/
│   ├── modules/                  reusable: hcloud-server, dns-record, firewall, …
│   └── envs/
│       ├── prod/                 one root per environment, remote state with locking
│       └── staging/
│
├── ump/
│   ├── config.yaml               accounts — secret references only
│   ├── machines.yaml             the machine overlay
│   ├── profiles/  layouts/  users/  images/
│
├── ansible.cfg                   at the root: ump provisions from here
├── inventory/
│   ├── groups.yml                the group tree: env above role
│   ├── group_vars/               by env, role, provider, location
│   ├── host_vars/                rare; most per-host facts come from the host files
│   └── hosts/                    written by ump: one file per machine (or hosts.yml: in a storage; ump.yml: ump is the inventory)
├── playbooks/
│   └── NN-<role>.yml             every one runs, in order, on the hosts it names; 00-base imports ofzhur.ansible.base
├── roles/                        one role per concern; no logic in playbooks
├── collections/                  ofzhur.ansible as a submodule; others pinned (requirements.yml)
│
├── secrets/                      sops-encrypted files, or nothing: references to pass / vault / op
├── docs/
│   ├── runbooks/                 human- and agent-readable procedures specific to this infra
│   └── decisions/                ADRs: why things are the way they are
└── .github/workflows/            PR: terraform plan, ansible --check --diff, ump validate
                                  merge: terraform apply (environment-gated)
```

Conventions the skill enforces and, during adoption, introduces:

- **One owner per fact** (table above). Duplicated data is a bug: an IP
  address in `host_vars` that also exists in Terraform state is removed in
  favour of the generated inventory.
- **Naming**: `<env>-<role>-<nn>` for machines; the same role name in the
  Terraform module call, the overlay, the Ansible group and the role
  directory.
- **Everything pinned**: Terraform providers and modules, Ansible
  collections, images by checksum, tool versions in `mise.toml`, the skill
  to the `ump` version.
- **Secrets never in plaintext in git**: sops-encrypted files or external
  managers; UMP secret references; Ansible Vault or sops for Ansible. The
  audit greps for the usual accidents, in history as well as the tree.
- **Plan/check mode for every change**: `terraform plan`,
  `ansible-playbook --check --diff`, `ump plan`, `ump profile validate`.
  Roles that cannot run in check mode are fixed or marked, because an
  unreviewable change is an unmergeable one.
- **Changes travel through git**: branch, PR with the plan output
  attached, CI re-runs the plan, apply after merge (Terraform,
  environment-gated) or after an explicit human approval (UMP, Ansible
  against production).
- **Machines are rebuilt, not nursed**: an OS version change is a new image
  plus `ump server reinstall`, one machine at a time, canary first; Ansible owns
  configuration, not OS upgrades. Data-carrying machines document their
  rebuild procedure in a runbook.
- **Environments are separate roots**, separate inventory groups and, where
  credentials differ, separate UMP accounts.
- **Runbooks are code-adjacent**: a procedure that is run more than twice
  lives in `docs/runbooks/` and is referenced from the skill; the agent
  updates it when the procedure changes.

## Workflows

Each workflow is a file under `references/runbooks/`, written as a
sequence of tool steps with explicit human gates. The agent follows it; it
does not reinvent the order.

| Workflow | Sequence | Human gates |
|---|---|---|
| **Add a cloud machine** | Terraform module call → PR with plan → merge, apply → `ump machine import` → `ump plan` → approve → `ump server reinstall` → handoff to Ansible → verify → update README and runbooks | PR review; `ump approve` |
| **Add a bare-metal machine** | ordered at the provider by a human → appears in `ump server list` → rename to convention (`ump server rename`) → overlay entry → `ump plan` → approve → install → Ansible → verify | ordering; `ump approve` |
| **Reinstall / rebuild** | drain (Ansible: remove from LB, stop writers) → `ump plan` → approve → `ump server reinstall` → Ansible → undrain → verify | `ump approve` |
| **OS upgrade across a fleet** | new image in `ump/images/` → bump profile → PR → canary rebuild → soak → remaining machines one at a time | PR; one approval per machine, or one for a named batch |
| **Change a disk layout** | new layout → data migration runbook → rebuild as above | PR; approval |
| **Add or rotate users and keys** | edit `ump/users/*.yaml` → PR → Ansible `users` role on existing machines (`--check` first) → new installs pick it up automatically | PR; production run approval |
| **Decommission** | Ansible: drain, remove from monitoring and DNS → Terraform destroy (cloud) or contract cancellation at the provider (bare metal, human only) → remove overlay entry → verify inventory | PR; a destroy apply; cancellation |
| **Drift check** (weekly) | `terraform plan` in every root (expect no changes) → `ump server list --diff` (added or removed since last time) → `ump inventory check` → `ansible-playbook --check --diff` → report | none; findings become PRs or issues |
| **Health report** | reachability, machines in no inventory, contracts cancelled or expiring (`paid_until`), traffic, failed runs, expired approvals, pin freshness | none |
| **Host down** | `ump machine show`, provider status and console, `ump ssh` diagnostics → propose: hard reset, rescue and inspect, or rebuild → execute the approved option → post-mortem note | every action beyond reading |
| **Dependency bumps** | providers, collections, images, tools → one PR per bump with plan output | PR review |

Worked example, adding `prod-web-03`:

```console
agent  $ task plan -- prod                      # terraform plan: +1 hcloud_server "prod-web-03"
agent  → opens PR "add prod-web-03" with the plan attached
human  → reviews, merges; CI applies

agent  $ terraform -chdir=terraform/envs/prod output -json | ump machine import --from terraform
agent  $ ump plan prod-web-03 --profile web -o json       # also prints the human summary
agent  → shows the plan: disks, layout, hostname, network, users, handoff
human  $ ump approve run-7f3a                             # interactive, TTY only
agent  $ ump install prod-web-03 --group web --approved run-7f3a   # install, reboot, verify, then
                                                          # the host files and the playbooks on the fleet
agent  $ ansible prod-web-03 -m ping && task check -- prod-web-03
agent  → reports: debian-13, mirrored-lvm, role web applied, 14 min; fleet table in README updated
```

## Taking over an existing repository

The adoption path for a repository that has grown organically — many
roles, several ways of creating machines, inventory by hand, scripts of
unknown provenance. The rule is **incremental and reversible**: every step
is one PR that is independently mergeable and leaves the infrastructure
working.

| Phase | Goal | Typical PRs | Done when |
|---|---|---|---|
| **0 — Audit** | Understand without changing | `ump repo audit` report plus the agent's reading → `docs/repo-health.md` and a migration plan as an ADR | the owner agrees with the plan |
| **1 — Safety** | Remove the ways to lose data or secrets | secrets out of git (sops or references), remote state with locking, pinned providers and collections, CI that runs plan and check | `terraform plan` and `--check` are clean and run in CI |
| **2 — Structure** | Move to the reference layout | pure moves with `moved` blocks (plan shows no changes), one playbook per group, roles deduplicated, playbook logic pushed into roles, hand-written inventory replaced by the host files ump writes | layout matches; no behaviour change in any PR |
| **3 — Provisioning** | Replace bespoke install paths with UMP | users and disk layouts extracted from scripts and roles into `ump/`; profiles per role; cloud-init snowflakes and install scripts retired; first machines rebuilt with UMP | every new machine goes through `ump server reinstall` |
| **4 — Operation** | Routine work runs through the skill | runbooks written from what was learned, weekly drift and health reports, dependency bumps as PRs | the human mostly reviews and approves |

Rules during adoption:

- Never reorganize and change behaviour in the same PR.
- A move is proven by an unchanged plan; a role refactor by an unchanged
  `--check --diff`.
- Dead code is removed, not moved: roles no playbook references, variables
  nothing reads, scripts no runbook mentions. The audit lists them with
  evidence.
- Everything the audit found but did not fix becomes an issue with the
  file and line.

What the audit looks at (the deterministic part is `ump repo audit`, the
judgement is the agent's):

- structure: how machines get created today (Terraform? console? scripts?),
  how inventory is produced, how many entry points exist
- Terraform: state backend and locking, provider and module pins, drift
  (`plan` not clean), resources imported by hand, hardcoded IPs and
  credentials, missing `moved` history
- Ansible: roles without a consumer, duplicated tasks across roles, tasks
  that break check mode, `shell`/`command` where a module exists, unpinned
  collections, vault usage, `host_vars` that duplicate another owner's
  facts
- secrets: plaintext patterns in history as well as the working tree
- OS provisioning: install scripts, cloud-init user-data, images in use —
  the inputs to future UMP profiles
- documentation: runbooks that exist, procedures that only exist in
  someone's head (asked, not guessed)

## Safety model

The skill assumes the agent is capable and fast, and therefore dangerous
when wrong. Three tiers:

| Tier | Examples | The agent may |
|---|---|---|
| **Observe** | `ump server list`, `ump machine show/facts`, `terraform plan`, `ansible --check --diff`, reading state, `ump ssh` with diagnostic commands | act freely and repeatedly |
| **Change** | editing repository files, `terraform apply` of an additive plan, `ansible-playbook` against staging or an additive role, `ump server rename` | act after showing the plan or diff, through a PR or an explicit go from the human |
| **Destroy** | `ump server reinstall`, `terraform apply` with destroys, Ansible against production data paths, power actions, DNS changes in production, contract cancellation | act only with an approval it could not have produced itself |

How the last line is enforced rather than hoped for:

- **Approval is a separate command.** `ump plan` writes a plan with a
  hash; `ump approve <run-id>` shows it again and records the approval.
  `ump approve` refuses to run without a TTY, so an agent in a
  non-interactive shell cannot approve its own plan. The approval is bound
  to the plan hash and expires; if facts changed by install time (a disk
  missing, a different machine behind the name), it is void and the plan
  is shown again.
- **UMP never prompts when stdin is not a TTY.** It fails with exit code 3
  and says exactly which approval is missing. Agents get a clear next step
  instead of a hang.
- **Terraform and Ansible get the same treatment** by convention: saved
  plans (`plan -out`, apply the file), `--check --diff` before every real
  run, production applies through CI environments with required reviewers
  or from a human's terminal.
- **Harness permissions** add a second fence: observe-tier commands can be
  pre-approved; change- and destroy-tier commands always surface the full
  command line to the human. `ump skill install --with-permissions` writes
  a suggested allow/ask list for the agent in use.
- **Secrets never enter the agent's context.** Configuration references
  them; `ump` redacts them from all output including `-v`;
  `ump repo audit` reports locations, not values; the skill forbids
  printing secret files.
- **No improvisation on targets.** The agent uses the repository's entry
  points and the three tools. It does not hand-roll `ssh host 'wipefs …'`.
  Diagnostic SSH is read-only by rule; anything that changes a machine goes
  through a runbook and a tier.
- **Blast radius defaults.** Destroy-tier actions run on one machine at a
  time unless the approval names a batch; `--parallel` is for idempotent
  configuration only; production after staging; canary before fleet.
- **Audit trail.** Every agent-initiated change references the PR or the
  run ID; UMP's run log, Terraform's plan file and Ansible's diff output
  are kept; the weekly report lists what the agent did.

## What the skill takes over, what stays with you

Takes over, once the repository follows the conventions:

- writing and editing Terraform within the module conventions; opening PRs
  with plans
- UMP plan, install and verify after approval; Ansible runs; handoff and
  verification
- inventory hygiene: imports, exports, mismatch detection, naming cleanups
- drift detection, weekly health reports, expiring contracts and pins
- dependency and image bumps as reviewable PRs
- documentation: README fleet table, runbooks, ADRs for decisions taken
- the repository audit and the mechanical parts of migration
- first response to incidents: facts gathered, options laid out, the
  approved option executed

Stays with the human:

- approvals for everything in the destroy tier, including every
  `ump server reinstall`
- credentials and access: creating API tokens, granting the agent accounts,
  key custody
- architecture and capacity: what to buy, where, how big, which provider
- anything with a financial or contractual consequence: ordering,
  cancelling, resizing
- the final say in incidents and in post-mortems
- reviewing PRs — the agent writes them to be reviewable, it does not merge
  them

The honest version of "90%": on an organized repository, the human's
share becomes reading plans and typing approvals. On a snowflake
repository, the human's share is the migration decisions, and the agent's
is the audit and the mechanical PRs — until it is organized.

## What this requires from `ump`

Designing the tool for agents costs little and improves it for humans too.
Consequences for the CLI:

- stable `-o json` for every command; `ump plan -o json` carries
  `plan_hash`, the structured plan and a `summary` text ready to paste into
  a PR or a chat
- `ump approve <run-id>` (TTY only, hash-bound, expiring) and
  `ump server reinstall --approved <run-id>`
- no prompts without a TTY; exit code 3 plus a message naming the missing
  approval
- `ump inventory export --format json|hosts --group-by
  env,role,provider,location,profile`, for what is not in the Ansible
  inventory; the machines ump installed are there already, one host file
  each (`ump server reinstall --group`, `ump provision`) or through
  `ump server list -o ansible`
- `ump inventory check` and `ump repo audit` (Markdown and JSON): the
  deterministic halves of the drift check and the adoption audit
- `ump repo init`: the reference repository skeleton and templates, from
  the binary
- `ump machine import --from terraform` reading `terraform output -json`
- the machine overlay `ump/machines.yaml` with `defaults`, `match` and
  `machines`
- structured errors: `error.code`, `error.hint`, and a `troubleshooting.md`
  keyed by code
- `ump doctor`: tools, versions, credentials reachable (without printing
  them), skill ↔ CLI version match
- `ump skill install [--user|--project] [--with-permissions]` writing the
  embedded skill, `ump skill path`
- redaction of secrets in every output stream, including verbose and debug
- run logs that are agent-readable: `ump run show -o json`, `ump run logs`
  with step boundaries

## Open questions

- **One skill or two?** Organizing (audit, migration) and operating
  (runbooks) have different triggers. One skill with progressive disclosure
  keeps a single install; two skills trigger more precisely. Start with
  one, split if the description gets muddy.
- **Approval without a terminal.** Teams that only work through CI need an
  approval path that is not a TTY: a signed PR comment, a GitHub
  environment approval, or `ump approve --from <url>` verifying a
  reviewer's identity. Design it when the first such team appears.
- **MCP server.** `ump mcp` would give agents typed tools instead of shell
  plus JSON. The CLI-first approach works with every agent today and keeps
  one code path; MCP can wrap the same commands later.
- **Ansible consuming UMP's user sets directly** (a lookup of
  `ump/users/*.yaml`) versus `ump inventory export` emitting user data as
  group vars. The first keeps one file; the second keeps Ansible unaware of
  UMP's formats.
- **Where machines not under Terraform are declared.** Bare metal is
  ordered by hand; the overlay plus the provider listing describes it. Is
  an explicit "expected machines" list needed to detect machines that
  *should* exist but do not?
- **How much Terraform and Ansible knowledge belongs in the skill** versus
  in general-purpose skills for those tools. The UMP skill should carry the
  integration and the conventions, not a Terraform tutorial.
