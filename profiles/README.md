# Published profiles

This directory holds the profile, the disk layouts and the firewalls that
ump publishes: `make upload` puts them under `profiles/` of the storage
(`https://ump.zhurko.de/dl/profiles/` for this project's). The first
`ump config os`, `disk-layout` or `firewall` command downloads them into
`~/.config/ump/profiles/`, where they are yours to edit; a profile that
is not there is still fetched from the storage by name, so `ump server
reinstall NAME` works with the published `ubuntu-26.04` out of the box.

| File | What it holds | Documentation |
|---|---|---|
| [`ubuntu-26.04.yaml`](ubuntu-26.04.yaml), [`ubuntu-24.04.yaml`](ubuntu-24.04.yaml), [`ubuntu-22.04.yaml`](ubuntu-22.04.yaml) | the published profiles, `ubuntu-26.04` the default: Ubuntu 26.04 on every disk as one linear volume group, the sudo user `ump` with your keys, ICMP and SSH open | [OS profiles](../docs/profiles.md) |
| [`layouts.yaml`](layouts.yaml) | the named disk layouts `lvm-linear`, `lvm-striped`, `lvm-raid1` and `lvm-example` | [Disk layouts](../docs/disk-layouts.md) |
| [`firewalls.yaml`](firewalls.yaml) | the named firewalls `default`, `web` and `open` | [Firewalls](../docs/firewalls.md) |
| [`users.yaml`](users.yaml) | the named user `ump`, the administrator UMP provides | [Users](../docs/profiles.md#users) |

A change here reaches every ump without a local copy of the file at the
next `make upload`; the installer that consumes the resolved profile is
described in [`install/README.md`](../install/README.md).
