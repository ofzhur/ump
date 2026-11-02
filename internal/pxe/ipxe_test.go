package pxe

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBootScript(t *testing.T) {
	s := BootScript("http://192.0.2.5:8067/")
	for _, want := range []string{
		"#!ipxe\n",
		"iseq ${platform} pcbios && cpuid 0 && set lm 0 ||\n",
		"iseq ${platform} pcbios && cpuid --ext 29 && set lm 1 ||\n",
		"chain --autofree http://192.0.2.5:8067/ipxe?mac=${netX/mac}&ip=${netX/ip}&buildarch=${buildarch}&platform=${platform}&uuid=${uuid}&mem=${memsize}&lm=${lm} || goto down\n",
		":down\necho ump: no answer from the PXE server; booting from disk\nsleep 5\nexit\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("boot.ipxe lacks %q:\n%s", want, s)
		}
	}
}

func TestScripts(t *testing.T) {
	if got := ExitScript("ump: web-07: local", false); got != "#!ipxe\necho ump: web-07: local\nexit\n" {
		t.Errorf("local: %q", got)
	}
	if got := ExitScript("ump: x: ${evil} || chain http://x # y\nexit", true); got != "#!ipxe\necho ump: x: $ {evil} | chain http://x  y exit\nsleep 5\nexit\n" {
		t.Errorf("a line kept as text: %q", got)
	}
	s := KernelScript("web-07", ModeRescue, "http://192.0.2.5:8067", "3q9Kf", KernelArgs("ttyS1,115200n8", ArchAMD64))
	for _, want := range []string{
		"echo ump: web-07: rescue\n",
		"kernel http://192.0.2.5:8067/b/3q9Kf/vmlinuz initrd=initrd console=ttyS1,115200n8 console=tty0 panic=30 || goto retry\n",
		"initrd --name initrd http://192.0.2.5:8067/b/3q9Kf/initrd || goto retry\n",
		"boot || goto failed\n",
		"iseq ${n} 3 && goto failed ||\n",
		":failed\necho ump: the boot failed; booting from disk in 10 s\nsleep 10\nexit\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script lacks %q:\n%s", want, s)
		}
	}
	for _, c := range []struct{ console, arch, want string }{
		{"", ArchAMD64, "initrd=initrd console=ttyS0,115200n8 console=tty0 panic=30"},
		{"", "", "initrd=initrd console=ttyS0,115200n8 console=tty0 panic=30"},
		{"ttyS1,115200n8", ArchAMD64, "initrd=initrd console=ttyS1,115200n8 console=tty0 panic=30"},
		{ConsoleNone, ArchAMD64, "initrd=initrd panic=30"},
		{"", ArchARM64, "initrd=initrd panic=30"},
		{"ttyAMA0", ArchARM64, "initrd=initrd console=ttyAMA0 panic=30"},
		{ConsoleNone, ArchARM64, "initrd=initrd panic=30"},
	} {
		if got := KernelArgs(c.console, c.arch); got != c.want {
			t.Errorf("%q on %q: %q", c.console, c.arch, got)
		}
	}
}

func TestBootConfig(t *testing.T) {
	b := BootConfig{Hostname: "web-07", BootMAC: "0c:c4:7a:aa:bb:01", DNS: "dhcp", Time: time.Unix(1793613600, 0),
		URL: "http://192.0.2.5:8067/", Boot: "3q9Kf", Install: true,
		Keys: []string{"ssh-ed25519 AAAAump ump", "ssh-ed25519 AAAAoleg oleg"}, Token: "s3cret"}
	if got := b.Cmdline(); got != "ump.hostname=web-07 ump.boot-mac=0c:c4:7a:aa:bb:01 ump.dns=dhcp ump.time=1793613600 ump.pxe=http://192.0.2.5:8067 ump.pxe.boot=3q9Kf ump.pxe.mode=install\n" {
		t.Errorf("cmdline: %q", got)
	}
	data := b.Cpio()
	if len(data)%512 != 0 {
		t.Errorf("not padded to 512 bytes: %d", len(data))
	}
	files, err := ReadCpio(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []CpioFile{
		{Name: "ump", Mode: 0o040700},
		{Name: "ump/cmdline", Mode: 0o100644, Data: []byte(b.Cmdline())},
		{Name: "ump/authorized_keys", Mode: 0o100600, Data: []byte("ssh-ed25519 AAAAump ump\nssh-ed25519 AAAAoleg oleg\n")},
		{Name: "ump/token", Mode: 0o100600, Data: []byte("s3cret\n")},
	}
	if len(files) != len(want) {
		t.Fatalf("files: %+v", files)
	}
	for i, f := range files {
		if f.Name != want[i].Name || f.Mode != want[i].Mode || !bytes.Equal(f.Data, want[i].Data) || f.MTime != 1793613600 {
			t.Errorf("file %d: %+v, want %+v", i, f, want[i])
		}
	}
	// the header the kernel reads: newc, the name's size with its NUL,
	// the name and the data aligned to 4 bytes
	if string(data[:6]) != "070701" || !bytes.Contains(data, []byte("TRAILER!!!\x00")) {
		t.Errorf("not a newc archive: %q", data[:120])
	}
	// a rescue boot without a token or keys has the cmdline alone
	files, err = ReadCpio(BootConfig{Hostname: "x"}.Cpio())
	if err != nil || len(files) != 2 || string(files[1].Data) != "ump.hostname=x\n" || files[1].MTime != 0 {
		t.Errorf("the least: %+v %v", files, err)
	}
	for _, bad := range [][]byte{nil, []byte("070702"), data[:200], bytes.Repeat([]byte("0"), 120)} {
		if _, err := ReadCpio(bad); err == nil {
			t.Errorf("%q read", bad)
		}
	}
	in := []CpioFile{{Name: "a", Mode: 0o100644, Data: []byte("x")}}
	var buf bytes.Buffer
	WriteCpio(&buf, in[:1:1])
	if len(in) != 1 {
		t.Error("WriteCpio changed its argument")
	}
}

func FuzzReadCpio(f *testing.F) {
	f.Add(BootConfig{Hostname: "web-07", Keys: []string{"k"}, Token: "t"}.Cpio())
	f.Add([]byte("070701"))
	f.Fuzz(func(t *testing.T, data []byte) {
		files, err := ReadCpio(data)
		if err != nil {
			return
		}
		var buf bytes.Buffer
		if err := WriteCpio(&buf, files); err != nil {
			t.Fatal(err)
		}
		again, err := ReadCpio(buf.Bytes())
		if err != nil || len(again) != len(files) {
			t.Fatalf("%d files written, %d read back: %v", len(files), len(again), err)
		}
	})
}
