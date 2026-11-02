package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/pxe"
	"github.com/ofzhur/ump/internal/pxe/tftp"
)

// ump pxe: the PXE server of a site (serve), what the PXE servers see
// (status), and an armed boot taken back (disarm). The server reads
// the backend itself, never the home's mirror (daemonCommand), and only
// .pxe/<site>/ there.

func (a *app) newPXECmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pxe",
		Short: "The PXE server of a site, what the PXE servers see, and armed boots",
		Long: `ump pxe serve is the PXE server of a site: a private network whose DHCP
server points the machines' firmware at an iPXE binary, and iPXE at the
server's http://HOST:8067/boot.ipxe (docs/pxe.md has the DHCP servers'
recipes). It serves the machines of pxe.yaml as ump config pxe apply
rendered them for the site: UMP's rescue system to a machine armed for
it (ump server rescue) or standing in mode rescue, its disk to the
others. ump hands out no addresses.

ump pxe status shows what the PXE servers of each site see, ump pxe
disarm takes an armed boot back.`,
		Example: `  ump pxe serve --site fra1 --http 192.0.2.5:8067 --tftp 192.0.2.5:69
  ump pxe status                # the servers and the machines
  ump pxe status web-07         # one machine
  ump pxe disarm web-07`,
	}
	cmd.AddCommand(a.newPXEServeCmd(), a.newPXEStatusCmd(), a.newPXEDisarmCmd())
	return cmd
}

// daemonCommand says whether the arguments run the PXE server, which
// reads its site's documents from the backend and nothing of the home:
// it neither pulls nor pushes the mirror, and checks for no update.
func daemonCommand(args []string) bool {
	var words []string
	for i := 0; i < len(args) && len(words) < 2; i++ {
		switch arg := args[i]; {
		case arg == "--":
			i = len(args)
		case arg == "--config" || arg == "--account":
			i++ // the global flags' values are not the command
		case !strings.HasPrefix(arg, "-"):
			words = append(words, arg)
		}
	}
	return len(words) == 2 && words[0] == "pxe" && words[1] == "serve"
}

// pxeServeFlags are the flags of ump pxe serve; each has an environment
// variable, UMP_PXE_ and its name (UMP_PXE_SITE, UMP_PXE_IPXE_DIR), which
// the flag wins over.
var pxeServeFlags = []string{"site", "http", "url", "tftp", "ipxe-dir", "data", "cache", "poll"}

