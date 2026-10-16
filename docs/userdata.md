# Userdata

Userdata is what runs in a freshly installed system before its first
boot: a script that installs packages, drops files, enables services,
or anything else every server of a kind needs. Userdata entries are
objects of their own, so the same entry goes into any number of
[profiles](profiles.md) by name and is changed in one place; a profile
may also carry entries inline for what belongs to it alone.

```console
$ ump config userdata list
NAME     TYPE   LINES   STATE   DESCRIPTION
debian   bash   8       ok      what every Debian gets; nothing yet
nixos    nix    12      ok      what every NixOS gets; nothing yet
ubuntu   bash   7       ok      what every Ubuntu gets; nothing yet
```

An entry is a bash script (type `bash`) for Ubuntu, or a NixOS module
(type `nix`) for [NixOS](oses.md#nixos), where it is merged with UMP's own
module instead of run. A profile takes the type its system takes, and
ump refuses a mix before it touches the machine. Other systems that take
a declarative configuration (cloud-init, Talos) may follow as other
types. The rest of this page is about bash userdata; [NixOS](oses.md#userdata-on-nixos)
describes the modules.

## Where userdata lives

Named entries are the mapping of `userdata.yaml` in the profiles
directory, `~/.config/ump/profiles/userdata.yaml`; the first use
downloads the published [`userdata.yaml`](../profiles/userdata.yaml)
there (see [profiles](profiles.md#where-profiles-live)). A profile
lists the names it wants in its `userdata` field, in the order they
run, inline entries as mappings, or both:

```yaml
# userdata.yaml
userdata:
  ubuntu:
    description: what every Ubuntu gets; nothing yet
    type: bash
    script: |
      #!/bin/bash
      set -euo pipefail
      echo "userdata ubuntu: nothing to do on $(hostname)"
  web:
    description: nginx and certbot
    script: |
      #!/bin/bash
      set -euo pipefail
      apt-get install -y --no-install-recommends nginx certbot
      systemctl enable nginx
```

```yaml
# in a profile
userdata:
  - ubuntu                    # by name, from userdata.yaml
  - web                       # yours, in userdata.yaml
  - name: monitoring          # inline: only this profile has it
    script: |
      apt-get install -y prometheus-node-exporter
```

`ump config os update` replaces the published entry (`ubuntu`) and
keeps the entries you added; an entry of yours under the published
name is replaced too, so give yours their own names. The published
`ubuntu` does nothing but say so: it is the place for what every
Ubuntu of yours should get, and the published profiles list it.

## Fields

```yaml
name: web                     # inline entries only; in userdata.yaml the entry's key is the name
description: ...              # optional; shown by ump config userdata list
type: bash                    # optional; bash (the default) or nix (a NixOS module, docs/oses.md#nixos)
script: |                     # the script itself
  #!/bin/bash
  set -euo pipefail
  apt-get install -y nginx
```

| Field | Meaning |
|---|---|
| `name` | Lower-case letters, digits, `.`, `_` and `-`, starting with a letter or digit. In `userdata.yaml` the entry's key is the name. |
| `description` | One line, shown by `ump config userdata list`. |
| `type` | `bash`: a script run by bash, for Ubuntu (the default). `nix`: a NixOS module, for NixOS, merged with UMP's own (see [NixOS](oses.md#userdata-on-nixos)). |
| `script` | The script or the module, not empty. A shebang line is not needed, bash runs it either way. |

## How it runs

The installer writes every entry of the profile to
`/etc/ump/userdata/NN-NAME.sh` in the new system (`01-ubuntu.sh`,
`02-web.sh`, ...; mode 700) and runs them there in that order, as
root, in a chroot of the installed system while the server is still in
UMP's rescue system: the disks hold the new system, the network is the
rescue system's, no service of the new system is running and none is
started (`policy-rc.d` says so during the run, so `apt-get install`
does not start what it installs; `systemctl enable` works). The
package lists are those of the image; run `apt-get update` first when
the script needs the archive's latest.

The environment names the context: `UMP_USERDATA` (the entry's name),
`UMP_HOSTNAME`, `UMP_OS`, `UMP_PROFILE` and `UMP_ADMIN` (the
administrator's user name). The output of every script is kept in
`/var/log/ump-userdata.log` on the new system, one section per entry;
a failing script (a non-zero exit) fails the installation with its
output shown, and the server stays in the rescue system for a look.
`/etc/ump/install` records the names that ran (`userdata=ubuntu,web`).

What does not fit a script that runs once at installation, such as
keeping a system configured over time, belongs to the provision step
after the first boot (`provision` in the profile) or to your
configuration management.

## Managing userdata

```
ump config userdata list                          # the entries, with type, lines and whether they are valid
ump config userdata get NAME                      # print one
ump config userdata create [NAME] [--from NAME]   # copy an entry (default: ubuntu), open the file in your editor; asks on a terminal
ump config userdata edit [NAME]                   # your editor on userdata.yaml, then a check of every entry
ump config userdata delete NAME [--yes]
```

`create` and `delete` edit `userdata.yaml` in place; its comments
survive. `edit` checks every entry when the editor returns and keeps
the file either way, naming the problem. The command is also `ud`,
and the verbs have the short forms of every `ump config` entity (`ump
c ud ls`); see the [CLI reference](cli.md#ump-config).

## Using userdata

A profile names the entries in `userdata`, in order; `ump server
reinstall --set 'userdata=[ubuntu, web]'` overrides the list for one
installation, and `--set 'userdata[+]={name: x, script: "..."}'`
appends an inline entry. `ump server reinstall --dry-run` lists what
would run. A profile without `userdata` runs nothing.
