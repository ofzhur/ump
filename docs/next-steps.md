# Next steps: building your infrastructure

> **Draft.** Recommendations for what comes after the first
> installations, from the experience ump grew out of.

ump's job ends when a machine is installed and answers: the right
system on the right disks, users and keys, network, a firewall, and the
userdata that had to run before the first boot. From there the machine
belongs to the tools that keep it configured and the pipeline that
deploys onto it. This page is about that hand-over: where the line runs,
how to lay out the repository that holds the rest, security, and the
configurations that go beyond the published defaults. Applying roles to
a server is [provisioning](provisioning.md).

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

**Recommended: keep the state centralized.** With the state in one
[backend](state.md) rather than in each laptop's `~/.config/ump`, ump
can run from several places at once: people, CI jobs and scheduled
reports work against the same servers in parallel. Each reinstall
and rescue takes a lock on its own server only (provisioning runs will
too), so runs on different servers go ahead side by side and two runs
on the same server cannot collide. With a local state each place has its own
view and nothing stops two of them from reinstalling the same machine.

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

The servers get their roles when they are installed, and attributes of
your own for the rest of the grouping:

```console
$ ump server reinstall web-03 --profile ump/profiles/web.yaml --role web
$ ump server set web-03 --attr env=prod
```

The grouping comes from the roles ump records with each server (`ump
server reinstall --role`, `ump server provision`) and from attributes you
set; [provisioning](provisioning.md) has the inventory script and the
strategies that apply the roles.

## Provisioning

Turning an installed system into what the server is for (its roles) is
[provisioning](provisioning.md): the roles, the commands, push and pull
with Ansible (`ansible-bootstrap`, `ansible-pull`), a control plane, and
what works by hand until the strategies run.

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
- **Mixed fleets:** a profile per OS; userdata is bash on Ubuntu, Debian,
  Arch Linux and CachyOS and a NixOS module on NixOS. On NixOS the configuration
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

Inquiries are welcome through the contact form at
[zhurko.de/contact](https://zhurko.de/contact/).
