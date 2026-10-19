# Provider accounts

A provider is a hosting API that knows about servers: which ones exist,
how to reset them, how to boot them into a rescue system, how to set
their names and reverse DNS. A **provider account** (`ump config isp`,
for *internet service provider*) is one set of credentials for one
provider, stored in the configuration under a name of your choice. You
can have several accounts of the same type, say one per Hetzner Cloud
project, and `ump server list` shows the servers of all of them together.

```console
$ ump config isp list
NAME     TYPE            SERVERS   SETTINGS             SECRETS
hcloud   hetzner-cloud   4         <none>               token=file
hrobot   hetzner-robot   2         login=#ws+AbCdEfGh   password=file
```

SERVERS is what the last listing found (from the cache, no API call;
`-` before the first one); SECRETS says where each secret comes from,
`-o wide` spells the references out (`token=file:~/.config/ump/secrets/hcloud.token`),
and `-o json` or `yaml` carry everything.

## Provider types

```console
$ ump config isp types
TYPE            KIND         CAPABILITIES                                          DESCRIPTION
hetzner-cloud   cloud        inventory, describe, rename, rdns, power, rescue      Hetzner Cloud servers
hetzner-robot   bare-metal   inventory, describe, rename, rdns, power, rescue      Hetzner dedicated servers (Robot web service)
ovh             mixed        inventory, describe, rename, rdns, power, rescue      OVHcloud dedicated servers and Public Cloud instances
manual          mixed        inventory, describe                                   servers added by hand (ump server add): no API, no rescue system; reached over SSH alone
```

