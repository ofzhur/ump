# Configuration

ump keeps its configuration in YAML files under `~/.config/ump/`. The
provider accounts are written by `ump config isp`, the profiles, layouts
and firewalls edited through `ump config os|disk-layout|firewall`, and
the rest is optional.

## Where things live

```
~/.config/ump/
├── backend.yaml             the state backend, when it is not this directory (ump config state setup)
├── config.yaml              accounts, storage, UMP's key, output default
├── profiles/
│   ├── ubuntu-26.04.yaml    the published profiles (downloaded on first use; ump config os update replaces them)
│   ├── ubuntu-24.04.yaml
│   ├── ubuntu-22.04.yaml
│   ├── debian-13.yaml
│   ├── debian-12.yaml
│   ├── nixos-25.11.yaml
│   ├── NAME.yaml            your profiles
│   ├── layouts.yaml         the named disk layouts: the published ones and yours
│   ├── firewalls.yaml       the named firewalls
│   └── users.yaml           the named users
├── keys.yaml                the named public keys (ump config key add)
├── state.yaml               what ump knows about servers beyond the providers: profile, price, attributes, manual servers, installations
├── cache/                   the listing cache (cache_ttl, ump server list --diff, --cached)
├── secrets/                 the secret files the account wizard writes (mode 0600)
├── staging_ed25519          UMP's own SSH key, generated on first use
├── staging_ed25519.pub
└── update-check.json        when the storage was last asked for a newer release, and what it said

~/.cache/ump/listings/       snapshots of server listings, for ump server list --diff; safe to delete
```

`$XDG_CONFIG_HOME/ump` and `$XDG_CACHE_HOME/ump` are used when those
variables are set. `--config PATH` or `UMP_CONFIG=PATH` selects another
configuration file (a directory means `config.yaml` inside it); a team can
keep its accounts, as references, in a repository and point ump at it.
The profiles directory, the secret files and UMP's key stay under your own
`~/.config/ump/` whatever `--config` says, so a shared configuration never
carries secrets. `ump config isp view` names the file in use.

## `config.yaml`

Everything in it is optional; a missing file is an empty configuration.

```yaml
defaults:
  output: table                        # the output format of ump server list: table, wide, json, yaml or name

accounts:                              # written by ump config isp; see docs/providers.md
  hrobot:
    type: hetzner-robot
    login: "#ws+AbCdEfGh"
    password: { file: ~/.config/ump/secrets/hrobot.password }
  hcloud:
    type: hetzner-cloud
    token: { env: HCLOUD_TOKEN }
  ovh:
    type: ovh
    endpoint: ovh-eu                   # ovh-eu | ovh-ca | ovh-us | a URL
    application_key: AbCdEfGh
    application_secret: { command: "pass show ovh/app-secret" }
    consumer_key: { file: ~/.config/ump/secrets/ovh.consumer_key }

storage: https://ump.zhurko.de/dl         # the base URL everything a server fetches comes from (the default)

updates: true                          # once a day, after a command, say when a newer ump is published (the default)
default_profile: ubuntu-26.04          # what ump server reinstall takes without --profile and without a profile
                                       # set for the server; ump config os default NAME changes it
wait: 15m                              # how long ump waits for a machine to answer (the default)
cache_ttl: 5m                          # how long a server listing is served from the cache (the default)

staging:
  key: ~/.config/ump/staging_ed25519   # UMP's own SSH key (the default; generated on first use)
  image: https://ump.zhurko.de/dl/rescue  # the rescue image alone, when it lives elsewhere than <storage>/rescue
  keys: [oleg]                         # your keys in every rescue system, next to UMP's (ump config key rescue)

```

Which state backend holds all this is not in `config.yaml` but in
`backend.yaml` next to it, or in `UMP_STATE`; see [the state
backend](state.md).

