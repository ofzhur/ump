# State

The state is what ump knows about the fleet beyond the providers' word:
the profile, the price and the attributes set per server, the servers
added by hand, and what ump installed where. The providers' APIs say
what exists; the state says what you decided and what ump did. It is
one YAML document behind a backend, shared by everyone who uses the
same backend, and the part of ump that configuration management reads
through [the inventory](inventory.md).

```console
$ ump state                              # where it lives, its version, what it holds
$ ump state show                         # the document
$ ump state migrate --to s3://fleet/ump/state.yaml
$ UMP_STATE=file:///srv/fleet/ump/state.yaml ump server list
```

## Where it lives

Three places are asked, in this order:

1. `UMP_STATE` in the environment: a URL (below). CI and scripts set
   it, and nothing in the configuration file is needed.
2. `state.url` in `config.yaml`, set by hand or by `ump state migrate`.
3. Nothing: the local state file, `~/.local/state/ump/state.yaml`
   (`$XDG_STATE_HOME/ump/state.yaml`). A fresh system starts here, and a
   single person on one machine never needs more.

`ump state` says which of the three applies.

## Backends

| URL | Backend | Needs |
|---|---|---|
| `local` | the local state file | nothing |
| `file:///path/state.yaml` | a file on this machine, for example in the fleet's git repository, where history and sharing come from git | nothing |
| `ssh://[user@]host/path/state.yaml` | a file on another machine, reached with your ssh client (keys, agent, `~/.ssh/config`) | an account there with `sh`, `sha256sum` and `mkdir` |
| `s3://bucket/key` | an S3 bucket, or any S3-compatible store that supports conditional writes (`If-Match`): AWS S3, MinIO, ... | `state.endpoint` (AWS from the region when empty), `state.region`, `state.access_key`, `state.secret_key` |
| `r2://account/bucket/key` | a Cloudflare R2 bucket | `state.access_key`, `state.secret_key` |
| `http://` or `https://host/path` | an HTTP service, Terraform-style: `GET` with an ETag, `PUT` with `If-Match` (`If-None-Match: *` for the first write; 412 or 409 is a conflict), `LOCK` and `UNLOCK` with `?key=SERVER` (423 when held, the holder as JSON in the body), `GET ?locks` | `state.token` (bearer), or `state.user` and `state.password` |
| `consul://host:8500/path` | a key in Consul's KV store (`http+consul://` for plain http) | `state.token` |
| `vault://host:8200/mount/path` | a secret in a KV version 2 engine of Vault (`http+vault://` for plain http) | `state.token` |

The credentials come from `state.*` in the configuration, as secret
references like an account's (`{file: ...}`, `{env: ...}`, `{command:
...}`), or from the environment, which wins: `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `UMP_STATE_ENDPOINT`,
`UMP_STATE_TOKEN` (also `VAULT_TOKEN` and `CONSUL_HTTP_TOKEN`),
`UMP_STATE_USER`, `UMP_STATE_PASSWORD`. No secret is ever in the state
itself.

```yaml
# config.yaml
state:
  url: s3://fleet/ump/state.yaml
  endpoint: https://s3.eu-central-1.amazonaws.com   # optional for AWS
  region: eu-central-1
  access_key: {env: FLEET_STATE_KEY}
  secret_key: {file: ~/.config/ump/secrets/state.secret}
```

## How it is written

Every change reads the document with its version (the file's hash, the
object's ETag, Consul's modify index, Vault's version), applies the
change and writes it back only if the version is still the same; when
someone wrote in between, ump reads again and reapplies the change. So
two people setting attributes on different servers at the same moment
both get their change in, and nobody overwrites the other.

Operations that must not run twice on one machine, `ump server
reinstall` and `ump server rescue`, take a **lock per server** for their
duration: a second ump on the same server is refused with who holds the
lock and since when, while every other server stays free. `ump state
locks` lists them, `ump state unlock SERVER` releases one an ump that
died left behind.

## The document

```yaml
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
wherever the state does. Unknown fields are an error, so a typo is
caught before anything is written.

## Editing it

By command: `ump server set`, `ump server add` and `remove`, and the
reinstall. By hand: where the backend is a file (`local`, `file://`),
edit the file; ump checks it on the next read and names the problem.
Anywhere:

```console
$ ump state edit                 # your editor, then a checked write, refused when someone wrote meanwhile
$ ump state pull fleet.yaml      # the document, with the version it was pulled from
$ ump state push fleet.yaml      # written back over that version; refused when the state moved on
$ ump state push fleet.yaml --force
```

A command rewrites the document in its own layout, so comments in it do
not survive; a server's `note` is the place for remarks.

## Moving it

```console
$ ump state migrate --to file:///srv/fleet/ump/state.yaml
$ ump state migrate --to r2://account/bucket/ump/state.yaml
```

`migrate` reads the state where it is, writes it to the new backend
(which must be empty, unless `--force`) and sets `state.url` in the
configuration file; the old copy stays where it was. Set the new
backend's credentials in the configuration or the environment first.
With `UMP_STATE` in the environment the configuration is not changed:
the environment keeps saying where the state is, and `migrate` tells
you to point it at the new place. Moving back is the same command.

A configuration file from before the state (with `servers:` or
`manual:` in it) is migrated by the first command that needs the
state: the entries move into the state, the file loses them, and ump
says so once.
