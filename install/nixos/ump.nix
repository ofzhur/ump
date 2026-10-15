# What UMP set up on this machine, as a NixOS module. ump server reinstall
# puts it at /etc/nixos/ump/default.nix, next to the data it reads:
#
#   spec.json       the resolved profile (users and keys, kernel switches,
#                   time zone, firewall, interface naming)
#   facts.json      what the installer found on the machine (interfaces and
#                   their MAC addresses, the uplink's addresses, the boot
#                   disks, md arrays)
#   nftables.conf   the firewall ruleset, the same UMP writes on Ubuntu
#
# configuration.nix imports this module and every userdata module of the
# profile (/etc/nixos/userdata/*.nix, in order), and NixOS merges them:
# lists add up (packages, groups, kernel parameters), and the settings set
# here with lib.mkDefault yield to the userdata's. The administrator, its
# keys and sshd are set plainly, so a userdata module changes them only
# with lib.mkForce. A reinstall replaces this directory; change the machine
# in configuration.nix or the userdata modules, then nixos-rebuild switch.
{ config, lib, pkgs, ... }:

let
  spec = builtins.fromJSON (builtins.readFile ./spec.json);
  facts = builtins.fromJSON (builtins.readFile ./facts.json);

  labels = lib.splitString "." spec.hostname;
  domain = lib.concatStringsSep "." (lib.tail labels);

  rootListed = lib.any (u: u.name == "root") spec.users;
  people = lib.filter (u: u.name != "root") spec.users;

  # a user's shell is a path in the profile (/bin/bash); NixOS wants the
  # package, so the name picks it: bash, zsh, fish, else bash
  shellOf = u:
    let name = baseNameOf (u.shell or "/bin/bash");
    in if name == "zsh" then pkgs.zsh
    else if name == "fish" then pkgs.fish
    else pkgs.bashInteractive;
  usesShell = name: lib.any (u: baseNameOf (u.shell or "") == name) people;

  ipv6 = spec.kernel.ipv6 or false;
  drop = (spec.firewall.policy or "accept") == "drop";
in
{
  # --- the machine ---------------------------------------------------------

  networking.hostName = lib.head labels;
  networking.domain = lib.mkIf (domain != "") domain;
  time.timeZone = lib.mkDefault spec.timezone;
  boot.kernelParams = facts.kernel_params;
  networking.enableIPv6 = lib.mkDefault ipv6;
  system.stateVersion = lib.mkDefault facts.state_version;
  nix.settings.experimental-features = lib.mkDefault [ "nix-command" "flakes" ];

  # --- the boot loader: GRUB, on every disk with a BIOS boot partition and
  # in the EFI system partition (as the removable path, so no firmware
  # variable needs writing) ---------------------------------------------------

  boot.loader.grub = {
    enable = true;
    devices = if facts.bios_disks == [ ] then [ "nodev" ] else facts.bios_disks;
    efiSupport = facts.efi;
    efiInstallAsRemovable = facts.efi;
  };
  boot.loader.efi.efiSysMountPoint = lib.mkIf facts.efi "/boot/efi";
  boot.swraid = lib.mkIf facts.md {
    enable = true;
    mdadmConf = facts.mdadm_conf;
  };
  # the swap volumes of the layout (nixos-generate-config sees only those
  # in use, and the installer does not use them)
  swapDevices = map (u: { device = "/dev/disk/by-uuid/${u}"; }) facts.swaps;
  # logical volumes with raid levels need the device-mapper raid targets
  boot.initrd.kernelModules = lib.optionals facts.lvm_raid [ "dm-raid" "raid0" "raid1" "raid10" "raid456" ];

  # --- the network: the interfaces by MAC address, renamed to ext01, int01,
  # ... unless the profile keeps the kernel's names; the uplink with the
  # addresses the machine had, the others without ----------------------------

  networking.useDHCP = false;
  networking.useNetworkd = true;
  systemd.network.enable = true;
  systemd.network.links = lib.mkIf facts.rename (lib.listToAttrs (map (i: {
    name = "10-ump-${i.name}";
    value = {
      matchConfig.PermanentMACAddress = i.mac;
      linkConfig.Name = i.name;
    };
  }) facts.interfaces));
  systemd.network.networks = lib.listToAttrs (map (i:
    let uplink = i.mac == facts.uplink_mac;
    in {
      name = (if uplink then "10-ump-" else "20-ump-") + i.name;
      value = {
        matchConfig.MACAddress = i.mac;
        address = lib.optionals uplink (lib.optional (facts.ip4 != "") facts.ip4 ++ lib.optional (ipv6 && facts.ip6 != "") facts.ip6);
        routes = lib.optionals uplink (lib.optional (facts.gw4 != "") { Gateway = facts.gw4; GatewayOnLink = true; }
          ++ lib.optional (ipv6 && facts.gw6 != "") { Gateway = facts.gw6; GatewayOnLink = true; });
        dns = lib.optionals uplink facts.dns;
        DHCP = if uplink && facts.ip4 == "" && facts.ip6 == "" then "ipv4" else "no";
        networkConfig = {
          LinkLocalAddressing = if ipv6 then "ipv6" else "no";
          IPv6AcceptRA = false;
        };
        linkConfig.RequiredForOnline = if uplink then "routable" else "no";
      };
    }) facts.interfaces);

  # --- the users: the administrator and the others from the profile, with
  # their keys; sudo without a password, since no passwords are set ----------

  users.users = lib.listToAttrs (map (u: {
    name = u.name;
    value = {
      isNormalUser = true;
      shell = shellOf u;
      extraGroups = (u.groups or [ ]) ++ lib.optional (u.sudo or false) "wheel";
      openssh.authorizedKeys.keys = u.keys;
    };
  }) people) // lib.optionalAttrs rootListed {
    root.openssh.authorizedKeys.keys = (lib.findFirst (u: u.name == "root") { keys = [ ]; } spec.users).keys;
  };
  users.groups = lib.genAttrs (lib.unique (lib.concatMap (u: u.groups or [ ]) people)) (_: { });
  security.sudo.wheelNeedsPassword = false;
  programs.zsh.enable = lib.mkIf (usesShell "zsh") true;
  programs.fish.enable = lib.mkIf (usesShell "fish") true;

  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = if rootListed then "prohibit-password" else "no";
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
    };
  };

  # --- the firewall: the ruleset UMP writes on every system (input policy,
  # the same list for connections forwarded in from outside, so ports that
  # containers publish are held to it too) ------------------------------------

  networking.firewall.enable = lib.mkDefault false;
  networking.nftables = lib.mkIf drop {
    enable = true;
    flushRuleset = false;
    ruleset = builtins.readFile ./nftables.conf;
  };

  # --- what an operator wants on every server ------------------------------

  environment.systemPackages = with pkgs; [
    htop atop iotop sysstat iftop tcpdump mtr dnsutils ethtool lsof strace
    curl wget jq rsync git vim tmux tree file unzip ncdu
    pciutils usbutils smartmontools nvme-cli parted gptfdisk lvm2 mdadm nftables
  ];
}
