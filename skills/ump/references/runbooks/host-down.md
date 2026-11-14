# A server is down

The server does not answer, did not come back after a reboot or an
installation, or a monitor says it is gone. Gather facts freely; every
action beyond reading is a proposal the human chooses from.

## 1. Facts (Observe)

```sh
ump server list provider/id --refresh         # status at the provider: running, off, cancelled, ...
ump server show provider/id                   # power, rescue armed, contract, traffic
ump server list --diff                        # what changed since the last listing
ump config state locks                        # an installation or rescue holding it
ping -c 3 ADDRESS
ump ssh NAME -- uptime                        # the installed system
ump ssh NAME --staging -- uptime              # a rescue system (fresh host key every boot)
```

Note which answers: nothing, ping only, the provider's rescue system,
the UMP rescue system, the installed system. An installation that ended
with exit 1 or 5 leaves the server in the UMP rescue system.

## 2. Options (propose, do not pick)

Offer the human the options that fit, each with its effect:

| Option | Command | Effect |
|---|---|---|
| wait | none | a provider that is slow to show a change; a reboot under way |
| boot from disk | `ump server unrescue NAME --reboot --yes` | disarms the provider's rescue system and resets; leaves any rescue system |
| reboot | `ump server reboot NAME --wait --yes` | ACPI reboot; a stopped server is started |
| hard reset | `ump server reboot NAME --hard --wait --yes` | cuts the server off; for a hung system |
| look from a rescue system | `ump server rescue NAME --yes -- lsblk -f` | the running system stops; the disks are untouched; then read-only commands only |
| the provider's rescue system | `ump server rescue NAME --via provider --no-image --yes` | stops in the provider's own rescue system, nothing written to the disks |
| the console | the provider's web panel | for the human: a kernel panic or boot loader shows at once |
| reinstall | reinstall-active-server.md or install-fresh-server.md | erases every disk |

A crash of the UMP rescue kernel reboots by itself after 30 seconds
into the system on the disk.

## 3. Execute the approved option

Run only the option the human chose, then check again (step 1). For
reading a broken installed system from a rescue system, the checks are
in troubleshooting.md, "The installed system does not boot"; all of them
read, none write the disks without a new go.

## 4. Report

What was observed, what was done, what answers now, and a note for the
case or post-mortem: cause if known, what would have told it sooner.