func pxeServeEnv(flag string) string {
	return "UMP_PXE_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

type pxeServeOptions struct {
	site, http, url, tftp, ipxeDir, data, cache string
	poll                                        time.Duration
	check                                       bool
}

func (a *app) newPXEServeCmd() *cobra.Command {
	var o pxeServeOptions
	cmd := &cobra.Command{
		Use:   "serve [--site SITE] [--http ADDR] [--url URL] [--tftp ADDR] [--ipxe-dir DIR] [--data DIR] [--cache DIR] [--poll DURATION] [--check]",
		Short: "Serve the machines of a site over the network: the PXE server",
		Long: `Serve the machines of a site over the network, in chain mode: the
site's DHCP server points the firmware at an iPXE binary (from its own
TFTP server, or from this one with --tftp) and iPXE at
http://HOST:8067/boot.ipxe, which asks for the machine's own script. A
machine of pxe.yaml gets its disk (iPXE exits to the next boot entry),
or UMP's rescue system: kernel and initrd over HTTP, verified against
the rescue image's manifest and cached, with a boot config in front
(hostname, root's keys, DHCP on the interface it booted from). An armed
rescue boots once: it is consumed when its initrd was sent completely.
Every failure ends in the disk.

The server reads the backend of the home (UMP_STATE and the credential
variables, else the home of the user it runs as) every --poll, and only
.pxe/<site>/ there: site.json, which ump config pxe apply renders, and
the arms; it writes the outcomes of the arms it consumed and its status
(ump pxe status). It reads nothing of pxe.yaml: a hand edit reaches it
through ump config pxe apply.

--http is where it listens (default :8067, every interface: on a host
with public addresses, give the private network's), --url where the
machines reach it when that is not the address they asked (NAT, a
container). --tftp ADDR serves the iPXE binaries read-only over TFTP
(port 69 needs root or CAP_NET_BIND_SERVICE): those of --ipxe-dir, by
default of the distribution's ipxe package (/usr/lib/ipxe on Debian and
Ubuntu, /usr/share/ipxe on Fedora, RHEL and Arch). --data keeps what
outlives a restart (default $STATE_DIRECTORY, else
~/.local/state/ump-pxe/SITE), --cache the rescue image (default
$CACHE_DIRECTORY, else ~/.cache/ump/pxe/SITE). Each flag can come from
UMP_PXE_ and its name in upper case (UMP_PXE_SITE, UMP_PXE_IPXE_DIR).

--check reads site.json, fetches and verifies the rescue image, binds the
ports and exits: 0 when the server can serve. SIGTERM or Ctrl-C stops
the server after the transfers under way.`,
		Example: `  ump pxe serve                                  # site default, a lab on this machine
  ump pxe serve --site fra1 --http 192.0.2.5:8067 --tftp 192.0.2.5:69
  ump pxe serve --site fra1 --check`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range pxeServeFlags {
				if v := os.Getenv(pxeServeEnv(name)); v != "" && !cmd.Flags().Changed(name) {
					if err := cmd.Flags().Set(name, v); err != nil {
						return usageErrorf(cmd, "%s: %v", pxeServeEnv(name), err)
					}
				}
			}
			switch {
			case !pxe.ValidSite(o.site):
				return usageErrorf(cmd, "--site %q is not a site's name (a DNS label)", o.site)
			case o.ipxeDir != "" && o.tftp == "":
				return usageErrorf(cmd, "--ipxe-dir names the files of the TFTP server: it needs --tftp")
			case o.poll < time.Second:
				return usageErrorf(cmd, "--poll %s: at least 1s", o.poll)
			}
			if o.url != "" {
				u, err := url.Parse(o.url)
				if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
					return usageErrorf(cmd, "--url %q is not where machines reach the server, like http://192.0.2.5:8067", o.url)
				}
				o.url = strings.TrimSuffix(o.url, "/")
			}
			o.http, o.tftp = withPort(o.http), withPort(o.tftp)
			return a.pxeServe(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.site, "site", pxe.DefaultSite, "the site it serves")
	f.StringVar(&o.http, "http", ":8067", "the address and port HTTP listens on")
	f.StringVar(&o.url, "url", "", "where the machines reach it (default: the address and port they asked)")
	f.StringVar(&o.tftp, "tftp", "", "serve the iPXE binaries over TFTP on this address (:69, or 192.0.2.5:69)")
	f.StringVar(&o.ipxeDir, "ipxe-dir", "", "the directory of the iPXE binaries TFTP serves (default: the ipxe package's)")
	f.StringVar(&o.data, "data", "", "the directory of what outlives a restart (default $STATE_DIRECTORY, else ~/.local/state/ump-pxe/SITE)")
	f.StringVar(&o.cache, "cache", "", "the directory of the rescue image (default $CACHE_DIRECTORY, else ~/.cache/ump/pxe/SITE)")
	f.DurationVar(&o.poll, "poll", 10*time.Second, "how often it reads the backend")
	f.BoolVar(&o.check, "check", false, "read, fetch, verify and bind, then exit")
	return cmd
}

// withPort takes a bare port (69) as one on every address (:69).
func withPort(addr string) string {
	if addr != "" && !strings.Contains(addr, ":") {
		return ":" + addr
	}
	return addr
}

// pxeDirs are the server's directories: --data and --cache, else
// systemd's (StateDirectory=, CacheDirectory=), else the user's.
func pxeDirs(o pxeServeOptions) (data, cache string) {
	first := func(env string) string {
		d, _, _ := strings.Cut(os.Getenv(env), ":")
		return d
	}
	data, cache = o.data, o.cache
	if data == "" {
		if data = first("STATE_DIRECTORY"); data == "" {
			base := os.Getenv("XDG_STATE_HOME")
			if base == "" {
				home, _ := os.UserHomeDir()
				base = filepath.Join(home, ".local", "state")
			}
			data = filepath.Join(base, "ump-pxe", o.site)
		}
	}
	if cache == "" {
		if cache = first("CACHE_DIRECTORY"); cache == "" {
			cache = filepath.Join(config.CacheDir(), "pxe", o.site)
		}
	}
	return data, cache
}

// pxeName is the server's name among those of its site: the host's.
func pxeName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	h = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, h)
	if h = strings.Trim(h, "-"); h == "" {
		return "pxe"
	}
	return h
}

