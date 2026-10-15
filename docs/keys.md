# Keys

SSH keys are how anyone, ump included, gets into an installed system:
no passwords are set, so every [user](users.md) needs at least one
public key. A user's `keys` list holds **key references**, resolved on
your machine when a profile is used; named keys are kept by ump so that
a user can carry several people's keys without files lying around.

```console
$ ump config key list
NAME     TYPE          COMMENT        FINGERPRINT    NOTE
ump      ssh-ed25519   ump@laptop     SHA256:...     UMP's own key (~/.config/ump/staging_ed25519)
oleg     ssh-ed25519   oleg@laptop    SHA256:...
work     ssh-rsa       oleg@work      SHA256:...
```

## UMP's own key

ump has an SSH key pair of its own, generated on first use (by `ump
config key`, or the first `ump server rescue`) at
`~/.config/ump/staging_ed25519`, or at the path set as `staging.key` in
the [configuration](configuration.md#umps-key). It is an ed25519 key
that only you can read, and it is never part of a release: every
installation of ump has its own.

It goes into every rescue system ump boots and, through the key
reference `ump`, to the administrator of every installed system, which
is how ump logs in again afterwards (`ump ssh`, the check after an
installation, the direct way back into the rescue system without a
reboot). `ump config key show` prints the public key and its
fingerprints (`-o json` for scripts); put it in `authorized_keys` of a
machine installed by other means, and `ump server rescue` takes the
direct way in there too.

ump sets that up by itself where it can: when `ump server rescue` or
`ump server reinstall` gets into a running system with one of your keys
(your default keys or the ssh-agent) but UMP's key alone does not open
the account, UMP's public key is appended to that account's
`authorized_keys` over the connection that works, and ump says so. From
then on UMP's key reaches the machine by itself, which is what a team
sharing the [state](state.md) and CI rely on. The same happens after a
provider rebuilt a machine with the keys it was created with
(DigitalOcean). Nothing is changed where UMP's key works already.

## Key references

In a user's `keys`:

| Reference | Meaning |
|---|---|
| `ump` | UMP's own key, above. Give it to the administrator. |
| `local` | the public keys next to your `~/.ssh/id_*` files (`id_ed25519.pub`, `id_rsa.pub`, ...); nothing when there are none |
| `agent` | every key your ssh-agent holds (`ssh-add -L`); an error when it holds none |
| a key name | a key kept under that name with `ump config key add` (below) |
| a path | a file with one key per line, `~` allowed (`~/.ssh/ops.pub`) |
| a key line | `ssh-ed25519 AAAA... comment`, as it stands, inlined |

References are resolved when the profile is used, on the machine that
runs `ump server reinstall`, so `local` and `agent` mean the keys of
whoever runs it, and a named key means what that machine's `keys.yaml`
holds. A user whose references together yield no key stops the
installation before any disk is touched. Duplicate lines are written
once.

## Named keys

Named keys live in `~/.config/ump/keys.yaml`, a mapping from a name to
a public key line:

```yaml
keys:
  oleg: ssh-ed25519 AAAA... oleg@laptop
  work: ssh-rsa AAAA... oleg@work
```

```
ump config key show [-o json|yaml]            # UMP's own public key and its fingerprints
ump config key list                           # ump and the named keys, with type, comment and fingerprint
ump config key add                            # on a terminal: asks for the name and a file, a line or the agent
ump config key add NAME --file FILE           # every key line of the file; a second one becomes NAME-2, ...
ump config key add NAME --line 'ssh-... AAAA... comment'
ump config key add NAME --agent               # what ssh-add -L lists, one name each
ump config key remove NAME
```

A name is lower-case letters, digits, `-`, `_` and `.`; `ump`, `local`
and `agent` are taken. A key line must be a public key (`ssh-ed25519`,
`ssh-rsa`, `ecdsa-sha2-*`, `sk-*`); anything else is refused. The file
is yours: keys from another machine or from your password manager go in
by hand just as well.

## Using keys

```console
$ ump config key add oleg --file ~/.ssh/id_ed25519.pub
$ ump config user create oleg --from ump          # keys: [ump, oleg]
$ ump server reinstall web-01 --ssh-key ~/.ssh/laptop.pub   # one more key for the administrator, this time
```

`ump ssh NAME` offers UMP's key together with your own (the agent's and
the usual `~/.ssh/id_*` files), so a server that knows only UMP's key is
reachable, and one that knows yours works without ump as well:
`ssh ump@ADDRESS`.
