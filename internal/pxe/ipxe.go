package pxe

import (
	"fmt"
	"os"
	"strings"
)

// IPXEDirs are where the distributions' ipxe packages put the iPXE
// binaries: Debian's and Ubuntu's ipxe, Fedora's and RHEL's
// ipxe-bootimgs-x86 and ipxe-bootimgs-aarch64, Arch's ipxe.
var IPXEDirs = []string{"/usr/lib/ipxe", "/usr/share/ipxe"}

// IPXEDir is the first of IPXEDirs this machine has; empty when none.
func IPXEDir() string {
	for _, d := range IPXEDirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}

// The iPXE scripts a PXE server answers with. The site's DHCP server
// points iPXE at boot.ipxe (BootScript), which is the same for every
// machine and asks for the machine's own script with what iPXE knows of
// it (Client); that script boots the disk (ExitScript) or a kernel and
// an initrd of the server (KernelScript). Every failure ends in exit,
// the next boot entry: the disk, never an installation.

// BootScript is boot.ipxe of the server at base (http://ADDRESS:PORT).
// lm says whether an x86 BIOS machine has a 64-bit CPU: 1 yes, 0 no
// (cpuid works, but the CPU has no long mode), empty when the iPXE
// binary has no cpuid; a BIOS iPXE says i386 whatever the CPU.
func BootScript(base string) string {
	base = strings.TrimSuffix(base, "/")
	return `#!ipxe
# ump: the machine's own script, asked for with what iPXE knows of it
iseq ${platform} pcbios && cpuid 0 && set lm 0 ||
iseq ${platform} pcbios && cpuid --ext 29 && set lm 1 ||
chain --autofree ` + base + `/ipxe?mac=${netX/mac}&ip=${netX/ip}&buildarch=${buildarch}&platform=${platform}&uuid=${uuid}&mem=${memsize}&lm=${lm} || goto down
:down
echo ump: no answer from the PXE server; booting from disk
sleep 5
exit
`
}

// ExitScript says a line on the console and exits to the next boot
// entry; with pause it waits five seconds first, so that the line can
// be read.
func ExitScript(line string, pause bool) string {
	s := "#!ipxe\necho " + scriptText(line) + "\n"
	if pause {
		s += "sleep 5\n"
	}
	return s + "exit\n"
}

// KernelScript boots the kernel and the initrd of a boot (boot, its ID)
// from the server at base, with the kernel's arguments (KernelArgs),
// trying three times; a boot that fails exits to the next boot entry.
func KernelScript(name, mode, base, boot, args string) string {
	base = strings.TrimSuffix(base, "/")
	return fmt.Sprintf(`#!ipxe
echo %s
set n 0
:load
kernel %s/b/%s/vmlinuz %s || goto retry
initrd --name initrd %s/b/%s/initrd || goto retry
boot || goto failed
:retry
imgfree
inc n
iseq ${n} 3 && goto failed ||
sleep 5
goto load
:failed
echo ump: the boot failed; booting from disk in 10 s
sleep 10
exit
`, scriptText("ump: "+name+": "+mode), base, boot, args, base, boot)
}

// KernelArgs are the rescue kernel's arguments for a machine: the
// initrd iPXE loaded, its consoles and panic=30 (a kernel that crashes
// reboots, to the disk). On amd64 the serial console (console, default
// ttyS0,115200n8) and the screen, last, which programs write to; on
// arm64 the console only when pxe.yaml gives one, since the kernel takes
// it from the firmware (SPCR) and a wrong one silences the right one.
// ConsoleNone passes none.
func KernelArgs(console, arch string) string {
	args := []string{"initrd=initrd"}
	switch {
	case console == ConsoleNone:
	case arch == ArchARM64 && console != "":
		args = append(args, "console="+console)
	case arch == ArchARM64:
	case console != "":
		args = append(args, "console="+console, "console=tty0")
	default:
		args = append(args, "console=ttyS0,115200n8", "console=tty0")
	}
	return strings.Join(append(args, "panic=30"), " ")
}

// scriptText keeps a line of text what it is in an iPXE script: no
// setting expansion, no operators, no line break.
func scriptText(s string) string {
	return strings.NewReplacer("${", "$ {", "||", "|", "&&", "&", "\n", " ", "\r", " ", "#", "").Replace(s)
}
