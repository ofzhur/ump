# The home and its backend

Everything ump keeps lives in one directory, its **home**: `config.yaml`
with the accounts, the secret files, the profiles with the layouts,
firewalls and users, the named keys, UMP's own SSH key, `state.yaml`
(what ump knows about servers beyond the providers' word: profile,
price, attributes, the servers added by hand, what was installed where)
and the listing cache. Without a backend that directory is
`~/.config/ump`, and a single person on one machine never needs more.

With a **backend**, the home lives there, shared by everyone who uses
the same one, UMP's key included, so a team acts as one UMP. ump works
on a **mirror** of it under its cache directory: pulled before every
command, pushed after it, file by file with version tags, so the files
stay editable by hand and a concurrent writer is noticed, never
overwritten in silence. `state.yaml` and the per-server locks of `ump
server reinstall` and `rescue` go through the backend directly.

```console
$ ump config state set                          # the wizard: pick a backend, copy the home there
$ ump config state view                         # where the home is, the mirror, what it holds
$ ump config state migrate --to file:///srv/fleet/ump
$ UMP_STATE=r2://account/bucket/ump ump server list
```

## Where the home is

Three places are asked, in this order:

1. `UMP_STATE` in the environment: a backend URL (below), with the
   credential variables. CI and scripts set it, and nothing on disk is
   needed.
2. `~/.config/ump/backend.yaml`: the pointer `ump config state` writes.
3. Nothing: the home is `~/.config/ump` itself (`$XDG_CONFIG_HOME/ump`).
   A fresh system starts here.

`ump config state view` says which applies, and where the mirror is.

## Backends

| URL | Backend | Needs |
|---|---|---|
| `file:///path/to/dir` | a directory on this machine, for example in the fleet's git repository, where history and sharing come from git | nothing |
| `ssh://[user@]host/path/to/dir` | a directory on another machine, reached with your ssh client (keys, agent, `~/.ssh/config`) | an account there with `sh`, `sha256sum`, `find` and `mkdir` |
| `s3://bucket/prefix` | an S3 bucket, or any S3-compatible store that supports conditional writes (`If-Match`): AWS S3, MinIO, ... | `endpoint` (AWS from the region when empty), `region`, `access_key`, `secret_key` |
| `r2://account/bucket/prefix` | a Cloudflare R2 bucket | `access_key`, `secret_key` |
| `http://` or `https://host/path` | a service, Terraform-style: `GET /path/FILE` with an ETag, `PUT` with `If-Match` (`If-None-Match: *` for a new file; 412 or 409 is a conflict), `DELETE`, `GET /path?list` as JSON `[{"path", "version"}]`, `LOCK` and `UNLOCK /path?key=SERVER` (423 when held, the holder as JSON), `GET /path?locks` | `token` (bearer), or `user` and `password` |
| `consul://host:8500/prefix` | keys in Consul's KV store (`http+consul://` for plain http) | `token` |
| `vault://host:8200/mount/prefix` | secrets in a KV version 2 engine of Vault, one per file (`http+vault://` for plain http) | `token` |

The credentials come from `backend.yaml` as references (`{env: NAME}`,
`{file: PATH}`, `{command: CMD}`), written by `ump config state` (a
value given as such is kept in a file of its own next to the pointer),
or from the environment, which wins: `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `UMP_STATE_ENDPOINT`,
`UMP_STATE_TOKEN` (also `VAULT_TOKEN` and `CONSUL_HTTP_TOKEN`),
`UMP_STATE_USER`, `UMP_STATE_PASSWORD`.

```yaml
# ~/.config/ump/backend.yaml
url: s3://fleet/ump
region: eu-central-1
access_key: {env: FLEET_STATE_KEY}
secret_key: {file: ~/.config/ump/backend.secret_key}
```

The home holds secrets, the accounts' and UMP's key, in clear text,
as `~/.config/ump` does; choose a backend accordingly (a private
bucket, a Vault, a repository nobody else reads).

## How the mirror works

Before a command, ump lists the backend and downloads the files whose
version changed; a file changed in the mirror meanwhile is left as it
is. After the command, every file that changed in the mirror is written
back over the version it was pulled at; a file that changed on both
sides is a **conflict**: ump names it, keeps both copies, and `ump
config state push --force` writes the mirror's copy over the backend's
when that is what you want. When the backend does not answer, the
command runs on the mirror as it is and says so; nothing is pushed
until the backend is back.

`state.yaml` is special: every change to it is a read-modify-write on
the backend's version tag, retried on conflict, so two people setting
attributes on different servers at the same moment both get their
change in. Operations that must not run twice on one machine, `ump
server reinstall` and `rescue`, take a **lock per server** on the
backend for their duration; a second ump on the same server is refused
with who holds the lock and since when, while every other server stays
free. `ump config state locks` lists them, `unlock SERVER` releases one
an ump that died left behind.

## Commands

```
ump config state view                  # where the home is, the mirror, what it holds
ump config state set                   # the wizard, on a terminal: pick a backend, copy the home there
ump config state set --url URL [...]   # point ump at a backend as it is (the home there is used)
ump config state migrate --to URL [...] [--force]   # copy the current home there, then point ump at it
ump config state clear                 # no backend: the local directory again
ump config state pull | push [--force] # the mirror and the backend, by hand
ump config state test                  # does the backend answer?
ump config state locks | unlock SERVER
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
version: 1
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
manual:
  rack-07:
    ipv4: 192.0.2.7
    location: basement
    product: Dell R640
    kind: bare-metal
    note: KVM at 192.0.2.250
```

`servers` is keyed by `provider/id`; a record that holds nothing
disappears. `installed` is written by `ump server reinstall` when the
new system answered, from what `/etc/ump/install` on the machine says,
and shown by `ump server show`. `manual` holds what `ump server add`
took; its servers are listed by the account `manual`, which exists
wherever the state does. Edit the file by hand (in the home, or in the
mirror and push) or with `ump server set`, `add` and `remove`; unknown
fields are an error, so a typo is caught before anything is written,
and a command rewrites the file in its own layout, so a server's `note`
is the place for remarks.
