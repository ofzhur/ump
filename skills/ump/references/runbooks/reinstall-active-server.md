# Reinstall a server that holds data or serves

For a server that runs anything someone uses, or whose disks hold
anything not kept elsewhere. A reinstall erases every disk; there is no
undo. The agent prepares, the human runs the reinstall.

## 1. Gather (Observe)

```sh
ump server show provider/id
ump server list provider/id -o wide           # profile, layout, groups, OS, installed_at
ump ssh NAME -- df -h
ump ssh NAME -- lsblk
ump ssh NAME -- systemctl list-units --type=service --state=running
```

Write down what runs, what data is on which disk, and where it goes.

## 2. Prepare (with the human, step by step)

- **Drain**: take the server out of what sends it work (a load
  balancer, DNS, a cluster), through the repository's playbooks or a
  runbook of the infrastructure. Each step is a Change with its own go.
- **Back up**: every piece of data that must survive, to a place that is
  not this server, with a restore of a sample tested and noted.
- **Plan**: the profile and groups as in install-fresh-server.md step 2.
  A dry-run brings the server into the rescue system and stops what it
  serves, so it belongs after the drain, with the human's go.

## 3. Hand over

Give the human, in one message: what was drained and backed up (with
where and how the restore was tested), the dry-run's plan, and the exact
command:

```sh
ump server reinstall provider/id --profile PROFILE --group GROUP --hosts HOSTNAME
```

on a terminal (ump asks there), or with `--yes` if they choose. Do not
run it yourself.

## 4. After the human ran it

Verify as in install-fresh-server.md step 7, restore the data through
the repository or the runbook, undrain, and report what is back and
what is not.