// listenError says why a port could not be bound, in words.
func listenError(flag, addr string, err error) error {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Errorf("%s %s: the address is in use (does the service run already?)", flag, addr)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%s %s: the port needs root or CAP_NET_BIND_SERVICE", flag, addr)
	}
	return fmt.Errorf("%s %s: %w", flag, addr, err)
}

// publicAddresses are this host's public addresses: those a listener on
// every interface answers the internet at.
func publicAddresses() []string {
	addrs, _ := net.InterfaceAddrs()
	var out []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if ip = ip.Unmap(); ok && ip.IsGlobalUnicast() && !ip.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			out = append(out, ip.String())
		}
	}
	return out
}

// everyInterface says whether an address listens on every interface.
func everyInterface(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return host == "" || err == nil && ip.IsUnspecified()
}

// sdNotify tells systemd how the service is (sd_notify(3)) when it runs
// as Type=notify; nothing otherwise.
func sdNotify(state string) {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return
	}
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte(state))
}

// lineWriter is a log.Logger's output as the server's log lines.
type lineWriter func(format string, args ...any)

func (w lineWriter) Write(b []byte) (int, error) {
	w("%s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func (a *app) pxeServe(cmd *cobra.Command, o pxeServeOptions) error {
	ctx := cmd.Context()
	var mu sync.Mutex
	line := func() func(format string, args ...any) {
		return func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(a.stderr, "pxe: %s: %s\n", o.site, fmt.Sprintf(format, args...))
		}
	}
	logf := line()
	var debugf func(format string, args ...any)
	if a.verbose {
		debugf = logf
	}
	data, cache := pxeDirs(o)
	store := a.pxeStore()
	where := config.AbbreviateHome(config.UserConfigDir())
	if a.home != nil && a.home.Backend != nil {
		where = a.home.Backend.String()
	}
	s := &pxe.Server{Site: o.site, Name: pxeName(), Version: Version, Store: store,
		Cache: &pxe.Cache{Dir: cache, Client: a.httpClient}, URL: o.url, Data: data, Poll: o.poll,
		Logf: logf, Debugf: debugf, Notify: sdNotify}
	logf("ump %s, server %s, the backend %s", Version, s.Name, where)
	if err := s.Open(ctx); err != nil {
		return err
	}
	loadErr := s.Load(ctx)

	ln, err := net.Listen("tcp", o.http)
	if err != nil {
		return listenError("--http", o.http, err)
	}
	defer ln.Close()
	var pc net.PacketConn
	var ts *tftp.Server
	if o.tftp != "" {
		dir := o.ipxeDir
		if dir == "" {
			if dir = pxe.IPXEDir(); dir == "" {
				return fmt.Errorf("--tftp: no iPXE binaries in %s; install the distribution's ipxe package (Debian and Ubuntu: ipxe; Fedora and RHEL: ipxe-bootimgs-x86 and ipxe-bootimgs-aarch64), or give --ipxe-dir", strings.Join(pxe.IPXEDirs, " or "))
			}
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("--ipxe-dir %s: not a directory", dir)
		}
		if pc, err = net.ListenPacket("udp", o.tftp); err != nil {
			return listenError("--tftp", o.tftp, err)
		}
		defer pc.Close()
		ts = &tftp.Server{Dir: dir, Logf: logf, Debugf: debugf}
	}
	var warnings []string
	if public := publicAddresses(); len(public) > 0 {
		for _, l := range []struct{ what, addr string }{{"HTTP", o.http}, {"TFTP", o.tftp}} {
			if l.addr != "" && everyInterface(l.addr) {
				w := fmt.Sprintf("%s listens on every interface (%s), and this host has public addresses (%s): give the private network's address, or keep the port closed on the others", l.what, l.addr, strings.Join(public, ", "))
				warnings = append(warnings, w)
				logf("%s", w)
			}
		}
	}
	s.Warnings = warnings

	if o.check {
		return a.pxeServeCheck(ctx, s, o, loadErr, ts)
	}

	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 16 << 10, ErrorLog: log.New(lineWriter(logf), "", 0)}
	failed := make(chan error, 2)
	go func() {
		if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("HTTP: %w", err)
		}
	}()
	tctx, stopTFTP := context.WithCancel(context.WithoutCancel(ctx))
	tftpDone := make(chan struct{})
	if ts != nil {
		go func() {
			defer close(tftpDone)
			if err := ts.Serve(tctx, pc); err != nil {
				failed <- fmt.Errorf("TFTP: %w", err)
			}
		}()
	} else {
		close(tftpDone)
	}
	rctx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	runDone := make(chan struct{})
	go func() { s.Run(rctx); close(runDone) }()
	served := "HTTP " + o.http
	if ts != nil {
		served += ", TFTP " + o.tftp + " (" + ts.Dir + ")"
	}
	logf("serving site %s: %s", o.site, served)
	sdNotify("READY=1")

	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	logf("stopping after the transfers under way")
	sdNotify("STOPPING=1")
	sctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	if serr := hs.Shutdown(sctx); serr != nil {
		logf("HTTP: %v", serr)
	}
	stopTFTP()
	<-tftpDone
	stopRun()
	<-runDone
	logf("stopped")
	return err
}

