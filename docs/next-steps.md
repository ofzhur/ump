# Next steps: building your infrastructure

> **Draft.** Recommendations for what comes after the first
> installations, from the experience ump grew out of. Parts of it (the
> `provision` step) describe what ump will do; what works today is said
> so.

ump's job ends when a machine is installed and answers: the right
system on the right disks, users and keys, network, a firewall, and the
userdata that had to run before the first boot. From there the machine
belongs to the tools that keep it configured and the pipeline that
deploys onto it. This page is about that hand-over: where the line runs,
how to lay out the repository that holds the rest, pull and push with
Ansible, the `provision` section of a profile, security, and the
configurations that go beyond the published defaults.

## Where ump stops

| Layer | Owned by | Changes |
|---|---|---|
| Machines, accounts, prices, attributes, what was installed | ump: the [inventory](inventory.md) | when machines come and go |
| Operating system, disks, users and keys, network, the firewall's baseline | ump: the [profile](profiles.md) | when a machine is (re)installed |
| What must be there at the first boot: packages, an agent, an enrollment | [userdata](userdata.md) | with the profile |
| Everything that changes while the machine lives: services, their configuration, accounts after day one, certificates | configuration management (Ansible, Salt, Puppet; NixOS itself) | every day |
| Applications | your deployment pipeline | many times a day |

Two rules keep the layers apart:

- **Before the first boot, or after?** What has to be right before the
  machine first starts (the disks, who may log in, what the firewall
  lets in, the agent that enrolls it) goes into the profile and its
  userdata. Everything else goes into configuration management, which
  can change it again tomorrow.
- **Reinstall rather than repair.** A profile plus a repository
  rebuilds any machine in minutes (`ump server reinstall`), so a machine
  that drifted or broke is reinstalled and converged again, not nursed.
  This only works when nothing important lives only on the machine:
  data on separate volumes or backed up, configuration in the
  repository.

## An infrastructure repository

One repository holds what humans review; ump's state backend holds what
machines and tokens need. Keep them apart: the [state](state.md) has the
provider credentials (as references), UMP's private key and the
installation records, and belongs in a backend with access control, not
in git.

```
infra/
├── README.md                 what is where, how to roll out, who to ask
├── ump/
│   ├── profiles/
│   │   ├── web.yaml          one self-contained profile per role: inline firewall,
│   │   ├── db.yaml           layout and users, or names from ump's published sets
│   │   └── base.yaml
│   └── userdata/             the userdata scripts the profiles inline (reviewed here)
├── ansible/
│   ├── ansible.cfg
│   ├── inventory/
│   │   └── ump.sh            the dynamic inventory: ump server list -o json, grouped
│   ├── group_vars/
│   │   ├── all.yml
│   │   ├── role_web.yml
│   │   └── env_prod.yml
│   ├── host_vars/
│   ├── roles/                base, hardening, monitoring, web, db, ...
│   ├── site.yml              push: the whole fleet, from your machine or CI
│   ├── local.yml             pull: what a machine applies to itself
│   └── requirements.yml      collections and roles from Galaxy, pinned
├── terraform/                optional: DNS, load balancers, object storage,
│                             what ump does not do
├── secrets/                  encrypted (sops or ansible-vault), never plain
└── .github/workflows/        lint, ansible --check, the drift report
```

Profiles are used from the repository by path, so a change to a role's
system goes through review like any other:

```console
$ ump server reinstall web-03 --profile ump/profiles/web.yaml
```

The grouping comes from attributes that ump keeps in its state, set once
per machine:

```console
$ ump server set web-03 --attr role=web --attr env=prod
```

### The inventory from ump

ump already knows every machine of every account; the inventory is a
view of it, not a second list to keep in step. This script groups the
servers by the attributes `role` and `env`, by account and by the
operating system ump installed, and hands Ansible the address, the
administrator and ump's facts:

```bash
#!/usr/bin/env bash
# ansible/inventory/ump.sh: the fleet as ump lists it, as an Ansible inventory
set -euo pipefail
[ "${1:-}" = --host ] && { echo '{}'; exit 0; }
ump server list -o json | jq '
  def group: gsub("[^A-Za-z0-9_]"; "_");
  reduce (.[] | select(.ipv4 != null)) as $m ({_meta: {hostvars: {}}};
    ._meta.hostvars[$m.name] = {
      ansible_host: $m.ipv4, ansible_user: "ump",
      ump_ref: "\($m.provider)/\($m.id)", ump_account: $m.account,
      ump_os: $m.os, ump_location: $m.location, ump_attributes: ($m.attributes // {})}
    | reduce (["role_\($m.attributes.role // "none")", "env_\($m.attributes.env // "none")",
               "account_\($m.account)", "os_\($m.os // "unknown")"][] | group) as $g
        (.; .[$g].hosts += [$m.name]))'
```

