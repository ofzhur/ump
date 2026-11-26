# When a command fails

The output of `reinstall` and `rescue` names each step, so the last line
printed says where it stopped. Read it before anything else. The full
guide is docs/troubleshooting.md of the ump repository.

## By exit code

| Code | Signature | Next step |
|---|---|---|
| 1 | `the installation on NAME failed (exit status N, see above); the server stays in the UMP rescue system` | read the installer's `==>` lines above; look with `ump ssh NAME --staging -- lsblk -f` (read-only); fix the profile or layout (a Change, with the human), then a new dry-run and approval |
| 1 | `could not fetch the installer from ...` | the server cannot reach the storage: its network or the storage URL (`staging.image`, `--storage`) |
| 1 | a reference matched nothing, or several without a terminal | `ump server list` for the right `provider/id`, or `--account` |
| 2 | `the installer refused the specification or the machine ... nothing was changed` | the plan does not fit the machine: a layout whose targets match no disk, too few disks for a RAID level, firmware without the layout's `bios`/`efi` part |
| 2 | a missing `ansible.cfg`, `inventory/` or playbook; a playbook that does not parse; a hostname that is a group; `--hosts` matching nothing; a host file ump did not write | fix the repository (a commit) or run from its root; `--no-provision` installs without it |
| 2 | a private address in use, or one another ump gave meanwhile | `ump config network list`; pick another with the human |
| 3 | no terminal and no `--yes` | the command needs a go: get it, then add `--yes` |
| 3 | a lock: another ump installs or rescues the server, or provisions the fleet | `ump config state locks`; wait; `unlock` only when that ump died, with the human's go |
| 3 | `rescue` refused at DigitalOcean, Vultr or a manual server | there is no provider rescue system to fall back on; `--force` is the human's decision |
| 3 | `kexec-into-rescue.sh` reports too little memory | the server keeps running what it ran; the provider's rescue system (`--via provider`) may have more |
| 4 | an account could not be listed, or the provider refused a call | `ump config isp test NAME`; credentials and API access are the human's |
| 5 | `Waiting for the UMP rescue system ...` ran out | the jump happened and nothing answers: network driver, kernel hang; host-down.md, docs/troubleshooting.md "The rescue system does not answer" |
| 5 | `Waiting for SSH on ...` after the reboot ran out | the new system does not answer: host-down.md; from a rescue system check the network configuration, the firewall, the boot loader (docs/troubleshooting.md "The installed system does not boot") |
| 5 | no root access with `--via direct` | the running system does not let UMP in; without `--via direct` ump goes through the provider's rescue system |
| 6 | `provisioning failed on HOST (failed\|unreachable)` | the server is installed and recorded; fix what Ansible reports, then `ump provision --hosts HOST` |
| 6 | the record could not be written (ump prints it for `state.yaml`) | hand the printed record to the human; nothing was provisioned |

## Getting a server back to a known state

- **The system on its disk**: `ump server unrescue NAME --reboot --yes`.
- **The provider's rescue system**: `ump server rescue NAME --via
  provider --no-image --yes`; nothing is written to the disks.
- **A hard reset**: `ump server reboot NAME --hard --yes`.
- **The console**: the provider's web panel, for the human.

Each is a Change: propose it, with what it does to what the server runs.

## What not to do

- Do not debug by writing to the disks of a server that failed to boot
  without a go: they are as the installer left them, and the evidence is
  there.
- Do not retry a failed reinstall in a loop: the second failure says
  nothing the first did not, and each run is a reboot.
- Do not edit `state.yaml` or a host file to make a check pass.