// pxeServeCheck is serve --check: what the server would serve, said; an
// error when it cannot.
func (a *app) pxeServeCheck(ctx context.Context, s *pxe.Server, o pxeServeOptions, loadErr error, ts *tftp.Server) error {
	var problems []string
	d, err := s.Store.Site(ctx, o.site)
	switch {
	case loadErr != nil:
		problems = append(problems, fmt.Sprintf("the backend: %v", loadErr))
	case err != nil:
		problems = append(problems, err.Error())
	case d == nil:
		problems = append(problems, fmt.Sprintf("no site.json for site %s: ump config pxe apply renders it", o.site))
	default:
		fmt.Fprintf(a.stdout, "Site %s: site.json %s, %s.\n", o.site, d.Hash, plural(len(d.Machines), "machine"))
	}
	if d != nil {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err := s.Fetch(fctx)
		cancel()
		st := s.Status()
		for _, arch := range slices.Sorted(func(yield func(string) bool) {
			for a := range st.Rescue {
				if !yield(a) {
					return
				}
			}
		}) {
			fmt.Fprintf(a.stdout, "Rescue image %s for %s: verified, in %s.\n", st.Rescue[arch], arch, config.AbbreviateHome(s.Cache.Dir))
		}
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	fmt.Fprintf(a.stdout, "HTTP %s: bound.\n", o.http)
	if ts != nil {
		files := ipxeFiles(ts.Dir)
		binaries := "1 iPXE binary"
		if len(files) != 1 {
			binaries = fmt.Sprintf("%d iPXE binaries", len(files))
		}
		fmt.Fprintf(a.stdout, "TFTP %s: bound, %s in %s", o.tftp, binaries, ts.Dir)
		if len(files) > 0 {
			fmt.Fprintf(a.stdout, ": %s", strings.Join(files, ", "))
		}
		fmt.Fprintln(a.stdout, ".")
		if len(files) == 0 {
			problems = append(problems, "no iPXE binary in "+ts.Dir)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("site %s cannot be served: %s", o.site, strings.Join(problems, "; "))
	}
	return nil
}

// ipxeFiles are the iPXE binaries of a directory and of its
// subdirectories (Fedora's arm64-efi/), by their path under it.
func ipxeFiles(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() && strings.Count(rel, string(filepath.Separator)) >= 1 {
			return filepath.SkipDir
		}
		switch filepath.Ext(p) {
		case ".efi", ".kpxe", ".kkpxe", ".pxe", ".lkrn":
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// --- status ------------------------------------------------------------------

// pxeServerRow is a PXE server as ump pxe status shows it.
type pxeServerRow struct {
	Name    string    `json:"name" yaml:"name"`
	Site    string    `json:"site" yaml:"site"`
	Version string    `json:"version" yaml:"version"`
	URL     string    `json:"url,omitempty" yaml:"url,omitempty"`
	Started time.Time `json:"started" yaml:"started"`
	Updated time.Time `json:"updated" yaml:"updated"`
	// Running says that it wrote its status lately, Current that it
	// serves the site.json in the backend.
	Running  bool              `json:"running" yaml:"running"`
	SiteHash string            `json:"site_hash,omitempty" yaml:"site_hash,omitempty"`
	Current  bool              `json:"current" yaml:"current"`
	Rescue   map[string]string `json:"rescue,omitempty" yaml:"rescue,omitempty"`
	Warnings []string          `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

// pxeMachineRow is a machine as ump pxe status shows it: its status, and
// what a server of its site saw of it last.
type pxeMachineRow struct {
	Name   string `json:"name" yaml:"name"`
	Site   string `json:"site" yaml:"site"`
	MAC    string `json:"mac" yaml:"mac"`
	Status string `json:"status" yaml:"status"`
	// Answer is what it got at its last network boot, Reason why the
	// disk; Address the address it had then, Rescue its rescue system's,
	// from the hello: both learned, never where ump connects but for an
	// armed boot.
	Answer   string     `json:"answer,omitempty" yaml:"answer,omitempty"`
	Reason   string     `json:"reason,omitempty" yaml:"reason,omitempty"`
	Seen     *time.Time `json:"seen,omitempty" yaml:"seen,omitempty"`
	SeenBy   string     `json:"seen_by,omitempty" yaml:"seen_by,omitempty"`
	Address  string     `json:"address,omitempty" yaml:"address,omitempty"`
	Boot     string     `json:"boot,omitempty" yaml:"boot,omitempty"`
	Rescue   string     `json:"rescue,omitempty" yaml:"rescue,omitempty"`
	Arm      string     `json:"arm,omitempty" yaml:"arm,omitempty"`
	Signals  []string   `json:"signals,omitempty" yaml:"signals,omitempty"`
	Hello    *pxe.Hello `json:"hello,omitempty" yaml:"hello,omitempty"`
	MACs     []string   `json:"macs" yaml:"macs"`
	Standing string     `json:"mode" yaml:"mode"`
}

// pxeUnknownRow is a MAC without a machine that a server saw.
type pxeUnknownRow struct {
	MAC     string    `json:"mac" yaml:"mac"`
	Site    string    `json:"site" yaml:"site"`
	Seen    time.Time `json:"seen" yaml:"seen"`
	SeenBy  string    `json:"seen_by" yaml:"seen_by"`
	Address string    `json:"address,omitempty" yaml:"address,omitempty"`
	Boot    string    `json:"boot,omitempty" yaml:"boot,omitempty"`
}

type pxeStatusDoc struct {
	Servers  []pxeServerRow  `json:"servers" yaml:"servers"`
	Machines []pxeMachineRow `json:"machines" yaml:"machines"`
	Unknown  []pxeUnknownRow `json:"unknown,omitempty" yaml:"unknown,omitempty"`
}

// pxeAgo is how long ago a time was, for people.
func (a *app) pxeAgo(t time.Time) string {
	d := a.now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour && int(d.Minutes())%60 != 0:
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func bootWords(arch, firmware string) string {
	return strings.TrimSpace(arch + " " + firmware)
}

func (a *app) newPXEStatusCmd() *cobra.Command {
	var output, logOf string
	var unknown, noHeaders bool
	cmd := &cobra.Command{
		Use:   "status [NAME|MAC...] [--unknown] [--log NAME] [-o table|wide|json|yaml]",
		Short: "What the PXE servers of each site see: the servers, the machines and their boots",
		Long: `Show the PXE servers of each site of pxe.yaml (their version, when they
last wrote their status, whether they serve the site.json in the
backend, what they warn of) and the machines, or those named (by name or
MAC): their status, what they got at their last network boot and why
the disk when their arm or mode says otherwise, when and by which server
they were seen, the address iPXE had and, wide, how they booted and their
rescue system's address from its hello. What a server noticed (a disk
that does not boot, a rescue system that does not come up, a boot order
that skips the network) follows the tables. Addresses learned at a boot
are never where ump connects but for an armed boot.

--unknown lists the MACs without a machine that a server saw (sites with
unknown: local), --log NAME prints the end of a machine's installation
log. -o json and -o yaml carry everything, the hellos too.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil || format == formatName {
				return usageErrorf(cmd, "unknown output format %q (use one of table, wide, json, yaml)", output)
			}
			c, err := loadPXE()
			if err != nil {
				return err
			}
			if logOf != "" {
				return a.pxeLog(cmd, c, logOf)
			}
			rows, sn, err := a.pxeRows(cmd, c)
			if err != nil {
				return err
			}
			doc, err := a.pxeStatus(cmd.Context(), c, rows, sn, args)
			if err != nil {
				return err
			}
			switch format {
			case formatJSON, formatYAML:
				return printEncoded(a.stdout, doc, format)
			}
			if unknown {
				return a.printPXEUnknown(doc.Unknown, noHeaders)
			}
			return a.printPXEStatus(c, doc, format == formatWide, noHeaders, len(args) > 0)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide (how each booted, its rescue system's address, its arm), json or yaml")
	cmd.Flags().BoolVar(&unknown, "unknown", false, "the MACs without a machine that a server saw")
	cmd.Flags().StringVar(&logOf, "log", "", "print the end of the installation log of this machine")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header lines")
	return cmd
}

// pxeStatus is what ump pxe status shows, of the machines named by name
// or MAC (all when none is).
func (a *app) pxeStatus(ctx context.Context, c *pxe.Config, rows []pxeRow, sn *pxe.Snapshot, args []string) (*pxeStatusDoc, error) {
	doc := &pxeStatusDoc{Servers: []pxeServerRow{}, Machines: []pxeMachineRow{}}
	for _, site := range c.SiteNames() {
		for _, st := range sn.Statuses[site] {
			r := pxeServerRow{Name: st.Daemon, Site: site, Version: st.Version, URL: st.URL, Started: st.Started, Updated: st.Updated,
				Running: a.now().Sub(st.Updated) <= pxeLive, SiteHash: st.SiteHash, Rescue: st.Rescue, Warnings: st.Warnings}
			r.Current = sn.Sites[site] != nil && st.SiteHash == sn.Sites[site].Hash
			doc.Servers = append(doc.Servers, r)
		}
	}
	// the arguments: machines by name, or MACs of machines or not
	want := map[string]bool{}
	var macs []string
	for _, arg := range args {
		name := strings.TrimPrefix(arg, pxe.TypeName+"/")
		if c.Machine(name) != nil {
			want[name] = true
			continue
		}
		mac, err := pxe.NormalizeMAC(arg)
		if err != nil {
			return nil, a.pxeSet(ctx).missing(name)
		}
		found := false
		for _, m := range c.Merged() {
			if slices.Contains(m.MACs, mac) {
				want[m.Name], found = true, true
			}
		}
		if !found {
			macs = append(macs, mac)
		}
	}
	for _, r := range rows {
		if len(args) > 0 && !want[r.Name] {
			continue
		}
		m := pxeMachineRow{Name: r.Name, Site: r.Site, MAC: r.MACs[0], MACs: r.MACs, Status: r.Status, Standing: r.Mode}
		var last *pxe.Seen
		for _, st := range sn.Statuses[r.Site] {
			for _, mac := range r.MACs {
				if s := st.Machines[mac]; s != nil && (last == nil || s.At.After(last.At)) {
					last, m.SeenBy, m.MAC = s, st.Daemon, mac
				}
			}
		}
		if last != nil {
			at := last.At
			m.Seen, m.Answer, m.Reason, m.Address, m.Boot, m.Arm, m.Signals, m.Hello =
				&at, last.Answer, last.Reason, last.Address, bootWords(last.Arch, last.Firmware), last.Arm, last.Signals, last.Hello
			if last.Hello != nil {
				m.Rescue = last.Hello.Address()
			}
		}
		doc.Machines = append(doc.Machines, m)
	}
	for _, site := range c.SiteNames() {
		for _, st := range sn.Statuses[site] {
			for mac, s := range st.Unknown {
				if len(args) > 0 && !slices.Contains(macs, mac) {
					continue
				}
				doc.Unknown = append(doc.Unknown, pxeUnknownRow{MAC: mac, Site: site, Seen: s.At, SeenBy: st.Daemon, Address: s.Address, Boot: bootWords(s.Arch, s.Firmware)})
			}
		}
	}
	sort.Slice(doc.Unknown, func(i, j int) bool { return doc.Unknown[i].Seen.After(doc.Unknown[j].Seen) })
	for _, mac := range macs {
		if !slices.ContainsFunc(doc.Unknown, func(u pxeUnknownRow) bool { return u.MAC == mac }) {
			return nil, fmt.Errorf("no machine of %s has the MAC %s, and no PXE server saw it", config.AbbreviateHome(pxePath()), mac)
		}
	}
	return doc, nil
}

func (a *app) printPXEStatus(c *pxe.Config, doc *pxeStatusDoc, wide, noHeaders, named bool) error {
	var notes []string
	if !named {
		if len(doc.Servers) == 0 {
			fmt.Fprintf(a.stderr, "No PXE server of %s has written its status (ump pxe serve --site SITE).\n", strings.Join(c.SiteNames(), ", "))
		} else {
			headers := []string{"SERVER", "SITE", "VERSION", "URL", "UPDATED", "SITE.JSON"}
			if wide {
				headers = append(headers, "RESCUE", "STARTED")
			}
			var table [][]string
			for _, s := range doc.Servers {
				updated := a.pxeAgo(s.Updated)
				if !s.Running {
					updated += " (stopped?)"
				}
				served := orDash(s.SiteHash)
				switch {
				case s.SiteHash == "":
				case s.Current:
					served += " (current)"
				default:
					served += " (old)"
				}
				row := []string{s.Name, s.Site, s.Version, orDash(s.URL), updated, served}
				if wide {
					var rescue []string
					for _, arch := range slices.Sorted(func(yield func(string) bool) {
						for a := range s.Rescue {
							if !yield(a) {
								return
							}
						}
					}) {
						rescue = append(rescue, arch+" "+s.Rescue[arch])
					}
					row = append(row, orDash(strings.Join(rescue, ", ")), a.fmtTime(s.Started))
				}
				table = append(table, row)
				if s.Running {
					for _, w := range s.Warnings {
						notes = append(notes, s.Site+"/"+s.Name+": "+w)
					}
				}
			}
			if err := writeTable(a.stdout, headers, table, noHeaders); err != nil {
				return err
			}
		}
	}
	if len(doc.Machines) > 0 {
		if !named && len(doc.Servers) > 0 {
			fmt.Fprintln(a.stdout)
		}
		headers := []string{"NAME", "SITE", "MAC", "STATUS", "ANSWER", "SEEN", "BY", "ADDRESS"}
		if wide {
			headers = append(headers, "BOOT", "RESCUE", "ARM")
		}
		var table [][]string
		for _, m := range doc.Machines {
			seen := "-"
			if m.Seen != nil {
				seen = a.pxeAgo(*m.Seen)
			}
			row := []string{m.Name, m.Site, m.MAC, m.Status, orDash(m.Answer), seen, orDash(m.SeenBy), orDash(m.Address)}
			if wide {
				row = append(row, orDash(m.Boot), orDash(m.Rescue), orDash(pxe.ShortID(m.Arm)))
			}
			table = append(table, row)
			if m.Reason != "" {
				notes = append(notes, m.Name+": "+m.Answer+": "+m.Reason)
			}
			for _, s := range m.Signals {
				notes = append(notes, m.Name+": "+s)
			}
		}
		if err := writeTable(a.stdout, headers, table, noHeaders); err != nil {
			return err
		}
	}
	if len(doc.Unknown) > 0 && named {
		if len(doc.Machines) > 0 {
			fmt.Fprintln(a.stdout)
		}
		if err := a.printPXEUnknown(doc.Unknown, noHeaders); err != nil {
			return err
		}
	}
	if len(notes) > 0 {
		fmt.Fprintln(a.stdout)
		for _, n := range notes {
			fmt.Fprintln(a.stdout, n)
		}
	}
	return nil
}

func (a *app) printPXEUnknown(rows []pxeUnknownRow, noHeaders bool) error {
	if len(rows) == 0 {
		fmt.Fprintln(a.stderr, "No PXE server saw a MAC without a machine (a site lists them with unknown: local).")
		return nil
	}
	var table [][]string
	for _, u := range rows {
		table = append(table, []string{u.MAC, u.Site, a.pxeAgo(u.Seen), u.SeenBy, orDash(u.Address), orDash(u.Boot)})
	}
	return writeTable(a.stdout, []string{"MAC", "SITE", "SEEN", "BY", "ADDRESS", "BOOT"}, table, noHeaders)
}

// pxeLog prints the end of a machine's installation log, as a server
// of its site pushed it.
func (a *app) pxeLog(cmd *cobra.Command, c *pxe.Config, name string) error {
	name = strings.TrimPrefix(name, pxe.TypeName+"/")
	m := c.Machine(name)
	if m == nil {
		return a.pxeSet(cmd.Context()).missing(name)
	}
	data, version, err := a.pxeStore().Files.Read(cmd.Context(), pxe.LogPath(m.Site, name))
	if err != nil {
		return err
	}
	if version == "" {
		return fmt.Errorf("no installation log of %s: none of its installations reported one yet", name)
	}
	_, err = a.stdout.Write(data)
	return err
}

// --- disarm ------------------------------------------------------------------

// pxeDisarmed is a machine ump pxe disarm took the arm of.
type pxeDisarmed struct {
	Name    string `json:"name" yaml:"name"`
	Site    string `json:"site" yaml:"site"`
	Arm     string `json:"arm,omitempty" yaml:"arm,omitempty"`
	Mode    string `json:"mode,omitempty" yaml:"mode,omitempty"`
	By      string `json:"by,omitempty" yaml:"by,omitempty"`
	Pending bool   `json:"pending" yaml:"pending"`
}

func (a *app) newPXEDisarmCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "disarm NAME... [-o json|yaml]",
		Short: "Take back a machine's armed network boot",
		Long: `Remove the arm of each machine named: an armed rescue that has not booted
does not boot (the machine gets its standing mode, as pxe.yaml has it).
A machine without an arm is left as it is. An installation that is being
written is not disarmed (exit status 3): it ends with its report or at
its deadline. The PXE servers of the site take the change at their next
poll.`,
		Example: `  ump pxe disarm web-07
  ump pxe disarm web-07 db-01 -o json`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format := formatTable
			if output != "" {
				f, err := parseFormat(output)
				if err != nil || f != formatJSON && f != formatYAML {
					return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
				}
				format = f
			}
			ctx := cmd.Context()
			c, err := loadPXE()
			if err != nil {
				return err
			}
			// the arm is where it was written: at the machine's site, or
			// at the one it had before a hand edit moved it or took it out
			store := a.pxeStore()
			type target struct {
				name  string
				sites []string
			}
			var targets []target
			for _, arg := range args {
				name := strings.TrimPrefix(arg, pxe.TypeName+"/")
				sites, err := store.Holding(ctx, name)
				if err != nil {
					return err
				}
				m := c.Machine(name)
				if m == nil && len(sites) == 0 {
					return a.pxeSet(ctx).missing(name)
				}
				if m != nil && !slices.Contains(sites, m.Site) {
					sites = append([]string{m.Site}, sites...)
				}
				targets = append(targets, target{name, sites})
			}
			done := []pxeDisarmed{}
			for _, t := range targets {
				found := false
				for _, site := range t.sites {
					arm, _, err := store.Arm(ctx, site, t.name)
					if err != nil {
						return err
					}
					if arm == nil {
						continue
					}
					found = true
					o, _, err := store.Outcome(ctx, site, t.name)
					if err != nil {
						return err
					}
					if o != nil && o.Arm == arm.ID && o.Phase == pxe.PhaseWriting && a.now().Before(o.Deadline) {
						return withExit(exitAborted, fmt.Errorf("%s is being installed (since %s, by %s/%s); it ends with its report or at %s", t.name, a.fmtTime(o.At), site, o.Daemon, a.fmtTime(o.Deadline)))
					}
					if err := store.Disarm(ctx, site, t.name); err != nil {
						return err
					}
					d := pxeDisarmed{Name: t.name, Site: site, Arm: arm.ID, Mode: arm.Mode, By: arm.By, Pending: arm.Pending(o, a.now())}
					done = append(done, d)
					if format != formatTable {
						continue
					}
					by := ""
					if arm.By != "" {
						by = ", by " + arm.By
					}
					if d.Pending {
						fmt.Fprintf(a.stdout, "Disarmed %s: its armed %s (%s%s) does not boot.\n", t.name, arm.Mode, pxe.ShortID(arm.ID), by)
					} else {
						fmt.Fprintf(a.stdout, "Removed the arm of %s (%s %s%s), which was over.\n", t.name, arm.Mode, pxe.ShortID(arm.ID), by)
					}
				}
				if !found && format == formatTable {
					fmt.Fprintf(a.stdout, "%s has no arm.\n", t.name)
				}
			}
			if format != formatTable {
				return printEncoded(a.stdout, done, format)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "print what was disarmed as json or yaml")
	return cmd
}
