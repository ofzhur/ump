# The state backend

The **state** is everything ump keeps: `config.yaml` with the
accounts, the secret files, the profiles with the layouts, firewalls,
private networks, users and userdata, the named keys, UMP's own SSH
key, `state.yaml` (what ump knows about servers beyond the providers'
word: profile, price, attributes, the servers added by hand, what was
installed where, the groups, the private addresses)
and the listing cache. It lives in a **state backend**. Without one
configured, that is the local directory `~/.config/ump`, and a single
person on one machine never needs more.

With a shared state backend, everyone who uses the same one has the
same state, UMP's key included, so a team acts as one UMP. ump works
on a **mirror** of it under its cache directory: pulled before a
command (at most every 30 seconds, see [how the mirror
works](#how-the-mirror-works)), pushed after it, file by file with version tags, so the files
stay editable by hand and a concurrent writer is noticed, never
overwritten in silence. `state.yaml`, the locks of a server (`ump
server reinstall` and `rescue`) and the fleet's lock (`ump provision`)
go through the backend directly.

```console
$ ump config state setup                        # the wizard: pick a backend, migrate the state there
$ ump config state view                         # which backend, the mirror, what the state holds
$ ump config state migrate --to file:///srv/fleet/ump
$ UMP_STATE=r2://account/bucket/ump ump server list
```

## Which backend

Three places are asked, in this order:

1. `UMP_STATE` in the environment: a backend URL (below), with the
   credential variables. CI and scripts set it, and nothing on disk is
   needed.
2. `~/.config/ump/backend.yaml`: the pointer `ump config state setup`
   (or `set`, `migrate`) writes.
3. Nothing: the state is `~/.config/ump` itself (`$XDG_CONFIG_HOME/ump`).
   A fresh system starts here.

`ump config state view` says which applies, and where the mirror is.

## Setting it up

`ump config state setup` asks on a terminal: the kind of backend, its
address and credentials. It checks that the backend answers and then
looks at both sides. A backend that holds state already is used as it
is, unless this machine holds state too and you choose to overwrite
the backend with it. An empty backend gets this machine's state copied
over when there is any (the wizard says what it holds: accounts,
profiles, servers with settings) and asks first; that is a
**migration**, the same as `migrate --to URL`, and the copy left on
this machine stays. On a clean start nothing is copied and the backend
starts empty. The account wizard (`ump config isp add`) runs the same
questions on a fresh system when you say the state should not stay on
this machine.

## Backends

| URL | Backend | Needs |
|---|---|---|
| `file:///path/to/dir` | a directory on this machine, for example in the fleet's git repository, where history and sharing come from git | nothing |
| `ssh://[user@]host/path/to/dir` | a directory on another machine, reached with your ssh client (keys, agent, `~/.ssh/config`) | an account there with `sh`, `sha256sum`, `find` and `mkdir` |
| `s3://bucket/prefix` | an S3 bucket, or any S3-compatible store that supports conditional writes (`If-Match`): AWS S3, MinIO, ... | `endpoint` (AWS from the region when empty), `region`, `access_key`, `secret_key` |
| `r2://account/bucket/prefix` | a Cloudflare R2 bucket; `account` is the Cloudflare account ID (see [R2](#r2) below) | `access_key`, `secret_key` |
| `http://` or `https://host/path` | a service, Terraform-style: `GET /path/FILE` with an ETag, `PUT` with `If-Match` (`If-None-Match: *` for a new file; 412 or 409 is a conflict), `DELETE`, `GET /path?list` as JSON `[{"path", "version"}]`, `LOCK` and `UNLOCK /path?key=SERVER` (423 when held, the holder as JSON), `GET /path?locks` | `token` (bearer), or `user` and `password` |
| `consul://host:8500/prefix` | keys in Consul's KV store (`http+consul://` for plain http) | `token` |
| `vault://host:8200/mount/prefix` | secrets in a KV version 2 engine of Vault, one per file (`http+vault://` for plain http) | `token` |

The credentials come from `backend.yaml` as references (`{env: NAME}`,
`{file: PATH}`, `{command: CMD}`), written by `ump config state setup` (a
value given as such is kept in a file of its own next to the pointer),
or from the environment, which wins: `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `UMP_STATE_ENDPOINT`,
`UMP_STATE_TOKEN` (also `VAULT_TOKEN` and `CONSUL_HTTP_TOKEN`),
`UMP_STATE_USER`, `UMP_STATE_PASSWORD`.

### R2

The three parts of an `r2://` URL come from the Cloudflare dashboard:

- **account**: the account ID, 32 hexadecimal characters. It is shown
  as "Account ID" on the right of the account's home page and of the R2
  overview, and it is the first part of the bucket's S3 API address,
  `https://ACCOUNT.r2.cloudflarestorage.com`, which the bucket's
  Settings page lists.
- **bucket**: the bucket's name, under R2 > Overview > Create bucket.
  Keep it private: no public access, no custom domain, since the state
  holds secrets.
- **prefix**: any path inside the bucket, `ump` for example; it may be
  left out.

The keys come from R2 > Manage API tokens > Create API token, with the
permission "Object Read & Write", scoped to that bucket alone. Its
"Access Key ID" is the `access_key` and its "Secret Access Key" the
`secret_key`; Cloudflare shows the secret once, so keep it right away
(as a file, or in your password manager behind a `command:`
reference). The token value shown above them is not needed.

```console
$ ump config state migrate --to r2://0123456789abcdef0123456789abcdef/fleet-state/ump \
    --access-key env:R2_ACCESS_KEY_ID --secret-key file:~/.config/ump/r2.secret
```

```yaml
# ~/.config/ump/backend.yaml
url: s3://fleet/ump
region: eu-central-1
access_key: {env: FLEET_STATE_KEY}
secret_key: {file: ~/.config/ump/backend.secret_key}
```

The state holds secrets, the accounts' and UMP's key, in clear text,
as `~/.config/ump` does; choose a backend accordingly (a private
bucket, a Vault, a repository nobody else reads). The host files of
the Ansible inventory and Ansible's facts take the same URLs but
`http(s)://` ([host files in a
storage](provisioning.md#host-files-in-a-storage), [facts in a central
storage](provisioning.md#facts-in-a-central-storage)): keep each in a
place of its own, out of the state, with credentials of their own, the
collection's in `[ofzhur_storage]` of `ansible.cfg`, which ump takes
for the host files instead of these; on R2, that is three buckets
([three places](provisioning.md#three-places)).

The accounts' secret files (`secrets/NAME`) are part of the state and
move with it. A reference written as `file: secrets/NAME` is in the
home wherever it is; one written before a migration as
`~/.config/ump/secrets/NAME`, or naming another machine's mirror, is
found in the home too when that path does not exist on this machine.

## How the mirror works

Before a command, ump lists the backend and downloads the files whose
version changed; a file changed in the mirror meanwhile is left as it
is. After the command, every file that changed in the mirror is written
back over the version it was pulled at; a file that changed on both
sides is a **conflict**: ump names it and keeps both copies. `ump
config state pull --force` takes the backend's copy, `ump config state
push --force` writes the mirror's copy over the backend's; with
`state.yaml`, which commands write to the backend directly, the
backend's copy is the one to keep unless you edited the mirror's by
hand. When the backend does not answer, the command runs on the mirror
as it is and says so; nothing is pushed until the backend is back.

Several ump processes on one machine, such as reinstalls of different
servers side by side, share the mirror: a lock file in it
(`.mirror.lock`) lets one pull or push at a time go ahead, and the
others wait for it.

An ump that ump itself runs, the inventory plugin `ofzhur.ansible.ump`
under the Ansible of `ump provision` (also after `ump server
reinstall`), is part of that run. ump sets `UMP_PARENT` for it, where
it reads the state (`backend`; `mirror` when the backend did not answer
its pull; `local`), and `UMP_CONFIG`, its configuration file. That ump
reads the state there too, so it sees what its parent recorded before
the run, and neither pulls nor pushes: the mirror is its parent's for
the run, and it waits for no lock.

ump goes to the backend only when it has to, so that it never makes
you wait for nothing:

- **Shell completion, help and the version** never touch the backend,
  not even its credentials (a reference may run a password manager):
  they read the mirror. `eval "$(ump completion bash)"` in a bashrc and
  every Tab answer at once, online or not.
- **A pull holds 30 seconds:** a command within that of the last pull
  works with the mirror as it is. A burst of commands, or a script,
  pulls once.
- **A pull takes 10 seconds at most**, and after one that failed
  (offline, the backend down) the commands of the next 2 minutes do not
  try again: they run on the mirror without a word, and what they
  change goes back with the first push that reaches the backend. `ump
  config state pull` pulls at once, whenever you want the latest.

`state.yaml` is special: every change to it is a read-modify-write on
the backend's version tag, retried on conflict, so two people setting
attributes on different servers at the same moment both get their
change in. Operations that must not run twice on one machine, `ump
server reinstall` and `rescue`, take a **lock per server** on the
backend for their duration; a second ump on the same server is refused
with who holds the lock and since when, while every other server stays
free. A provisioning run (`ump provision`, also after a reinstall)
takes the **fleet's lock**, `fleet`: one run at a time; another waits
for it, saying who holds it, up to the configured wait, and is then
refused the same way. `ump config state locks` lists them, `unlock
SERVER` or `unlock fleet` releases one an ump that died left behind.

## Commands

```
ump config state view                  # which backend, the mirror, what the state holds
ump config state setup                 # the wizard, on a terminal: pick a backend, migrate the state there
ump config state set --url URL [...]   # point ump at a backend as it is (the state there is used)
ump config state migrate --to URL [...] [--force]   # copy the current state there, then point ump at it
ump config state clear                 # no backend: the local directory again
ump config state pull | push [--force] # the mirror and the backend, by hand
ump config state test                  # does the backend answer?
ump config state locks | unlock SERVER|fleet
```

The backend flags of `set` and `migrate`: `--endpoint`, `--region`,
`--access-key`, `--secret-key`, `--token`, `--user`, `--password`, each
a reference or a value. `migrate` refuses a backend that holds files
unless `--force`; the copy left behind stays. Moving back is `migrate
--to file:///home/you/.config/ump` or `clear` (then the local directory
is used as it was).

## The state document

```yaml
# state.yaml
version: 2
updated: 2026-10-14T12:00:00Z
servers:
  hetzner-cloud/234567:
    profile: web
    price: {amount: 5.99, currency: EUR, period: month}
    attributes: {owner: alice, role: web}
    installed:
      os: ubuntu-26.04
      image_version: 2026.10.14
      kernel: 7.0.0-14-generic
      profile: web
      layout: lvm-linear
      hostname: web-01
      admin: ump
      interfaces: {ext01: "96:00:01:02:03:04"}
      at: 2026-10-14T11:58:30Z
    groups: [web]
    private: {network: default, cidr: 10.0.0.0/24, address: 10.0.0.1}
manual:
  rack-07:
    ipv4: 192.0.2.7
    location: basement
    product: Dell R640
    kind: bare-metal
    note: KVM at 192.0.2.250
```

`version` is the layout of the file: 2 since `private`. An ump reads
the layouts up to its own and refuses a later one before anything else
(`the state is version 2; this ump reads up to 1`); an ump from before
that check fails instead on a key it does not know, where there is one:
`private`, or `stale_facts` for one older still. A state of an older
layout loads as it is, and the next write gives it this ump's. So every
machine that shares a state upgrades ump together: once one writes it,
the others refuse it until they upgrade.

`servers` is keyed by `provider/id`; a record that holds nothing
disappears. `installed` is written by `ump server reinstall` when the
new system answered, from what `/etc/ump/install` on the machine says
(with `--no-reboot` when the installer finished, without the image
version, kernel and interfaces), and shown by `ump server show`.
`groups` are the server's Ansible groups besides `common`
([provisioning](provisioning.md)): set by `ump server reinstall
--group`, changed by `ump server set --group` and `--ungroup`; the
listings show them as in the inventory, `common` first. `private` is
the server's address in a [private network](networks.md), with the
network's CIDR as it was when the address was given: recorded when an
installation starts, once the question is answered, in a
read-modify-write that refuses an address another server has, in any
network that has it, or a server whose address changed meanwhile (so
two umps never give one address twice, and what the installed system
records is what the state says), given back by an installation that
fails before its installer wrote the disks, and changed by `ump server
set --private-ip` and `--private-network` under the server's lock.
`stale_facts: true` comes with every installation recorded and goes once a
provisioning run completed on the server: until then, every run with it
clears the facts a fact cache still has of its old system. A state an
older ump wrote with `roles` is read as `groups` (`-` and `.` become
`_`) and written back as such. `manual` holds what `ump server add`
took; its servers are listed by the account `manual`, which exists
wherever the state does. Edit the file by hand (in the local directory,
or in the mirror and push) or with `ump server set`, `add` and
`remove`; unknown fields are an error, so a typo is caught before
anything is written, and a command rewrites the file in its own layout,
so a server's `note` is the place for remarks.
