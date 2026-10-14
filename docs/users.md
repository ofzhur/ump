# Users

A user is an account of an installed system: its name, whether it has
sudo, its groups, its shell and the SSH keys that open it. Users are
objects of their own, so the same user goes into any number of
[profiles](profiles.md) by name and is changed in one place; a profile
may also carry users inline for what belongs to it alone.

```console
$ ump config user list
NAME   SUDO   SHELL      KEYS         STATE   DESCRIPTION
ump    yes    /bin/bash  ump, local   ok      the administrator UMP provides
```

## Where users live

Named users are the entries of `users.yaml` in the profiles directory,
`~/.config/ump/profiles/users.yaml`; the first use downloads the
published [`users.yaml`](../profiles/users.yaml) there (see
[profiles](profiles.md#where-profiles-live)). A profile lists the names
it wants in its `users` field, inline users as mappings, or both:

```yaml
# users.yaml
users:
  ump:
    description: the administrator UMP provides
    sudo: true
    keys: [ump, local]
```

```yaml
# in a profile
users:
  - ump                       # by name, from users.yaml
  - ops                       # yours, in users.yaml
  - name: deploy              # inline: only this profile has it
    groups: [www-data]
    keys: [deploy]
```

`ump config os update` replaces the published entry (`ump`) and keeps
the users you added; an entry of yours under the published name is
replaced too, so give yours their own names.

## Fields

```yaml
name: ops                     # inline users only; in users.yaml the entry's key is the name
description: ...              # optional; shown by ump config user list
sudo: true                    # member of sudo without a password (no passwords are set)
groups: [adm, docker]         # optional; created when missing
shell: /bin/bash              # optional; /bin/bash by default
keys: [ump, local, oleg]      # at least one key reference (see Keys)
```

| Field | Meaning |
|---|---|
| `name` | Lower-case letters, digits, `_` and `-`, up to 32 characters, starting with a letter or `_`. `root` is allowed and special (below). |
| `description` | One line, shown by `ump config user list`. |
| `sudo` | `true` makes the user a member of `sudo` with a `NOPASSWD` drop-in (`/etc/sudoers.d/90-ump-NAME`), since no passwords are set. |
| `groups` | Extra groups the user joins; a group that does not exist is created. |
| `shell` | A path. Default `/bin/bash`, which the image ships with a colored prompt and completion; another shell (zsh, fish) needs its package in the profile's `packages`, or comes with your configuration management. |
| `keys` | [Key references](keys.md): `ump`, `local`, `agent`, a key name, a file, or a key line. Every user needs at least one: without a password, a user without keys could not log in. |

## The administrator and root

No passwords are set on an installed system, so SSH is the only way in,
and one user must be able to administer the system: the first user with
`sudo: true` is the **administrator**, the user ump logs in as
afterwards (`ump ssh NAME` without `--user`, the check after an
installation, the direct way back into the rescue system) and the one
you use day to day. Give it UMP's key (`ump`), or ump cannot reach the
server again. `root` counts as the administrator only when no user has
sudo and root is listed with keys.

Root cannot log in over SSH unless a profile lists `root` as a user;
then it logs in with keys only, never with a password. Root remains the
user of the rescue system, which is how ump installs the machine.

The published user is `ump`: sudo, bash, `keys: [ump, local]` (UMP's own
key and the public keys next to your `~/.ssh/id_*` files). The published
profiles list it alone; add yours next to it, or make a profile of your
own with other users.

## Managing users

```
ump config user                               # = list (alias: u)
ump config user list                          # the users, with sudo, shell and keys
ump config user get NAME                      # print one
ump config user create [NAME] [--from NAME]   # copy an entry (default: ump), open the file in your editor; asks on a terminal
ump config user edit [NAME]                   # your editor on users.yaml, then a check of every user
ump config user delete NAME [--yes]
```

`create` and `delete` edit `users.yaml` in place; its comments survive.
`edit` checks every user when the editor returns (the name, the groups,
the shell, that there are key references) and keeps the file either
way, naming the problem. `ump config os create` defines a user on the
spot when you name one it does not know, and keeps it in `users.yaml`.

## Using a user

```console
$ ump config user create oleg --from ump      # then: your name, your keys
$ ump config os create web                    # Users [ump]: ump, oleg
$ ump server reinstall web-01 --profile web
$ ump ssh web-01 --user oleg
```

`ump server reinstall --ssh-key FILE` adds the keys of a file to the
administrator for one installation, and `--set 'users[+]={name: ops,
sudo: true, keys: [local]}'` adds an inline user to it. On the
installed system, `/etc/ump/install` records the administrator
(`admin=ump`), and `/etc/ump/spec.json` every user with the key lines it
got; `~USER/.ssh/authorized_keys` holds them. UMP writes users once, at
installation: later changes are yours, or your configuration
management's.

## Examples

An operations team with sudo and a deployment account without:

```yaml
users:
  ops:
    description: the operations team
    sudo: true
    keys: [ump, alice, bob]
  deploy:
    description: what the CI deploys with
    groups: [www-data]
    keys: [deploy]
```

A profile that also lets root in, with UMP's key only:

```yaml
users:
  - ump
  - name: root
    keys: [ump]
```