| Type | Lists | Credentials | Where to get them |
|---|---|---|---|
| `hetzner-robot` | Hetzner dedicated servers | `login` (`#ws+...`), `password` | Robot → Settings → Web service and app settings |
| `hetzner-cloud` | the servers of one Hetzner Cloud project | `token` | Cloud Console → the project → Security → API tokens; read permission is enough for listing, read and write for everything else |
| `ovh` | OVHcloud dedicated servers, VPS and Public Cloud instances of one account | `endpoint` (`ovh-eu`, `ovh-ca`, `ovh-us`, or a URL), `application_key`, `application_secret`, `consumer_key` | an application at `https://eu.api.ovh.com/createApp/` (`ca.`/`us.` for the other endpoints) gives key and secret; the wizard requests the consumer key for you |
| `digitalocean` | the droplets of one DigitalOcean team | `token` | Control Panel → API → Tokens → Generate New Token; read for listing, write (or the custom scopes droplet read and update, actions read) for renaming and power |
| `linode` | the instances of one Linode (Akamai Cloud Computing) account | `token` | Cloud Manager → your profile → API Tokens → Create a Personal Access Token, with Linodes and IPs read/write and Images read only |
| `vultr` | the cloud instances and bare metal servers of one Vultr account | `api_key` | customer portal → Account → API → Enable API; under Access Control, allow the addresses ump runs from (or all IPv4 and IPv6) |
| `manual` | the servers added with `ump server add` | none | the account `manual` appears by itself with the first server added; see [manual servers](isps.md#manual-servers-manual) |

`ump config isp types` prints the settings of every type below the table,
with which are required and which are secret. The API types differ
in the details, most of all in the way into a rescue system: see
[ISPs](isps.md). The `manual` type has no API
behind it and does what SSH allows.

## Adding an account

On a terminal, the wizard:

```console
$ ump config isp add
UMP configuration wizard
Configuration file: ~/.config/ump/config.yaml
No accounts configured yet.
Type q at a menu or press Ctrl-C to quit. ...

Provider type
  1) hetzner-cloud   Hetzner Cloud servers
  2) hetzner-robot   Hetzner dedicated servers (Robot web service)
  3) ovh             OVHcloud dedicated servers and Public Cloud instances
  q) quit
Choice [1]: 1

Account name [hcloud]:

Where should this account's secrets be kept?
  1) files under ~/.config/ump/secrets, readable only by you
  2) environment variables that you set in your shell
  3) a command that prints the secret (pass, op, sops, vault, ...)
  q) quit
Choice [1]:

  a project API token; read permission is enough for listing (Cloud Console: project → Security → API tokens)
API token:

Testing hcloud ... ok (3 servers)
Saved account hcloud (hetzner-cloud) to ~/.config/ump/config.yaml
Add another account? [y/N]

Done. Try: ump server list
```

On a fresh system it first asks whether everything should stay in
`~/.config/ump` on this machine, and runs `ump config state setup` when the
answer is no, so that a team's first account lands in the shared state.
It then asks for the provider type, an account name and the credentials
(secrets are read without echo), keeps the secrets as files in the state,
wherever it lives, tests them with one API call (a listing) and writes
the account to the configuration. Environment variables and commands as
secret references are the non-interactive form's (`--set
password=env:NAME`, `--set password=command:pass show x`). An account is written only after its test passed, or
you chose to save it anyway, so leaving with `q` or Ctrl-C never saves a
half-entered account. In the non-interactive form, a secret referenced
through an environment variable that is not set in the current shell
skips the test and the account is saved with a note. For OVHcloud the wizard requests a consumer key with
the access rules the commands need and hands you the validation URL to
approve it.

Without a terminal, in CI or from an agent:

```console
$ ump config isp add hetzner-robot hrobot --set login='#ws+AbCdEfGh' --set password=env:HROBOT_PASSWORD
$ ump config isp add hetzner-cloud hcloud --set token=file:~/.config/ump/secrets/hcloud.token
$ ump config isp add ovh ovh --set endpoint=ovh-eu --set application_key=AbCdEfGh \
      --set application_secret=command:"pass show ovh/app-secret" --set consumer_key=env:OVH_CK
```

A value is plain text or a secret reference, `env:NAME`, `file:PATH` or
`command:CMD`; the credentials are tested before the account is saved
unless `--no-test` is given, and an account of the same name is replaced.
Account names are lower-case letters, digits, `-`, `_` and `.`.

### Secrets

Credentials never need to land in `config.yaml` as plaintext. A setting
is a plain string or a reference to where the secret is:

| In `config.yaml` | With `--set` | Source |
|---|---|---|
| `{ env: NAME }` | `env:NAME` | an environment variable |
| `{ file: PATH }` | `file:PATH` | the content of a file, trailing newline stripped; the wizard writes these under `~/.config/ump/secrets/` with mode 0600 |
| `{ command: "..." }` | `command:CMD` | the standard output of a command: `pass show ...`, `op read ...`, `sops -d ...`, `vault kv get ...` |

References are resolved when a command needs the account. Plaintext
values are accepted; `ump config isp list` never shows them (it says
`plain!`, a hint to move them out) and shows references as they are. The secret files stay under your own
configuration directory even when `--config` points at a configuration in
a repository, so a shared configuration carries references only.

## Managing accounts

```
ump config isp list              # the accounts: type, servers, settings, where the secrets come from
ump config isp types             # the provider types, their capabilities and settings
ump config isp add               # the wizard, on a terminal
ump config isp add TYPE NAME --set KEY=VALUE ... [--no-test]
ump config isp test [NAME...]    # resolve the secrets and list once; all accounts without names
ump config isp edit [NAME]       # that account alone in your editor, or the whole configuration file
ump config isp remove NAME       # the account and the secret files the wizard created for it
```

`edit NAME` opens that account alone (its type and settings, values
plain or as [secret references](#secrets)); when the editor returns, it
is checked (a known type, known settings, the required ones present)
and put back into the configuration file, the other accounts, the rest
of the file and its comments untouched. An invalid edit saves nothing:
on a terminal ump offers to open it again, otherwise it says where the
edit is kept. `edit` without a name opens the whole file and checks
every account afterwards. The check does not resolve the secrets; `ump
config isp test NAME` tries the credentials.

`ump config isp` is also `i`, `provider` or `account`, and the verbs
have the short forms of every `ump config` entity (`ump c i v`); see
the [CLI reference](cli.md#ump-config). Every command that touches servers takes `--account NAME`
(repeatable) to work with some accounts only; without it, every account
is used, queried in parallel, and an account whose API fails is reported
on stderr while the others are still listed (exit code 4).

## Per provider

What each provider type does in detail (credentials and IDs, prices,
renaming, power, reverse DNS, the way into a rescue system, what `ump
server show` adds) is in [ISPs](isps.md).

## Adding a provider type

Providers are small Go packages under `internal/provider/<name>` with
hand-written REST clients: UMP needs a handful of calls per provider, and
the binary does not pick up an SDK's dependency tree.

1. Declare a `provider.Type`: the name used in `config.yaml`, a
   description for the wizard, the settings as `Fields` (label, help,
   secret, required, choices, defaults; the wizard, `ump config isp add`
   and `types` are driven by them) and a `New` function that builds an
   instance from resolved settings. A `Complete` hook can fill settings
   interactively (OVH uses it to request the consumer key).
2. Implement `Provider` plus the capability interfaces: `Inventory`
   first, then `Describer`, `Renamer`, `ReverseDNS`, `Power`, `Rescue`
   as the API allows. The CLI checks them with type assertions, and a
   command that needs one the provider lacks says so instead of failing
   half-way.
3. Map the provider's objects to UMP's `Machine`: ID, name, addresses,
   kind, location, product, status, labels. Lower-case locations and
   statuses; keep product names as the provider spells them.
4. Register the type in `internal/provider/all`, and add tests against
   an `httptest` server with recorded API responses.

Provider quirks stay inside the provider package. If one leaks into the
rest of ump, the interface is missing something. Providers UMP does not
have yet are listed in the [roadmap](roadmap.md).
