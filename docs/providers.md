# Provider accounts

A provider is a hosting API that knows about servers: which ones exist,
how to reset them, how to boot them into a rescue system, how to set
their names and reverse DNS. A **provider account** (`ump config isp`,
for *internet service provider*) is one set of credentials for one
provider, stored in the configuration under a name of your choice. You
can have several accounts of the same type, say one per Hetzner Cloud
project, and `ump server list` shows the servers of all of them together.

```console
$ ump config isp view
# ~/.config/ump/config.yaml
accounts:
  hcloud:
    type: hetzner-cloud
    token: {file: ~/.config/ump/secrets/hcloud.token}
  hrobot:
    type: hetzner-robot
    login: '#ws+AbCdEfGh'
    password: {file: ~/.config/ump/secrets/hrobot.password}
```

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
| `manual` | the servers added with `ump server add` | none | the account `manual` appears by itself with the first server added; see [manual servers](#manual-servers-manual) |

`ump config isp types` prints the settings of every type below the table,
with which are required and which are secret. The three API types
implement every capability UMP has today, so every command of `ump
server` works with every account; the differences are in the details,
see [per provider](#per-provider) below. The `manual` type has no API
behind it and does what SSH allows.

## Adding an account

On a terminal, the wizard:

```console
$ ump config isp
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

It asks for the provider type, an account name, where the account's
secrets should be kept, and the credentials (secrets are read without
echo), tests them with one API call (a listing) and writes the account to
the configuration. An account is written only after its test passed, or
you chose to save it anyway, so leaving with `q` or Ctrl-C never saves a
half-entered account. With environment variables that are not set in the
current shell, the test is skipped and the wizard prints the `export`
lines to run first. For OVHcloud the wizard requests a consumer key with
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
values are accepted; `ump config isp view` redacts them and shows
references as they are. The secret files stay under your own
configuration directory even when `--config` points at a configuration in
a repository, so a shared configuration carries references only.

## Managing accounts

```
ump config isp                   # the wizard
ump config isp view              # the configuration, plaintext secrets redacted
ump config isp types             # the provider types, their capabilities and settings
ump config isp add TYPE NAME --set KEY=VALUE ... [--no-test]
ump config isp test [NAME...]    # resolve the secrets and list once; all accounts without names
ump config isp remove NAME       # the account and the secret files the wizard created for it
```

`ump config isp` has the aliases `provider`, `providers`, `account` and
`accounts`. Every command that touches servers takes `--account NAME`
(repeatable) to work with some accounts only; without it, every account
is used, queried in parallel, and an account whose API fails is reported
on stderr while the others are still listed (exit code 4).

## Per provider

### Hetzner Robot (`hetzner-robot`)

- Dedicated servers. The server name is a free label at Hetzner, not
  required to be unique; `ump server rename` sets it and refuses a name
  another listed server has (`--force` renames anyway).
- Rescue: UMP's key (and `--ssh-key` files) must be registered at the
  account; the provider registers keys it does not know by fingerprint,
  then arms the rescue system and resets the server. Robot prints the
  rescue system's host key fingerprints, and ump shows them.
- Power: `reboot` asks the system to reboot (ACPI), `--hard` is a
  hardware reset, `--power-cycle` cuts the power; a server that is
  switched off is switched on instead, because a reset does nothing
  then. `stop` presses the power button (ACPI shutdown), `--hard` holds
  it; `start` presses it.
- Reverse DNS is set for any address of the server (primary IP,
  additional IPs, subnets, the IPv6 /64), and any name is accepted.
- `ump server show` adds the contract (product, traffic, paid until,
  cancellation), addresses, features, power state and reset types, and
  the rescue state.

### Hetzner Cloud (`hetzner-cloud`)

- One account is one project; its API token is the credential. Server
  names must be unique within the project and valid hostnames; the API's
  uniqueness error is reported plainly.
- Rescue: the rescue system is enabled with the project's SSH keys (UMP's
  is registered when missing) and the server reset; Hetzner always issues
  a root password too. Actions are followed until they finish.
- Power: soft reboot, hard reset, power cycle, start and stop all exist.
- Reverse DNS works for the IPv4 and for single addresses inside the
  IPv6 /64; deleting an entry restores Hetzner's default name.
- `ump server show` adds the server type, image, location, networks
  (PTR records, floating IPs, private networks, cloud firewalls), traffic
  and protection.

### OVHcloud (`ovh`)

One account covers dedicated servers, VPS and Public Cloud instances,
because one set of API keys does. A dedicated server's ID is its service
name (`ns1234.ip-1-2-3.eu`) and its name the display name when one is
set; instances carry their project as labels; VPS are labelled
`family: vps`.

- Credentials: an application key and secret, plus a consumer key with
  access rules. The wizard requests a consumer key with exactly the rules
  the commands need and shows the validation URL; a consumer key made
  elsewhere needs the same rules, and a missing one shows up as
  `NOT_GRANTED_CALL` with the path. A key that may not see one family
  (say, no Public Cloud) is tolerated: that family is skipped. A consumer
  key made before VPS support lacks the `/vps` rules and shows no VPS;
  `ump config isp add` requests a new key with them.
- Rename: Public Cloud instances and VPS can be renamed; dedicated servers
  cannot through the API (the display name is read-only there), and ump
  says so.
- Power: a dedicated server knows one reboot (a hardware reset) and cannot
  be switched on or off through the API; instances and VPS reboot, start
  and stop. Every change to a VPS is a task at OVH, and OVH refuses the
  next action while one runs, so ump waits for the tasks.
- Rescue: a dedicated server takes one rescue SSH key; an instance is put
  into rescue mode by OVH itself (it reboots the instance and returns a
  password); a VPS rescue takes no key, so `ump server rescue` refuses the
  provider's way for a VPS and says so (the direct way, a kexec from the
  running system that lets UMP in as root, works). `ump server reinstall`
  still works on a VPS: OVH rebuilds it with its newest stock Debian
  image and UMP's key, and ump continues from there; the whole reinstall
  is unattended and takes a few minutes longer. Rescue stays the boot
  option on OVH dedicated servers until `ump server unrescue`; `reinstall`
  does that for you.
- Reverse DNS: OVH requires the name to resolve to the address before it
  accepts the entry, so create the forward record first.
- `ump server show` adds, for a dedicated server, the boot option, the
  addresses, the service dates and the hardware; for an instance, the
  flavor, image, addresses, SSH key and project.

### Manual servers (`manual`)

Not a provider at all, but the way machines no provider lists get into
the inventory: `ump server add NAME --ipv4 ADDRESS [--ipv6 ...]
[--location ...] [--product ...] [--kind bare-metal|cloud] [--note ...]`
puts a server into the [state](state.md) (`manual:`), and the account
`manual` of this type lists them next to the others, with the status
`manual`. `ump server remove NAME` forgets one. Prices and attributes
work as for any server; power control, renaming and reverse DNS do not
exist, and `ump server rescue` and `ump server reinstall` work only
through SSH: when the running system lets UMP in (as root or a sudo
user, with UMP's key or yours), the rescue image is loaded and entered
with kexec. Nothing stands behind that: a failed installation or a
system that does not boot is yours to recover, with a console or the
provider's own means, and ump says so before erasing anything. See
[inventory](inventory.md#servers-added-by-hand).

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
