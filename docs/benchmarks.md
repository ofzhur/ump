# Benchmarks: eight Linux systems on one server

Every operating system ump publishes, installed by `ump server reinstall` on
the same bare-metal server over UEFI, given the same tuning, then measured
with every suite of [ofzhur/benchmarks](https://github.com/ofzhur/benchmarks).
Network and HTTP run on the server's own loopback, so nothing outside the
machine is involved. Every system was installed and measured twice
(October 2026); the tables show the second round, with network and HTTP
averaged over both.

What this is for: the same hardware, the same disk layout, the same
kernel switches and the same sysctls, so what differs is the system
itself (its kernel, compiler, libraries and defaults). It is one server
and one CPU generation, not a ranking for every workload.

## Scores

For each test, each system's result divided by the average of all eight
(inverted where lower is better); a suite's score is the geometric mean of
its tests, and the overall score weighs CPU, memory, disk and HTTP
equally. 100 is the average system; the best of each row is in bold.
Network is shown but left out of the overall score: loopback TCP moved by
up to half between two installs of the same system.

| Suite | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **Overall** | 101.1 | 98.5 | 95.5 | 97.8 | 99.4 | 99.8 | **103.0** | 102.9 |
| CPU | 101.5 | 98.0 | 97.0 | **103.8** | 97.7 | 100.6 | 99.7 | 100.1 |
| Memory | 99.9 | 100.2 | 99.3 | **101.1** | 100.3 | 99.6 | 100.0 | 100.1 |
| Disk | 99.9 | 93.5 | 101.7 | **106.0** | 95.4 | 100.4 | 102.4 | 100.7 |
| Network | 105.8 | **125.8** | 54.9 | 79.8 | 86.7 | 108.0 | 114.8 | 120.6 |
| HTTP | 103.4 | 102.6 | 85.0 | 82.1 | 104.6 | 98.6 | 110.2 | **111.4** |

In short: Arch Linux and CachyOS lead overall, by their HTTP and 7-Zip
results; Ubuntu 26.04 is the best of the image-based systems; Debian 13
has the best CPU, memory and disk scores but trails on HTTP (see
[below](#why-debian-13-trails-on-http)); Ubuntu 22.04, the oldest kernel
and compiler here, is last.

## The server

| | |
|---|---|
| Machine | a Hetzner dedicated server (Server Auction), Helsinki |
| CPU | Intel Core i9-13900: 8 performance and 16 efficient cores, 32 threads, up to 5.6 GHz, 36 MB L3, AVX2 |
| Power | 65 W sustained, 219 W short-term (PL1/PL2, locked by the board) |
| Memory | 64 GB: 2 × 32 GB DDR5-4400 ECC |
| Storage | 2 × Intel D7-P5520 (SSDPF2KX019T1M), 1.92 TB NVMe each |
| Board and firmware | ASUS W680/MB DC, UEFI |
| Network | Intel I225-LM, 1 Gbit/s |
| Layout, every system | `lvm-linear`: one ext4 root over both disks, 3.5 TB; `mitigations=off`, IPv6 off |

## Installation time

Seconds from `ump server reinstall` on the running previous system until
the new one answers over SSH: the kexec into ump's rescue system, a wait
for DNS (see [the notes](#notes)), the installer itself, and the reboot
through the firmware, which takes 66–75 s for every system.

| System | Rescue | DNS wait | Installer | Reboot | Total |
|---|---:|---:|---:|---:|---:|
| Ubuntu 26.04 | 33 | 38 | 34 | 70 | **174** |
| Ubuntu 24.04 | 29 | 38 | 46 | 71 | **183** |
| Ubuntu 22.04 | 29 | 38 | 35 | 66 | **168** |
| Debian 13 | 28 | 38 | 28 | 70 | **164** |
| Debian 12 | 30 | 38 | 40 | 68 | **175** |
| NixOS 25.11 | 30 | 38 | 44 | 71 | **183** |
| Arch Linux | 31 | 38 | 48 | 75 | **192** |
| CachyOS | 30 | 38 | 54 | 72 | **194** |

The image-based systems (Ubuntu, Debian) unpack a prepared root file
system in 28–46 s; NixOS builds from its channel with `nixos-install`
(44 s); Arch Linux and CachyOS run `pacstrap` from their mirrors
(48–54 s). Without the DNS wait every installation finishes in 2 to 2.6
minutes.

## Tuning

The same on every system, applied before the run:

- Connection queues: `somaxconn` and `tcp_max_syn_backlog` 65535,
  `netdev_max_backlog` 16384; local ports 1024–65535, `tcp_tw_reuse=1`,
  `tcp_fin_timeout=15`, no slow start after idle.
- Socket buffers up to 64 MB (`rmem_max`, `wmem_max`, `tcp_rmem`,
  `tcp_wmem`); a conntrack table of 2 M entries, since ump's nftables
  firewall tracks loopback connections too.
- Scheduler autogroup off, so the benchmark's own processes are balanced
  as one workload; NUMA balancing off; `swappiness` 10, `dirty_ratio` 40.
- CPU: the performance governor and EPP on every core (`bench.sh
  --tune`), turbo on. Kernel switches from ump's profiles:
  `mitigations=off`, `ipv6.disable=1`.
- Left as each system ships it: transparent huge pages, the NVMe
  scheduler (none everywhere), read-ahead, the kernel and the compiler
  that builds the benchmark.

| System | Kernel | gcc | Microcode | THP | What was different |
|---|---|---|---|---|---|
| Ubuntu 26.04 | 7.0.0 | 15.2 | 0x133 | madvise | tuned (throughput-performance) already had the CPU at the performance governor and EPP, and NVMe read-ahead at 4 MB. Only the common network and scheduler settings were added; TCP buffers went from a 4 MB to a 64 MB ceiling. |
| Ubuntu 24.04 | 6.8.0 | 13.3 | 0x133 | madvise | Same as 26.04: tuned had the CPU and read-ahead right. The socket buffer ceiling was the kernel's 208 KB, raised to 64 MB with the common set. |
| Ubuntu 22.04 | 5.15.0 | 11.4 | 0x133 | madvise | tuned set the governor and EPP, but its older version leaves NVMe read-ahead at 128 KB (left as is: fio reads with O_DIRECT). Common set applied, buffers from 208 KB. |
| Debian 13 | 6.12.113 | 14.2 | 0x132 | always | tuned had the CPU right. Debian's kernel runs transparent huge pages in always mode (left as the OS ships it). Common set applied. The image now brings intel-microcode; in round one the CPU ran the BIOS's 0x12b. |
| Debian 12 | 6.1.0-54 | 12.2 | 0x132 | always | tuned had the CPU right; THP always, like Debian 13. Scheduler autogroup was already off, so only the network settings changed. Microcode from the image. |
| NixOS 25.11 | 6.12.93 | 14.3 | 0x133 | madvise | No tuned on NixOS: the CPU ran the powersave governor with EPP balance_performance, switched to performance for the run (ump sets it at install now). Also swappiness 60 to 10 and dirty_ratio 20 to 40 to match the others; read-ahead stays 128 KB. Benchmark tools came from nixpkgs. |
| Arch Linux | 7.2.11 | 16.2 | 0x137 | always | ump's install enables tuned, so the CPU was already at performance. Arch's kernel runs THP always. Common set applied. |
| CachyOS | 7.2.11 cachyos-server | 16.2 | 0x137 | madvise | As Arch, plus CachyOS's x86-64-v3 package builds and the linux-cachyos-server kernel. THP madvise. Common set applied. |

## All results

Higher is better unless marked ↓; the best of each row is in bold. Disk
throughput rows use the best single-job run of each system's sweep (see
[the notes](#notes)); network and HTTP are the mean of both rounds.

### CPU

| Test | Unit | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Integer ALU, 1 thread | Gops/s | 15.02 | 15.10 | 15.93 | **15.95** | 15.94 | 15.93 | 15.08 | 15.10 |
| Integer ALU, 32 threads | Gops/s | 303.6 | 288.0 | 302.7 | 298.4 | 301.3 | 305.5 | 304.6 | **308.3** |
| FP64 FMA, 1 thread | GFLOPS | 84.47 | 84.52 | 85.54 | **89.21** | 84.61 | 89.20 | 84.50 | 84.57 |
| FP64 FMA, 32 threads | GFLOPS | 812.4 | 772.3 | 776.2 | **829.8** | 775.1 | 793.9 | 801.6 | 803.2 |
| FP32 FMA, 1 thread | GFLOPS | 168.9 | 169.1 | 169.2 | **178.7** | 169.2 | 178.5 | 169.0 | 169.1 |
| FP32 FMA, 32 threads | GFLOPS | **1,711** | 1,590 | 1,596 | 1,655 | 1,595 | 1,636 | 1,649 | 1,665 |
| SHA-256 (portable C), 1 thread | MB/s | 432.1 | 450.8 | 437.7 | **474.0** | 452.7 | 473.3 | 432.2 | 431.1 |
| SHA-256 (portable C), 32 threads | MB/s | 6,478 | 6,314 | 6,577 | 6,560 | 6,406 | 6,555 | 6,606 | **6,609** |
| Sort, 1 thread | Melem/s | 25.89 | 27.67 | 26.89 | 28.06 | 26.58 | **28.55** | 21.99 | 22.01 |
| Sort, 32 threads | Melem/s | 447.5 | 451.1 | 457.2 | **464.5** | 449.5 | 461.0 | 367.8 | 367.8 |
| sysbench cpu, 1 thread | events/s | 4,440 | 4,561 | 4,557 | **4,808** | 4,541 | 4,465 | 4,484 | 4,460 |
| sysbench cpu, 32 threads | events/s | 72,141 | 75,188 | 74,693 | **76,728** | 74,675 | 70,304 | 72,176 | 73,147 |
| 7-Zip, 1 thread | MIPS | 10,256 | 7,707 | 7,101 | 10,677 | 8,282 | 8,114 | 10,679 | **10,748** |
| 7-Zip, 32 threads | MIPS | 127,671 | 98,770 | 77,791 | 111,398 | 96,803 | 100,949 | 128,292 | **131,201** |
| OpenSSL AES-256-GCM, 1 thread | MB/s | 8,413 | 8,534 | **8,570** | 8,324 | 7,588 | 8,304 | 8,367 | 8,318 |
| OpenSSL AES-256-GCM, 32 threads | MB/s | 91,902 | 90,657 | 91,321 | 92,010 | 84,015 | 92,135 | 92,470 | **92,853** |
| OpenSSL SHA-256, 1 thread | MB/s | 2,679 | 2,544 | 2,548 | 2,684 | 2,543 | 2,687 | **2,688** | 2,687 |
| OpenSSL SHA-256, 32 threads | MB/s | 47,649 | 46,356 | 46,247 | 47,472 | 46,415 | 47,541 | 47,640 | **47,999** |

### Memory

| Test | Unit | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Read bandwidth | GB/s | 69.42 | 67.31 | 65.05 | 68.53 | 68.02 | 68.58 | **69.89** | 69.86 |
| Write bandwidth | GB/s | 66.19 | 66.10 | 64.63 | 65.29 | 66.04 | 65.86 | 66.13 | **66.20** |
| Copy bandwidth | GB/s | **67.29** | 66.72 | 66.37 | 67.09 | 66.85 | 66.88 | 67.21 | 67.25 |
| Triad bandwidth | GB/s | 66.45 | 66.04 | 65.44 | 66.72 | 65.94 | 66.06 | **66.83** | 66.65 |
| Latency L1 | ns ↓ | 0.95 | 0.95 | 0.95 | **0.90** | 0.95 | 0.90 | 0.95 | 0.95 |
| Latency L2 | ns ↓ | 3.03 | 3.03 | 3.03 | **2.87** | 3.03 | 2.87 | 3.03 | 3.03 |
| Latency L3 | ns ↓ | 12.73 | 11.72 | 11.73 | 12.34 | 11.70 | **11.29** | 12.74 | 12.64 |
| Latency DRAM | ns ↓ | 87.89 | 88.79 | 88.86 | 89.67 | 88.65 | 109.7 | **87.69** | 87.96 |

### Disk

| Test | Unit | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Sequential read 1M | MB/s | 5,601 | 5,348 | **5,823** | 5,482 | 5,410 | 5,369 | 5,371 | 5,301 |
| Sequential write 1M | MB/s | 2,026 | **2,100** | 2,063 | 2,022 | 2,049 | 2,055 | 2,054 | 2,042 |
| Random read 4K | IOPS | 568,911 | 494,707 | 572,580 | 567,022 | 533,712 | 564,365 | 568,053 | **579,472** |
| Random write 4K | IOPS | 503,276 | 374,561 | 480,979 | 445,939 | 405,448 | 487,699 | **507,938** | 501,988 |
| Read latency 4K QD1 | us ↓ | 69.00 | 68.30 | 70.10 | **61.70** | 69.50 | 69.90 | 68.80 | 69.80 |
| Write latency 4K QD1 | us ↓ | 9.60 | 10.90 | 9.40 | **7.80** | 9.90 | 9.20 | 8.70 | 8.90 |
| fdatasync'ed writes 4K | IOPS | 98,199 | 108,890 | 109,115 | **125,282** | 100,252 | 105,604 | 107,844 | 100,311 |

### Network

| Test | Unit | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| TCP loopback, one way | Gbit/s | 576.8 | 645.9 | 412.8 | 429.0 | 533.5 | 568.2 | 622.4 | **662.5** |
| TCP loopback, both ways | Gbit/s | 732.3 | **925.3** | 275.3 | 560.0 | 531.3 | 774.9 | 799.7 | 829.2 |

### HTTP

| Test | Unit | Ubuntu 26.04 | Ubuntu 24.04 | Ubuntu 22.04 | Debian 13 | Debian 12 | NixOS 25.11 | Arch Linux | CachyOS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Hello World | req/s | 1,398,394 | 1,353,234 | 1,031,287 | 1,016,782 | 1,434,212 | 1,270,448 | 1,597,034 | **1,622,176** |
| Hello World, 16 pipelined | req/s | 15,191,418 | 14,811,812 | 11,904,422 | 11,790,656 | 15,392,928 | 14,647,832 | 16,542,870 | **16,687,540** |
| JSON, 20 records | req/s | 938,465 | 941,939 | 717,210 | 664,324 | 954,489 | 889,196 | 1,013,878 | **1,034,830** |
| SHA-256 ×1000 per request | req/s | 74,480 | 76,436 | **77,042** | 74,256 | 73,947 | 74,368 | 71,698 | 71,508 |

## Notes

- **CPU and memory are mostly the hardware's.** The benchmark's own
  integer, FP and SHA-256 kernels stay within 6–10 % across the eight;
  sort moves 26–30 % with the gcc version that builds it (11.4 to 16.2).
  The reference tools are each system's own packages: 7-Zip ranges from
  7,101 MIPS single-thread on Ubuntu 22.04, which ships 7-Zip 16.02, to
  10,750 on CachyOS. Memory bandwidth is within 1–7 %; DRAM latency is
  88–89 ns everywhere except one NixOS run at 110 ns (88 in the first
  round).
- **HTTP separates the systems and repeats:** every system landed within
  4 % of its first-round rate. CachyOS and Arch Linux lead; Debian 12,
  Ubuntu 26.04 and 24.04 reach 83–92 % of CachyOS, NixOS 78–88 %, and
  Debian 13 and Ubuntu 22.04 63–71 %. CPU-bound HTTP (SHA-256 per
  request) is the same everywhere within 8 %.
- **Loopback TCP does not repeat across installations:** the same
  system moved by up to half between rounds (Ubuntu 24.04 from 548 to
  743 Gbit/s, Ubuntu 22.04 from 526 to 299), while three runs on one
  installation agreed within 3 %. The table shows the mean of both
  rounds; read it as a rough ordering only.
- **Disk:** one NVMe gives 5.3–5.8 GB/s sequential read and 2.0–2.1 GB/s
  write on every system. Debian 13, Arch Linux and CachyOS have the
  lowest QD1 latency and the most `fdatasync`'ed writes per second;
  Ubuntu 24.04 gets the fewest 4K IOPS from one submitting job in both
  rounds. The 4 GB test file lies on one disk or straddles both, by
  chance of ext4's allocation over the linear volume, so runs with
  several jobs can read both disks at once (11.2 GB/s once); the tables
  compare single-job results.
- **DNS:** the server sits behind Hetzner's stateless Robot firewall,
  which accepted TCP answers but no UDP answers, so lookups to public
  resolvers timed out until systemd-resolved fell back to TCP, about a
  minute after each boot: the 38 s DNS wait of every installation. An
  input rule for UDP from source port 53 removes it; ump warns when a
  Robot firewall drops DNS answers.
- **Microcode:** the first round showed Debian running the firmware's
  older microcode, since Debian's base system does not include it; ump's
  Debian images bring `intel-microcode` and `amd64-microcode` now, and
  the second round, shown here, has them.

## Why Debian 13 trails on HTTP

- **Not the kernel.** Debian 13 reached 1.0–1.1 M requests/s on its
  6.12 kernel, Debian's backports 7.2 and XanMod 7.2 alike, while
  Ubuntu 26.04 booted with Debian's very 7.2 kernel reached 1.48 M.
  Debian's 250 Hz timer is not it either: that same kernel has it.
- **Not the benchmark's userspace:** Ubuntu 26.04's compiler, glibc
  and binaries in a chroot on Debian 13 stayed slow. Not microcode,
  sysctls, transparent huge pages, the preemption mode, cgroups,
  scheduling, the firewall, auditing, or the watchdog drivers only
  Debian loads.
- **What differs is how the CPU runs under the load:** the same server
  and load generator get 0.55 instructions per clock on Debian 13 and
  0.90 on Ubuntu, with the uncore (the ring between the cores and L3) at
  700 against 1,200 MHz, inside the same locked 65 W budget. Which part
  of Debian 13 makes the CPU split its power that way is still open.

## Running it yourself

On a server ump installed, with any of these systems:

```console
$ git clone https://github.com/ofzhur/benchmarks && cd benchmarks
$ sudo ./bench.sh --install-deps -o /tmp/deps sysinfo   # the tools each suite needs
$ sudo ./bench.sh --tune -o results                     # every suite, the CPU tuned as above
```

The sysctls above go in first with `sysctl -w`. `results/` then holds
`results.json`, `summary.md`, the log and the raw tool output; see the
benchmarks' README for the suites and their options.