```console
$ ansible-inventory -i ansible/inventory/ump.sh --graph
$ ansible -i ansible/inventory/ump.sh role_web -m ping
```

The listing is [cached](cli.md#the-listing-cache-and---diff) for five
minutes, so running the inventory often costs no API calls. Ansible logs
in as the administrator (`ump`, with sudo and no password) with your own
key, which the published user carries (`keys: [ump, local]`); in CI,
give the pipeline its own key as a [named key](keys.md) in the users of
your profiles, rather than UMP's.

## Pull or push

Configuration management reaches machines in one of two ways, and a
fleet often uses both.

| | Push | Pull |
|---|---|---|
| How | `ansible-playbook site.yml` from your machine or CI, over SSH | every machine runs `ansible-pull` on a timer against the repository |
| Good at | changes rolled out in order (`serial`), orchestration across machines, seeing the result at once | large fleets, machines CI cannot reach, converging back by itself after drift |
| Needs | SSH from CI to every machine, a key in CI | read access to the repository from every machine, monitoring to see failures |
| Fails | loudly, in the pipeline | quietly, on the machine, unless reported |

A common split: **pull** for the baseline every machine needs (users
added later, hardening, monitoring agent, package updates), applied
every half hour, so that a machine converges back to the baseline by
itself; **push** for changes to services, rolled out on purpose and
watched.

### Pull from the first boot, today

The `provision` step below will set this up by itself; until then, a
userdata entry does it. It installs Ansible, writes a service and a
timer, and enables them; the first run happens at the first boot:

```yaml
# in userdata.yaml (ump config userdata create ansible-pull), listed in the profile after ubuntu
ansible-pull:
  description: converge with the repository every 30 minutes
  type: bash
  script: |
    #!/bin/bash
    set -euo pipefail
    apt-get install -y --no-install-recommends ansible-core git      # Arch: pacman -S --noconfirm --needed ansible-core git
    cat > /etc/systemd/system/ansible-pull.service <<'UNIT'
    [Unit]
    Description=Converge with the infrastructure repository
    Wants=network-online.target
    After=network-online.target

    [Service]
    Type=oneshot
    ExecStart=/usr/bin/ansible-pull -U https://git.example.com/infra.git -C main \
      -d /var/lib/ansible-pull -i localhost, ansible/local.yml
    UNIT
    cat > /etc/systemd/system/ansible-pull.timer <<'UNIT'
    [Timer]
    OnBootSec=2min
    OnUnitActiveSec=30min
    RandomizedDelaySec=5min

    [Install]
    WantedBy=timers.target
    UNIT
    systemctl enable ansible-pull.timer
```

`local.yml` runs against `localhost` and picks its roles from the facts
the machine has, for example the record ump left in `/etc/ump/install`
(profile, OS, layout, interfaces) or the role set as an attribute. A
private repository needs a credential on every machine: a read-only
deploy token fetched at the first boot from your secret store, not one
written into the userdata, which ends up in the profile, in git and in
`/etc/ump/userdata` on the machine.

### Push

```console
$ ansible-playbook -i ansible/inventory/ump.sh ansible/site.yml --limit role_web --check --diff
$ ansible-playbook -i ansible/inventory/ump.sh ansible/site.yml --limit role_web
```

A newly installed machine is part of the next push by itself, since the
inventory comes from ump; `ump server reinstall` followed by a push of
that one host (`--limit web-03`) brings a rebuilt machine back to its
role.

## The provision section

A profile names the step that hands a machine over after its first boot:

```yaml
provision:
  type: ansible-pull          # none | ansible-pull | ansible-bootstrap
  repo: https://git.example.com/infra.git
  branch: main
  playbook: ansible/local.yml
```

**Today** ump checks the type, keeps the section, and records it on the
installed machine in `/etc/ump/install` (`provision=ansible-pull`,
`provision_repo=...`, `provision_branch=...`); `ump server reinstall`
says that it is not run yet. Your own tooling can read it from there: a
playbook, a first-boot unit from the userdata above, or the inventory.

**Planned** (see the [roadmap](roadmap.md)):

- `ansible-pull`: the machine runs the playbook from the repository
  itself after its first boot, and a timer keeps it doing so, as the
  userdata above does by hand;
- `ansible-bootstrap`: ump runs the playbook against the new machine
  from where ump runs, once it answers, and reports the result with the
  installation;
- a plain `script` step for anything else.

Until then, the userdata entry and a push after the reinstall cover both.

## Security profiles

Security is layered, and each layer has its place in the table at the
top. A starting point:

**In the profile (ump), from the first boot:**

- **Firewall per role:** `default` (ICMP and SSH) for everything, `web`
  for the public web, and your own for the rest, with sources:
  ```yaml
  firewall:
    policy: drop
    allow: [icmp, ssh, {proto: tcp, port: 5432, from: 10.0.0.0/8}, {proto: tcp, port: 9100, from: 203.0.113.5/32}]
  ```
  Restrict SSH to your networks or a bastion once you have one
  (`{proto: tcp, port: 22, from: ...}`). The same rules hold for the
  ports containers publish ([firewalls](firewalls.md)).
- **People, not shared accounts:** one [user](users.md) per person with
  their [named keys](keys.md) and sudo where needed, next to the
  administrator `ump`; no root login (the default); no passwords at all.
- **Kernel:** the CPU vulnerability mitigations are off by default, for
  performance. Turn them on (`kernel.mitigations: true`) on machines that
  run code you do not trust, share hardware with others (most cloud
  instances), or carry other people's data.
- **Rescue access:** choose your keys for rescue systems (`ump config key
  rescue`), so that you are not left with UMP's key alone ([keys in
  rescue systems](keys.md#keys-in-rescue-systems)).

**Around ump:**

- **Provider tokens** with the narrowest scopes each provider allows
  ([ISPs](isps.md)), as secret references (`command: pass show ...`)
  rather than files where you can.
- **The state backend** holds UMP's private key and the credentials'
  references: give it the access control of your secret store, and give
  CI its own token.
- **UMP's own key** opens every machine as the administrator; treat it
  like a root key, and rotate it (a new `staging.key`) when someone who
  could read it leaves.

**In configuration management, afterwards:**

- automatic security updates (`unattended-upgrades` on Ubuntu and
  Debian, a scheduled `pacman -Syu` with a reboot window on Arch);
- `auditd`, an intrusion prevention (`fail2ban` or CrowdSec), a CIS
  baseline where required (the `ansible-lockdown` roles);
- monitoring agents reachable only from the monitoring network (the
  firewall's `from`);
- certificates and secrets delivered at run time, never baked into
  profiles or userdata.

Disk encryption (LUKS with remote unlock) is on the
[roadmap](roadmap.md).

## Advanced configurations

- **Profiles per role:** `ump config os create db --from ubuntu-26.04`,
  then the layout (`lvm-raid1` on two disks for a database, `lvm-striped`
  for scratch space), the firewall, the users and the userdata of that
  role. Keep them in the repository and use them by path.
- **Disk layouts of your own:** md arrays and LVM with mirrored or
  striped volumes, matched to whatever disks a machine has
  ([disk layouts](disk-layouts.md)).
- **Mixed fleets:** a profile per OS; userdata is bash on Ubuntu, Debian
  and Arch Linux and a NixOS module on NixOS. On NixOS the configuration
  is the system itself: keep the modules in the repository and deploy
  them with `nixos-rebuild --target-host`, `deploy-rs` or `colmena`
  instead of Ansible.
- **Teams:** a shared [state backend](state.md), per-server locks for
  reinstall and rescue, everyone acting as one UMP.
- **Drift and cost reports:** `ump server list --diff --since 1d` in a
  scheduled job shows what appeared, vanished or changed at the
  providers; `-o json` with the prices and attributes feeds cost reports
  per role or customer ([inventory](inventory.md)).
- **Everything else the providers offer** (DNS, load balancers,
  networks, object storage): Terraform or the providers' tools, with the
  addresses ump lists as inputs.

## Getting help

UMP is built and maintained by an independent DevOps engineer. If you
would rather not build all of this alone, help is available on a
commercial basis:

- designing the infrastructure repository and the hand-over from ump to
  your configuration management;
- migrating an existing fleet to profiles, layouts and reinstallable
  machines;
- Ansible roles, hardening and security baselines, monitoring;
- support for more providers, operating systems or setups in ump itself;
- operating a fleet, or training your team to.

Inquiries are welcome:

<!-- TODO: contact details before this page goes public -->
> **Contact:** _to be added_
