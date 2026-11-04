package pxe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// BootConfig is the boot config a PXE server puts in front of the
// rescue image: /ump in an uncompressed cpio, which the kernel unpacks
// into the same initramfs and the rescue system reads (images/rescue,
// init and ump-rescue-setup). It carries what does not belong on the
// kernel command line of an unauthenticated script, and what would not
// fit there.
type BootConfig struct {
	// Hostname is the rescue system's (ump.hostname).
	Hostname string
	// BootMAC is the interface the machine booted from, the only one
	// with DHCP (ump.boot-mac).
	BootMAC string
	// DNS is ump.dns: dhcp for the lease's resolvers, else addresses
	// separated by commas; empty: the rescue system's default.
	DNS string
	// Time sets the clock when it is far off (ump.time).
	Time time.Time
	// URL is the PXE server (ump.pxe), Boot this boot's ID
	// (ump.pxe.boot), Install that an installation is armed
	// (ump.pxe.mode=install).
	URL     string
	Boot    string
	Install bool
	// Keys are root's authorized_keys, Token the boot's token (0600).
	Keys  []string
	Token string
}

// Cmdline is /ump/cmdline: words of a kernel command line.
func (b BootConfig) Cmdline() string {
	var w []string
	add := func(k, v string) {
		if v != "" {
			w = append(w, k+"="+v)
		}
	}
	add("ump.hostname", b.Hostname)
	add("ump.boot-mac", b.BootMAC)
	add("ump.dns", b.DNS)
	if !b.Time.IsZero() {
		add("ump.time", strconv.FormatInt(b.Time.Unix(), 10))
	}
	add("ump.pxe", strings.TrimSuffix(b.URL, "/"))
	add("ump.pxe.boot", b.Boot)
	if b.Install {
		add("ump.pxe.mode", ModeInstall)
	}
	return strings.Join(w, " ") + "\n"
}

// Cpio is the boot config as the archive in front of the image.
func (b BootConfig) Cpio() []byte {
	var mtime int64
	if !b.Time.IsZero() {
		mtime = b.Time.Unix()
	}
	files := []CpioFile{
		{Name: "ump", Mode: 0o040700, MTime: mtime},
		{Name: "ump/cmdline", Mode: 0o100644, MTime: mtime, Data: []byte(b.Cmdline())},
	}
	if len(b.Keys) > 0 {
		files = append(files, CpioFile{Name: "ump/authorized_keys", Mode: 0o100600, MTime: mtime, Data: []byte(strings.Join(b.Keys, "\n") + "\n")})
	}
	if b.Token != "" {
		files = append(files, CpioFile{Name: "ump/token", Mode: 0o100600, MTime: mtime, Data: []byte(b.Token + "\n")})
	}
	var buf bytes.Buffer
	WriteCpio(&buf, files)
	return buf.Bytes()
}

// CpioFile is an entry of a newc cpio archive: Mode has the type bits
// (0o040000 a directory, 0o100000 a file) and the permissions.
type CpioFile struct {
	Name  string
	Mode  uint32
	MTime int64
	Data  []byte
}

const (
	cpioMagic   = "070701"
	cpioTrailer = "TRAILER!!!"
	cpioHeader  = 110
)

// WriteCpio writes the files as an uncompressed newc archive owned by
// root, padded to 512 bytes after the trailer, as the kernel takes it in
// front of a compressed one.
func WriteCpio(w io.Writer, files []CpioFile) error {
	var buf bytes.Buffer
	for i, f := range append(slices.Clone(files), CpioFile{Name: cpioTrailer}) {
		nlink, ino := 1, i+1
		if f.Mode&0o170000 == 0o040000 {
			nlink = 2
		}
		if f.Name == cpioTrailer {
			ino = 0
		}
		fmt.Fprintf(&buf, "%s%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			cpioMagic, ino, f.Mode, 0, 0, nlink, f.MTime, len(f.Data), 0, 0, 0, 0, len(f.Name)+1, 0)
		buf.WriteString(f.Name)
		buf.WriteByte(0)
		pad(&buf, 4)
		buf.Write(f.Data)
		pad(&buf, 4)
	}
	pad(&buf, 512)
	_, err := w.Write(buf.Bytes())
	return err
}

func pad(b *bytes.Buffer, to int) {
	for b.Len()%to != 0 {
		b.WriteByte(0)
	}
}

// ReadCpio reads a newc archive up to its trailer.
func ReadCpio(data []byte) ([]CpioFile, error) {
	var files []CpioFile
	off := 0
	field := func(h []byte, i int) (uint64, error) {
		return strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 32)
	}
	for {
		if off+cpioHeader > len(data) {
			return nil, errors.New("cpio: no trailer")
		}
		h := data[off : off+cpioHeader]
		if string(h[:6]) != cpioMagic {
			return nil, fmt.Errorf("cpio: no newc header at %d", off)
		}
		var v [13]uint64
		for i := range v {
			n, err := field(h, i)
			if err != nil {
				return nil, fmt.Errorf("cpio: header at %d: %w", off, err)
			}
			v[i] = n
		}
		size, namesize := int(v[6]), int(v[11])
		if namesize < 1 || off+cpioHeader+namesize > len(data) {
			return nil, fmt.Errorf("cpio: bad name size at %d", off)
		}
		name := string(data[off+cpioHeader : off+cpioHeader+namesize-1])
		off = align(off+cpioHeader+namesize, 4)
		if name == cpioTrailer {
			return files, nil
		}
		if size < 0 || off+size > len(data) {
			return nil, fmt.Errorf("cpio: %s: bad size", name)
		}
		files = append(files, CpioFile{Name: name, Mode: uint32(v[1]), MTime: int64(v[5]), Data: data[off : off+size]})
		off = align(off+size, 4)
	}
}

func align(n, to int) int { return (n + to - 1) / to * to }
