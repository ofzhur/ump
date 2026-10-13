# Configuration

ump keeps its configuration in YAML files under `~/.config/ump/`. The
provider accounts are written by `ump config isp`, the profiles, layouts
and firewalls edited through `ump config os|disk-layout|firewall`, and
the rest is optional.

## Where things live

```
~/.config/ump/
├── config.yaml              accounts, storage, UMP's key, per-server settings, output default
├── profiles/
│   ├── default.yaml         the default profile (downloaded on first use, then yours)
│   ├── NAME.yaml            your profiles
│   ├── layouts.yaml         the named disk layouts
│   ├── firewalls.yaml       the named firewalls
│   └── .published/          the three files as published, for ump config os update to compare
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
carries secrets. `ump config path` prints the file in use.

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

storage: https://ump.zhurko.de         # the base URL everything a server fetches comes from (the default)

updates: true                          # once a day, after a command, say when a newer ump is published (the default)

staging:
  key: ~/.config/ump/staging_ed25519   # UMP's own SSH key (the default; generated on first use)
  image: https://ump.zhurko.de/rescue  # the rescue image alone, when it lives elsewhere than <storage>/rescue

servers:                               # written by ump server set
  hetzner-cloud/234567:
    profile: web
```

| Section | Meaning |
|---|---|
| `defaults.output` | The format `ump server list` uses without `-o`: `table` (the default), `wide`, `json`, `yaml` or `name`. |
| `accounts` | One entry per [provider account](providers.md): the `type` and the type's settings, each a plain value or a secret reference. `ump config isp types` lists the settings. |
| `storage` | The base URL of the [storage](#storage), without a trailing slash. Default `https://ump.zhurko.de`. |
| `staging.key` | The path of UMP's own private SSH key; the public key is next to it with `.pub`. Default `staging_ed25519` in the configuration directory. |
| `staging.image` | The base URL of the rescue image, when it is not `<storage>/rescue`. `ump server rescue --image` overrides it for one run. |
| `updates` | `false` turns the daily release check off. Once a day, after a successful command, ump reads `bin/latest/VERSION` on the storage and says when a newer release is published, with the one-liner that installs it; `UMP_NO_UPDATE_CHECK=1` skips the check for one run, and a development build never checks. |
| `servers` | Per-server settings, keyed by `provider/id`: the `profile` the server is reinstalled with (`ump server set NAME --profile NAME`, `--clear` removes it). |

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
there too. The default is this project's, `https://ump.zhurko.de`. A
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
[profile](profiles.md#keys)) and what `ump ssh` offers next to your own
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
`NAME.yaml`, the [disk layouts](disk-layouts.md) in `layouts.yaml` and the
[firewalls](firewalls.md) in `firewalls.yaml`. The first command that
needs the directory creates it and downloads the published
`default.yaml`, `layouts.yaml` and `firewalls.yaml` from the storage; from
then on the files are yours and never overwritten. A profile, layout or
firewall is looked up locally first, then on the storage, so the
published ones remain available until you shadow them with a file of the
same name. `ump config os update` fetches the three published files
again: a file still as published is replaced, one you edited is kept and
the new published version is put next to it in `.published/` for you to
compare; profiles under other names are never touched.
