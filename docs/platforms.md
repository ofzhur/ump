# Platforms

What ump works with, beyond its own objects: the providers whose
machines it finds and controls, the operating systems it puts on them,
and later the configuration management that takes over from there.

| Platform | What ump does with it | Page |
|---|---|---|
| **Providers (ISPs)** | lists the machines of every account with their prices, shows, renames, powers them, sets reverse DNS, gets them into a rescue system and reinstalls them: Hetzner Robot, Hetzner Cloud, OVHcloud, DigitalOcean, Linode, Vultr, and servers added by hand | [ISPs](isps.md) |
| **Operating systems** | installs server systems on any of those machines, the same way on all of them: Ubuntu 26.04, 24.04 and 22.04 LTS and Debian 13 and 12 (UMP's prebuilt images), NixOS 25.11 (from NixOS's public servers) | [Operating systems](oses.md) |
| **Configuration management** | (planned) hands an installed machine over to the tool that keeps it configured, with the facts ump knows about it | [Roadmap](roadmap.md) |

A provider account is one of ump's [objects](../README.md#the-objects)
(`ump config isp`), and so is a profile, which names the operating
system (`ump config os`); this section is about what stands behind
them: which providers and systems there are, what each can do, and how
they differ.