| Section | Meaning |
|---|---|
| `defaults.output` | The format `ump server list` uses without `-o`: `table` (the default), `wide`, `json`, `yaml` or `name`. |
| `accounts` | One entry per [provider account](providers.md): the `type` and the type's settings, each a plain value or a secret reference. `ump config isp types` lists the settings. |
| `storage` | The base URL of the [storage](#storage), without a trailing slash. Default `https://ump.zhurko.de/dl`. |
| `staging.key` | The path of UMP's own private SSH key; the public key is next to it with `.pub`. Default `staging_ed25519` in the configuration directory. |
| `staging.keys` | Key references (a named key, `local`, `agent`, a file, a key line) that go into every rescue system next to UMP's key and are registered at the providers that keep keys; `ump config key rescue` sets them. Empty: only UMP's key, with a warning before each rescue and reinstall. |
| `staging.image` | The base URL of the rescue image, when it is not `<storage>/rescue`. `ump server rescue --image` overrides it for one run. |
| `wait` | How long ump waits for a machine to answer: after a reboot with `--wait`, for the UMP rescue system after the jump into it, and after a provider reinstalled a machine with UMP's key. A Go duration (`20m`, `1h`); default `15m`, which covers slow providers and large machines while a kexec usually answers within a minute. Every wait prints its limit and marks each minute waited (`Waiting for ... (up to 15m) .....1m.....`); `UMP_WAIT=30m` sets it for one run. A provider whose reinstall takes longer (a Vultr bare metal server) waits its own longer limit unless `wait` is set. |
| `cache_ttl` | How long a server listing is served from the cache before the providers are asked again, by `ump server list` and every command that looks a server up. A Go duration (`1m`, `30s`); default `5m`; `0` asks the providers every time. A command that changes a server makes its account's listings ask the provider again; `--refresh` does for one listing, `UMP_CACHE_TTL=0` for one run. See [the listing cache](cli.md#the-listing-cache-and---diff). |
| `updates` | `false` turns the daily release check off. Once a day, after a successful command, ump reads `bin/latest/VERSION` on the storage and says when a newer release is published, with the one-liner that installs it; `UMP_NO_UPDATE_CHECK=1` skips the check for one run, and a development build never checks. |

Sections this version of ump does not know are kept when the file is
saved. The file is written with mode 0600, atomically.

### Secret references

Any setting of an account can be a reference instead of the value:

| Form | Source |
|---|---|
| `{ env: NAME }` | the environment variable |
| `{ file: PATH }` | the content of a file, trailing newline stripped; `~` is expanded |
| `{ command: "..." }` | the standard output of a command, run with `sh -c` (`cmd /C` on Windows): `pass show ...`, `op read ...`, `sops -d ...`, `vault kv get ...` |

The wizard writes `file` references to `~/.config/ump/secrets/ACCOUNT.KEY`
and `ump config isp remove` deletes those files with the account.
Plaintext values work too; `ump config isp view` shows them as
`<redacted>` and references as they are. On the command line (`ump config
isp add --set KEY=VALUE`) the same references are `env:NAME`, `file:PATH`
and `command:CMD`.

## Storage

The storage is the one place a server fetches from during a rescue or an
installation: the rescue image (`rescue/`), the entry script, the
installer (`install/install.sh`), the published profiles (`profiles/`)
and the OS images (`os/<os>/<arch>/`); `get.sh` and the ump binaries live
there too. The default is this project's, `https://ump.zhurko.de/dl`. A
storage of your own is any web server or object store that answers plain
`GET` requests and that the servers can reach; [the rescue image's
README](../images/rescue/README.md#your-own-storage) lists what to serve,
and `make upload` produces exactly that layout. Set `storage:` here, or
`--storage URL` on `ump server reinstall` for one installation (the
rescue image moves along unless `staging.image` pins it).

## UMP's key

ump has an SSH key pair of its own, generated on first use (`ump config
key`, or the first `ump server rescue` or `reinstall`) at `staging.key`.
It is what rescue systems are booted with and reached by, what the
administrator of an installed system gets (the `ump` key reference in a
[user](keys.md)) and what `ump ssh` offers next to your own
keys, so ump can reach its machines whatever personal keys are around.

```console
$ ump config key
Path:         ~/.config/ump/staging_ed25519
Public key:   ssh-ed25519 AAAAC3... ump@laptop
Fingerprint:  SHA256:... (MD5 ...)
```

`-o json` prints the same for scripts. Put the public key wherever ump
should be able to log in as root, for instance in `authorized_keys` of a
machine installed by other means, so that `ump server rescue` and
`reinstall` can take the direct way in, without a reboot through the
provider.

## The profiles directory

`~/.config/ump/profiles/` holds the [profiles](profiles.md) as
`NAME.yaml`, the [disk layouts](disk-layouts.md) in `layouts.yaml`, the
[firewalls](firewalls.md) in `firewalls.yaml` and the
[users](profiles.md#users) in `users.yaml`. The first command that
needs the directory creates it and downloads the published files from
the storage. The published profiles (`ubuntu-26.04`, `ubuntu-24.04`,
`ubuntu-22.04`, `debian-13`, `debian-12`, `nixos-25.11`) and the published entries of the named files are
templates that `ump config os update` replaces; what you create under
names of your own is yours and never touched. `default_profile` in
`config.yaml` (set with `ump config os default NAME`) names the profile
taken when nothing else says which; `ubuntu-26.04` when unset.
