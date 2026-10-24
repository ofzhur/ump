# Provisioning

An installation by ump ends with the operating system ready to use: the
disks, the users and their keys, the network, the firewall, and the
[userdata](userdata.md) of the profile, which ran before the first boot.
Provisioning is the next step: it turns that system into what the server
is for (a web server, a database, a monitoring node). ump does it with
Ansible from your infrastructure repository: it puts every server it
installs into the repository's inventory, in **groups**, and runs the
repository's playbooks over the fleet, each play on the hosts its
`hosts:` names. A reinstall ends with that run; `ump provision` runs it
whenever you want the fleet converged; `ump server multiinstall`
installs several servers side by side and ends in one run for all of
them. Servers can also [pull](#servers-that-pull) the repository
themselves.

```console
$ cd ~/src/infra                                        # the infrastructure repository
$ ump server reinstall web-01 --group web                # install, then provision the fleet
$ ump server reinstall db-01 --group db --hosts db-01   # install, then provision db-01 alone
$ ump server multiinstall --plan fleet.yaml             # several at once, then one run
$ ump server set web-01 --group monitoring              # a group more; the next run applies it
$ ump provision                                         # converge the fleet
$ ump provision --hosts web --playbook 10-web           # some hosts, some playbooks
$ ump provision --hosts web-01 -- --check --diff        # what Ansible would change
$ ump server reinstall lab-01 --no-provision            # install only
```

## Installation and provisioning

| | Installation | Provisioning |
|---|---|---|
| What | the OS, disks, users, network, firewall, userdata | what the server's groups say: users after day one, services, their configuration |
| Defined by | the [profile](profiles.md) | the playbooks of your repository, which use the collection [ofzhur.ansible](https://github.com/ofzhur/ansible) |
| When | before the first boot | after the first boot, and with every `ump provision` or pull |
| Run by | `ump server reinstall`, `multiinstall` | `ump server reinstall`, `multiinstall`, `ump provision`, `ansible-playbook` from the repository, or `ansible-pull` on the server |

Every `ump server reinstall` provisions, unless `--no-provision`: the
server is then the operating system plus the userdata, nothing more,
until the next `ump provision`, which includes it (below).

Every server ump installs also gets an address in each [private
network](networks.md) of its profile (the published profiles name
`default`, `10.0.0.0/24`; a profile without `network.private` gives
none), or of `--private-network`: the lowest free one, or the one
`--private-ip` gives, kept across reinstalls and never another
server's. The installation records them; the host files carry them as
`ump_private_networks`, with `ump_private_address`,
`ump_private_network` and `ump_private_cidr` of the first network
([host files](#host-files)), for the roles that connect the servers,
such as a mesh per network. Nothing configures an interface with them
before those roles do.

## Groups and playbooks

Every server ump installs is in the group `common`. `--group` adds
groups (repeatable, or comma-separated); a group name is an Ansible
group name: lower-case letters, digits and `_`, not starting with a
digit; `all`, `ungrouped`, `localhost` and `_meta` are Ansible's own,
`common` is implied. Nor can the hostname be `localhost` or
`127.0.0.1`: plays and tasks for `localhost` would run on the server.

Every `playbooks/*.yml` of the repository runs, in file-name order, on
the hosts its plays name: `hosts: all`, `hosts: web`, `hosts:
web:&prod`. A group needs no playbook, and a playbook picks its hosts;
number the files to order them. The collection's playbooks come in
through a file of one line:

```yaml
# playbooks/00-base.yml
- import_playbook: ofzhur.ansible.base
```

```yaml
# playbooks/10-web.yml
- hosts: web
  become: true
  roles: [nginx]
```

Only `*.yml` directly in `playbooks/` runs, files or links to files: dot
files, other names and subdirectories do not, so tasks files and the
like go into subdirectories. `--playbook NAME` (the file name without
`.yml`; repeatable, or comma-separated) runs those alone, in file-name
order still.

ump runs them as one playbook that imports them, as a `site.yml` does
(a dot file it writes into `playbooks/` for the run): a host that
fails or is unreachable is left out of the plays after, and the others
go on. Given one by one, `ansible-playbook` would stop at the first
file with such a host, for every host. A play whose every host failed
ends the run, as Ansible does.

**Groups are best treated as immutable.** A server gets its groups when
it is installed, and a server that should become something else is
reinstalled with the new ones: every group starts from a fresh system,
and a server's state follows from its profile and groups alone.
`ump server set NAME --group GROUP` adds a group and `--ungroup GROUP`
takes one away, which suits groups built for it, such as monitoring or
backup agents. Nothing runs then: the next `ump provision` puts the
server into its groups and runs their plays on it; what the plays of a
group it left did stays until a reinstall.

ump records the groups in its [state](state.md), without `common`: a
successful `ump server reinstall` sets them (none without `--group`;
with `--no-reboot` too), `ump server set` changes them; a dry run or a
failed installation changes nothing. The listings show them as in the
inventory, `common` first: the `GROUPS` column of `ump server list`,
`groups` in its `-o json` and in `ump server show`; a server ump did
not install has none.

A server that Ansible must not touch (NixOS, a machine configured by
hand) is in `common` all the same: give it a group of its own (`--group
nixos`) and leave that group out, with `hosts: all:!nixos` in the plays
or `--hosts 'all:!nixos'`. The collection's `base` runs on `common`,
every server ump installs; a repository with such servers writes its
own `00-base.yml`, a play on `common:!nixos` with the role
`ofzhur.ansible.users`.

## The infrastructure repository

ump runs from the root of the repository. The layout it expects:

```
infra/
├── ansible.cfg                the collection's template, config/push/ansible.cfg
├── inventory/
│   ├── groups.yml             the static group tree; keeps the inventory valid before the first server
│   ├── group_vars/            common.yml, web.yml, ...
│   ├── host_vars/             overrides per host
│   ├── hosts/                 written by ump: one file per server
│   ├── hosts.yml              or this instead: the host files in a storage (below)
│   └── ump.yml                or this: ump as the inventory (below)
├── playbooks/                 00-base.yml, 10-web.yml, ...: every one runs, in this order
└── collections/ansible_collections/ofzhur/ansible    the collection, a git submodule
```

Setting it up:

```sh
git submodule add -b main https://github.com/ofzhur/ansible.git collections/ansible_collections/ofzhur/ansible
cp collections/ansible_collections/ofzhur/ansible/config/push/ansible.cfg .
mkdir -p inventory playbooks
printf 'all:\n  children:\n    common:\n' > inventory/groups.yml
printf -- '- import_playbook: ofzhur.ansible.base\n' > playbooks/00-base.yml
printf '.cache/\nplaybooks/.ump-*\n' > .gitignore
```

Ansible reads an empty inventory directory as a broken inventory, hence
`groups.yml`. The template reads collections from the repository only,
so a playbook uses `ansible.builtin` and what is installed under
`collections/`; its fact cache is `.cache/`, which stays out of git, as
does what ump writes into `playbooks/` for a run.

Before `ump server reinstall` or `ump provision` touches anything (for
a reinstall: before the question, the rescue system and the server's
lock), ump checks:

1. **The layout:** `ansible.cfg`, `inventory/` and a playbook in
   `playbooks/` in the current directory, `ansible-playbook` in `PATH`
   (from `ansible-core`). Without them it stops with exit status 2 and
   names what is missing; without a playbook, it says to create
   `playbooks/00-base.yml`. `--no-provision` needs none of them. A
   world-writable directory stops it too: Ansible ignores an
   `ansible.cfg` there, and ump, which names it in `ANSIBLE_CONFIG`,
   does not take it either. So does a pointer to host files in a
   storage beside another form of the inventory
   ([below](#the-inventory-three-forms)).
2. **The playbooks:** `ansible-playbook --syntax-check` on those of the
   run, which also reads the inventory. A playbook that does not parse
   or a broken inventory stop it with exit status 2 and Ansible's
   output; so does a `--playbook` that is not there.
3. **The names:** a host and a group of the same name make the group's
   plays match only the host (Ansible resolves a pattern that names a
   host as that host). A hostname that is a group of the server, of
   another server ump installed (its groups in the state) or of the
   inventory (`ansible-inventory --list`), or a group of the server that
   is another host of the inventory, stops it with exit status 2; so
   does the hostname `localhost` or `127.0.0.1`. For a group a server
   has in the state, the message says how to take it away: `ump server
   set NAME --ungroup GROUP`.
4. **The host files:** `inventory/hosts/HOSTNAME.yml` or `HOSTNAME.yaml`
   (or the object of that name in the storage) that ump did not write
   for that server stops it with exit status 2: one without `ump_ref`
   (written by hand), or one of another server. When ump is the
   inventory, any host file of the hostname or of the server stops it
   (the host would be defined twice), and so does the hostname as
   another host of the inventory. A storage that cannot be opened (its
   URL, its credentials) is exit status 2 as well, one that does not
   answer exit status 1.
5. **The hosts:** `--hosts` must match a host of the inventory as it
   will be: the current one plus the hosts ump writes (for a reinstall,
   the server under its hostname in `common` and its `--group` groups).
   `ansible-inventory --limit` matches it as Ansible does, on a copy of
   the group tree in a temporary file outside the repository. Matching
   none is exit status 2.

A reinstall checks the names, the host file of its server and `--hosts`
up front, and again for every server before the run; `ump provision`
checks them for every server ump installed, under the fleet's lock
(below), before it writes a file. `--dry-run` makes the checks, shows
the playbooks with the plan and writes nothing.

## The inventory: three forms

Every server ump installed is in the inventory, one of three ways, with
the same hosts, groups and variables in each:

1. **Host files in the repository**, the default: ump writes one file
   per server into `inventory/hosts/`, read by the collection's
   inventory plugin `ofzhur.ansible.host`. The files are committed with
   the rest, and Ansible runs from the repository without ump.
2. **Host files in a storage**, like the facts: `inventory/hosts.yml`
   points the same plugin to a storage (S3, R2, Consul, Vault, a
   directory over SSH), and ump writes the same files there, one object
   per server, instead of into `inventory/hosts/`. Ansible runs without
   ump, and a new server needs no commit.
3. **ump as the inventory**: `inventory/ump.yml` configures the
   collection's inventory plugin `ofzhur.ansible.ump`, which runs `ump
   server list -o ansible` and takes the servers ump installed from
   there. ump writes no host file.

Which to choose:

- **Host files in the repository** where the repository says what the
  fleet is: a review shows every new server, and Ansible runs wherever
  the repository is, without credentials (CI, a colleague's laptop,
  [servers that pull](#servers-that-pull)). ump writes the files;
  committing them is yours.
- **Host files in a storage** where Ansible runs in several places that
  should see a new server at once, without a commit: control machines
  and servers that pull read the storage with credentials of their own,
  never ump's state. It goes with the [facts in a
  storage](#facts-in-a-central-storage), each in a place of its own
  ([three places](#three-places)).
- **ump as the inventory** where Ansible runs only beside ump and its
  state (your laptop, a bastion, a CI job with the state backend's
  credentials): always current, nothing to write or remove for a
  server. Every Ansible run lists the servers with ump.

One form at a time: ump refuses `inventory/hosts.yml` beside host files
in `inventory/hosts/`, beside `inventory/ump.yml` or beside another
pointer (exit status 2), since Ansible would read them all.

### Host files

Every run of `ump provision`, and the one after a reinstall, writes
the host file of every server ump installed,
`inventory/hosts/HOSTNAME.yml` (or `HOSTNAME.yml` in the storage,
[below](#host-files-in-a-storage)), read by the collection's inventory
plugin `ofzhur.ansible.host`:

```yaml
# web-01: written by ump (ump server reinstall, ump provision).
# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.
plugin: ofzhur.ansible.host
groups: [common, web]
vars:
  ansible_host: 203.0.113.10
  ump_admin: ump
  ump_name: web-01
  ump_ref: hetzner-cloud/234567
  ump_provider: hetzner-cloud
  ump_account: main
  ump_location: fsn1
  ump_product: cx22
  ump_os: ubuntu-26.04
  ump_profile: ubuntu-26.04
  ump_installed_at: "2026-10-14T11:58:30Z"
  ump_private_address: 10.0.0.1
  ump_private_network: default
  ump_private_cidr: 10.0.0.0/24
  ump_private_networks:
    default:
      address: 10.0.0.1
      cidr: 10.0.0.0/24
  ump_attributes:
    owner: alice
```

- The file name is the hostname of the installation (`--hostname`, else
  the server's name). `ansible_host` is the IPv4 address, else the IPv6
  one; `ump_admin` the administrator of the installation (`ump` with
  the published profiles), whom ump's runs log in as; there is no
  `ansible_user`, so that people log in as themselves
  ([below](#logins-umps-and-yours)). `ump_private_networks` are the
  server's addresses in its [private networks](networks.md), by
  network, each with the network's CIDR; `ump_private_address`,
  `ump_private_network` and `ump_private_cidr` are those of its first
  network. `ump_attributes` are your
  [attributes](inventory.md#attributes). Variables without a value
  (`ump_location`, `ump_product`, `ump_private_*` without a private
  network, ...) are left out.
- The files come from the state and the providers' listings as they are
  at the run, under the fleet's lock: a new server, new groups (`ump
  server set`), a new address or attribute. Only a file that changed is
  written, under a dot name, which Ansible ignores, and renamed into
  place. A file of another name for the same server (an old hostname)
  is removed. An account that cannot be listed keeps its servers' files
  as they are. A file of the hostname without `ump_ref` is never
  overwritten: remove it, and keep what it said in
  `inventory/host_vars/HOSTNAME.yml`.
- Values are data, never templates. Overrides go to
  `inventory/host_vars/HOSTNAME.yml`, group variables to
  `inventory/group_vars/`, group trees (`prod` with the children `web`
  and `db`) to `inventory/groups.yml`.
- Commit the files: the inventory is the repository's, and Ansible runs
  from it without ump. ump commits nothing.
- ump does not remove the host file of a server that is gone: `git rm`
  it (in a storage, delete its object), or every run finds it
  unreachable. With the last one, remove `inventory/hosts/` too:
  Ansible fails on an empty directory, and so does ump's syntax check.

A server installed with `--no-provision` from the repository gets its
host file at once, under the fleet's lock: ump installed it, so it
belongs in the inventory. Its names and host file are checked first, as
above, which takes `ansible-inventory` in `PATH`; without it (or in a
world-writable directory), and from anywhere else, the next `ump
provision` from the repository writes it. A host file that cannot be
written then (another's in its way meanwhile, the lock held too long)
ends the reinstall with that exit status. With `--no-reboot` the host
file waits for the next run too, and until the reboot the run finds the
server unreachable.

### Host files in a storage

```yaml
# inventory/hosts.yml
plugin: ofzhur.ansible.host
url: r2://ACCOUNT/fleet-inventory/hosts
```

```ini
# ansible.cfg
[ofzhur_storage]
access_key_file = secrets/storage.access_key
secret_key_file = secrets/storage.secret_key
```

- The URL is one of ump's [state backends](state.md#backends) but
  `http(s)://`: `s3://BUCKET/PREFIX` (S3, or an S3-compatible store at
  `endpoint`), `r2://ACCOUNT/BUCKET/PREFIX`, `consul://HOST:8500/PREFIX`
  (`http+consul://` for plain http), `vault://HOST:8200/MOUNT/PREFIX`
  (`http+vault://`), `ssh://[USER@]HOST[:PORT]/PATH`, `file:///PATH`.
- A file directly in `inventory/` that says `plugin:
  ofzhur.ansible.host` and `url:` is the pointer. ump writes
  `PREFIX/HOSTNAME.yml` for every server, with the content of a file of
  `inventory/hosts/`, by the same rules: only an object that changed,
  as a whole (an object replaced at once, or a file written under a dot
  name and renamed into place); the object of the server's old hostname
  goes; one of the hostname without `ump_ref`, or of another server,
  stops ump up front. Nothing goes into `inventory/hosts/`.
- The plugin reads every object directly under the prefix whose name
  ends in `.yml` or `.yaml` (but for dot names) as a host file; no
  object is an empty inventory. In Vault, an object is the field `json`
  of a secret.
- Next to `url`, the pointer may set the options that are no
  credentials: `endpoint`, `region`, `vault_namespace`, `ca_file`
  (relative to the pointer) and `ssh_args`. Any other key, a credential
  among them, is refused, by the plugin and by ump.
- A place of its own, not that of the facts: the fact cache's flush
  deletes every object under its prefix, and servers that pull write
  the facts but must not write the host files ([three
  places](#three-places)).

#### The storage's credentials

ump reads them as the collection does, so that ump and Ansible reach
the same storage with the same credentials:

- From `[ofzhur_storage]` of the repository's `ansible.cfg`, or from
  the collection's variables, `ANSIBLE_OFZHUR_STORAGE_` and the
  option's name in capitals (`ANSIBLE_OFZHUR_STORAGE_ACCESS_KEY_FILE`),
  which win over it, also when empty. The pointer's options win over
  both.
- Each credential is a value or, better, a file: `access_key_file`,
  `secret_key_file`, `session_token_file` (S3, R2), `consul_token_file`,
  `vault_token_file`. A file wins over a value; an empty file is no
  credential. A relative path is relative to `ansible.cfg` there, and
  to the repository in a variable; `~` and `$VAR` are expanded.
- `ansible.cfg` is read as Ansible reads it: `;` after a blank starts a
  comment, an indented line continues a value, `[DEFAULT]` applies to
  the section. A value in quotes is refused: ansible-core 2.19 and
  later take the quotes off, earlier versions keep them.
- Never `AWS_*`, `VAULT_TOKEN`, `CONSUL_HTTP_TOKEN` or `backend.yaml`:
  those are the credentials of ump's state, and ump hands its
  environment to Ansible. A credential that is missing stops ump with
  exit status 2 and says where it goes.

| URL | Options |
|---|---|
| `s3://` | `access_key`, `secret_key`, `session_token`, `endpoint`, `region` (`us-east-1`) |
| `r2://` | `access_key`, `secret_key` |
| `consul://` | `consul_token` |
| `vault://` | `vault_token` (required), `vault_namespace` |
| `ssh://` | `ssh_args`: options of your ssh client, such as `-i ~/.ssh/fleet` |
| over https | `ca_file`: CA certificates (PEM) in place of the system's |

#### Three places

ump's state, the host files and the facts take a place each (a bucket,
a prefix, a Vault path, a directory), each with credentials of its own:

- **The state** holds UMP's private key and the accounts' secrets:
  control machines alone read and write it.
- **The inventory**, the host files: control machines write it, servers
  that pull read it. Whoever can write it changes what every run does.
- **The facts**: every machine that runs Ansible writes them, a server
  that pulls its own.

On R2, which scopes a token to buckets, not to prefixes, that is three
buckets (a control machine's token for those of the inventory and the
facts, a pulling server's reading the one and writing the other); on
S3, Consul and Vault a prefix each will do, never one under the state.
Give each machine no more than its part; the collection's README lists
the policies per storage.

### ump as the inventory

```yaml
# inventory/ump.yml
plugin: ofzhur.ansible.ump
```

The plugin runs `ump server list -o ansible`: every server ump
installed (the [state](state.md) has its installation), in the JSON of
an inventory script, each under the hostname of its installation, in
`common` and its groups, with the variables of its host file, made by
the same code:

```console
$ ump server list -o ansible
{
  "_meta": {
    "hostvars": {
      "web-01": {
        "ansible_host": "203.0.113.10",
        "ump_admin": "ump",
        "ump_name": "web-01",
        "ump_ref": "hetzner-cloud/234567",
        ...
      }
    }
  },
  "all": {"children": ["common", "web"]},
  "common": {"hosts": ["web-01"]},
  "web": {"hosts": ["web-01"]}
}
```

- A file directly in `inventory/` that says `plugin:
  ofzhur.ansible.ump` makes ump the inventory for `ump server reinstall`
  and `ump provision`: they write no host file and say so (`No host
  files: inventory/ump.yml makes ump the inventory.`); the checks and
  the run stay as they are. A pointer to host files in a storage beside
  it is refused.
- A host file of a server beside it, of its hostname or with its
  `ump_ref`, stops them with exit status 2: the host would be defined
  twice. Switching from host files, `git rm -r inventory/hosts`.
- Every server ump installed is in it, in `common`, from its record
  on: also one installed with `--no-provision` or `--no-reboot`, or from
  outside the repository, and a NixOS one. Keep `ump.yml` to the
  accounts of the servers Ansible configures (below), or give the
  others a group the plays leave out.
- A server gone from its provider's listing is gone from the
  inventory. Of two servers with the same hostname, the later
  installation has it; the other is left out with a note on stderr, and
  so is a server without an address or a valid hostname.
- Every account ump lists must answer: one that fails fails the
  inventory, and with it every Ansible run. `arguments: [--account,
  NAME]` keeps the plugin to an account (repeat the pair for more);
  `cached: true` takes ump's last listing however old, without the
  providers.
- The plugin runs the `ump` in `PATH`; `command:` names another (a
  path). Its other keys are `arguments`, `cached` and `timeout`
  (seconds, 120); any other key fails it.
- Every Ansible run lists the servers with ump, so ump, its
  configuration and its state (with the backend's credentials) must be
  wherever Ansible runs; the [listing
  cache](inventory.md#the-listing) spares the providers most of those
  listings. When ump runs Ansible itself, the plugin's ump is part of
  that run (below).

```yaml
# inventory/ump.yml: the servers of two accounts, as ump last listed them
plugin: ofzhur.ansible.ump
arguments: [--account, main, --account, lab]
cached: true
```

When ump cannot record an installation in the state (the backend does
not answer, say), the inventory would have the old record, from the
state as from the host files that `ump provision` writes: `ump server
reinstall` provisions nothing then and ends with exit status 6. It
prints the record; add it to `state.yaml` by hand and run `ump
provision --hosts HOSTNAME`.

## Logins: ump's and yours

ump's runs log in to every host as its administrator, `ump_admin`,
with UMP's key; you log in as yourself:

- **ump** passes `--extra-vars '{"ansible_ssh_user": "{{ ump_admin |
  default(ansible_user) | default(none) }}"}'`: an extra variable wins
  over `-u`, `remote_user` and the inventory, so every server ump
  installed is logged in to as its own administrator, two servers with
  two administrators in the same run. A host without `ump_admin` (one
  ump did not install, in `groups.yml`) is logged in to as the inventory
  says (`ansible_user`), else as Ansible would, and with UMP's key,
  which it does not have: give it its key,
  `ansible_ssh_private_key_file` in `host_vars/` or an `IdentityFile` in
  `~/.ssh/config` (ump's runs offer the keys given alone, below).
  Ansible's ssh connection takes `ansible_ssh_user` over `ansible_user`,
  which is why it can fall back to it. A plain
  `ansible_user={{ ump_admin }}` would fail every host without
  `ump_admin`, and every play on `localhost`.
- **You**, with `ansible-playbook playbooks/10-web.yml` from the
  repository, log in as your user name, or `-u NAME`, or `remote_user`
  of `ansible.cfg`, with your keys: the inventory names no login. The
  collection's role `users` gives your user its key and sudo on every
  server (`inventory/group_vars/common/users.yml`), so that you need
  neither UMP's key nor the administrator.

## How ump runs Ansible

```console
$ ansible-playbook playbooks/.ump-provision-4711.yml --private-key ~/.config/ump/staging_ed25519 \
    --extra-vars '{"ansible_ssh_user": "{{ ump_admin | default(ansible_user) | default(none) }}"}' \
    [ANSIBLE-ARGS...] [--limit PATTERN]
```

- In the repository's root, with `ANSIBLE_CONFIG` set to its
  `ansible.cfg`, whatever the environment says, and without
  `ANSIBLE_INVENTORY` and `ANSIBLE_HOSTS`: the inventory is the one
  `ansible.cfg` names.
- The playbooks of the run through one that imports them, written into
  `playbooks/` and removed after the run (above); a single playbook
  runs as it is, except with servers installed since a run last
  completed on them (`stale_facts` in the [state](state.md)): then the
  run starts with `meta: clear_facts` on those of them it has, so that a
  new system starts without the old one's facts, which a fact cache
  keeps.
- Every host as its administrator with UMP's key (above); the
  administrator has sudo without a password, which `become` uses.
- With the keys given only: `ANSIBLE_SSH_EXTRA_ARGS` (and
  `ANSIBLE_SFTP_EXTRA_ARGS`, `ANSIBLE_SCP_EXTRA_ARGS`) set to `-o
  IdentitiesOnly=yes`, so that ssh offers UMP's key (or the inventory's
  `ansible_ssh_private_key_file`) and not every key of your agent before
  it, which sshd's `MaxAuthTries` ends with "Too many authentication
  failures". Each is set only where neither the environment nor
  `ansible.cfg` sets it.
- The new system's host key is accepted on first contact, a changed one
  never: unless the environment or `ansible.cfg` set common SSH
  arguments of their own, ump sets `ANSIBLE_SSH_COMMON_ARGS=-o
  StrictHostKeyChecking=accept-new`. Ansible puts these after
  `ansible.cfg`'s `ssh_args`, and ssh takes the first value of an
  option, so the repository's own setting stays in force. A reinstall
  removes the old host key from `~/.ssh/known_hosts` before.
- With `UMP_PARENT` and `UMP_CONFIG`: an ump that Ansible runs in turn,
  the plugin `ofzhur.ansible.ump`, reads the state where this one does
  and the same configuration file, so it lists the installation and the
  groups ump recorded before the run. It leaves the mirror of a [state
  backend](state.md#how-the-mirror-works) to this one: no pull, no
  push, no lock to wait for.
- Arguments after `--` go to `ansible-playbook` after ump's, and
  `--limit PATTERN` of `--hosts` after them: `--check --diff`, `-v`,
  `--tags`, `-e`. `--limit` and `--inventory` are refused there, in
  every form `ansible-playbook` takes (`-l`, `-vl`, `--lim`, `-i`,
  `--inv`): the hosts are `--hosts`, which ump checks first, and the
  inventory is the repository's.
- Ansible's output goes to stderr, in its colors on a terminal; stdout
  stays for the result.

## One run at a time: the fleet's lock

`ump provision`, and the provisioning after `ump server reinstall`, take
the fleet's lock in the [state backend](state.md), beside the servers'
locks, for the whole run: the checks of the names and of `--hosts`, the
host files and Ansible; so does the host file of a reinstall with
`--no-provision`. Another run waits for it, with a line that says who
holds it, up to the configured wait (`wait:` in `config.yaml`,
`UMP_WAIT`; 15 minutes), and then gives up with exit status 3, as a
locked server does. Two reinstalls run side by side and provision one
after the other; `ump server multiinstall` runs its installations side
by side and provisions once, after the last. The lock keeps runs apart,
not servers: a server that a reinstall has in hand meanwhile is in the
run, and unreachable; its reinstall provisions it. `ump config state
locks` lists the lock; `ump config state unlock fleet` releases one that
an ump left behind when it died.

## Failures: exit status 6

ump reads Ansible's recap, per host. A run that does not complete is
exit status 6, which names the hosts that failed or were unreachable:

```
ump: provisioning failed on db-01 (failed), web-02 (unreachable), see above; fix that and run ump provision --hosts db-01,web-02
```

A failure without a host to blame (Ansible's own error) is named by its
exit status. After a reinstall, the server is judged by its own line of
the recap: failed or unreachable, the message starts with `web-01 is
installed and recorded, but not provisioned:`; without either, it is
provisioned, and a run that failed on other hosts says so:

```
ump: web-01 is provisioned; the run failed on db-01 (unreachable), see above; fix that and run ump provision --hosts db-01
```

The server is in the inventory either way, and without a line (no play
of the run matched it) it is not provisioned. `--hosts` that matches no
host is exit status 2, before anything is written (for a reinstall,
before the question). `-o json` (or `yaml`) prints the result, also on
a failure: per host `ok`, `changed`, `unreachable`, `failed`,
`skipped`, `rescued` and `ignored`, then `failed`, `unreachable` and
the `exit_status` of `ansible-playbook`; after a reinstall it is its
`provision`, with `provisioned`.

## The commands

**`ump provision [--hosts PATTERN] [--playbook NAME]... [--yes] [-o
json|yaml] [-- ANSIBLE-ARGS...]`** converges the fleet: the checks, then
under the fleet's lock the names and the hosts of `--hosts` (an Ansible
host pattern: `web-01`, `web`, `'web:&prod'`, `'all:!lab-*'`; it must
match a host), the host files of every server ump installed (in
`inventory/hosts/` or the storage; none when ump is the inventory), and
every playbook (or those of `--playbook`) on the hosts. Without
`--hosts` it asks first on a terminal, since it runs on the whole
fleet; without a terminal, `--yes` is required.

**`ump server reinstall NAME [--group GROUP]... [--hosts PATTERN]
[--playbook NAME]... [--no-provision]`** installs, records the
installation, and provisions as `ump provision` does: the whole fleet,
or the hosts of `--hosts` (`--hosts NAME` for the new server alone),
with every playbook or those of `--playbook`. Its question covers the
provisioning, and `--hosts` is checked before it.

- `--no-provision`: no layout needed, nothing runs; the host file is
  still written where the layout and `ansible-inventory` are there,
  after the checks of the names and the host file, under the fleet's
  lock. The next `ump provision` includes the server.
- `--no-reboot`: the installation is recorded, with its groups, but the
  new system is not up yet, so nothing runs; run `ump provision --hosts
  HOSTNAME` after the reboot. When ump is the inventory, the server is
  in it from the record on.
- `--dry-run`: the checks and the plan, nothing written.

**`ump server multiinstall NAME... | --plan FILE [--parallel N] [--hosts
PATTERN] [--playbook NAME]... [--no-provision]`** installs several
servers side by side, `--parallel N` at a time, each as `reinstall`
does, with the flags of a server shared or, from a [plan
file](cli.md#install-several-ump-server-multiinstall), its own (groups
among them). Parallel installations end in one fleet run: once the last
is done, ump records every server it installed and runs `ump
provision` once, under the fleet's lock, with the host files of all of
them, on the fleet or on `--hosts`. Each new server is provisioned by
its own line of the recap; a failed installation stops neither the
others nor the run, and comes first in the exit status (1, then 6).
`--hosts` is checked, with every new host in the inventory, before the
one question, and so are the hostnames, each once and none a group of
another server (new, or in the state).

**`ump server set NAME --group GROUP / --ungroup GROUP`** changes the
groups of a server ump installed in the state (repeatable, or
comma-separated; the names as for `--group`) and prints them; nothing
runs. A group is no host's name: not the server's hostname, nor, run
from the repository with `ansible-inventory` in `PATH`, another host of
the inventory (exit status 2). A server ump did not install is refused:
its groups come with the installation.

## The collection

[ofzhur.ansible](https://github.com/ofzhur/ansible) is the Ansible side
of ump, written once and used by every infrastructure repository that
includes it:

- `ofzhur.ansible.host`: the inventory plugin for the host files, in
  the repository or in a storage;
- `ofzhur.ansible.ump`: the inventory plugin that makes ump the
  inventory;
- `ofzhur.ansible.facts`: the fact cache in a central storage (below);
- `config/push/ansible.cfg`: the template of `ansible.cfg` for Ansible
  on a control machine (your laptop, a bastion, CI);
- the playbook `base`, on `common` (every server ump installs), which
  runs the role `users`: the users and groups of the group variables,
  with their keys and sudo. Its README says how. A repository imports
  it from `playbooks/00-base.yml`.

## Facts and secrets

Playbooks need more than their own server: a web server needs the
address of its database, a monitoring node the list of what to watch,
every server the certificates and passwords of its services.

Ansible runs on a control machine (a CI runner, a bastion, or your
laptop), connects to the servers of the run, gathers their facts and
hands each role what it needs; a fact cache keeps the facts of servers
outside the run (`--hosts`) between runs. The template's is a
directory of the repository, `.cache/facts`, on that machine alone.

### Facts in a central storage

The collection's fact cache `ofzhur.ansible.facts` keeps them in a
central storage instead, with the URLs and the credentials of the [host
files in a storage](#host-files-in-a-storage), in a place of its own:

```ini
# ansible.cfg
[defaults]
fact_caching = ofzhur.ansible.facts
fact_caching_connection = r2://ACCOUNT/fleet-facts/facts
fact_caching_timeout = 604800

[ofzhur_storage]
access_key_file = secrets/storage.access_key
secret_key_file = secrets/storage.secret_key
```

The [credentials](#the-storages-credentials) are the collection's own,
in `[ofzhur_storage]` or its variables, never ump's. Keep the facts out
of ump's state and apart from the host files ([three
places](#three-places)): everything that runs Ansible, pulling servers
included, reads and writes the facts, and the state holds UMP's key.

Runs from several control machines then share the facts: a run from
CI knows what a run from your laptop gathered. And one more option
opens.

### Servers that pull

With the facts central, a server can converge itself: a systemd timer
runs `ansible-pull` against the repository, which runs the plays of the
server's groups on it (`ansible-pull` limits a run to the machine
itself, by its hostname, which is its host name in the inventory), and
the server is eventually configured, also after drift, with nothing
reaching it from outside. ump still provisions it first, right after
the installation; the pull keeps it there.

A pulling server needs:

- `ansible-core` on the same side of 2.19 as the control machines: the
  facts are stored under other names from 2.19 on, and the two do not
  share them;
- `git`, and read access to the repository: a read-only deploy key;
- host files, in the repository or in the storage
  ([three forms](#the-inventory-three-forms)), since ump as the
  inventory needs ump's state;
- the credentials of the storage, as files in `[ofzhur_storage]` of its
  `ansible.cfg`, as narrow as the storage allows: it reads the host
  files and the facts of the others, and writes its own facts;
- a playbook at the root of the repository for `ansible-pull`
  (`local.yml`, which it runs when given none) that imports those of
  `playbooks/` in their order, as ump runs them; their `hosts:` pick
  the server's plays;
- the secrets its groups use, and no others: from a secrets store with
  a policy per group (Vault, OpenBao), or the key to its own encrypted
  files;
- never UMP's key, nor anything else of ump's state.

The collection's role that sets this up (the timer, the key, the
credentials) is to follow; until then, a playbook of your own does it.
`ump provision` on a schedule (CI, a timer on a control machine)
converges the servers as well, with the keys and the logs in one place.

### Secrets

Secrets are needed only where Ansible runs. They live encrypted in the
infrastructure repository ([git-crypt](https://github.com/AGWA/git-crypt),
[sops](https://github.com/getsops/sops) or `ansible-vault`) and are
decrypted on the control machine, so the servers end up holding almost
nothing but what their services use. This is the more secure setup, and
the simplest: the repository and a key that opens it is all there is. A
pulling server holds its own share, as above. Never write a token or a
private key into a profile or userdata: both end up in git and on the
server.

## NixOS

NixOS needs none of this, and is installed with `--no-provision`: its
configuration is the system, and the collection's playbooks do not
apply to it. Keep the modules of each group in the repository and
deploy them with `nixos-rebuild --target-host`, `deploy-rs` or
`colmena`. ump puts a NixOS server into the inventory all the same:
give it a group the plays leave out ([above](#groups-and-playbooks)).

## Help

Larger setups, migrations of an existing fleet to profiles and groups,
or a provisioning design of your own: help is available from the author
of UMP on a commercial basis, through the contact form at
[zhurko.de/contact](https://zhurko.de/contact/). See also [next
steps](next-steps.md).
