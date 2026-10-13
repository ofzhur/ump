// Package ovh integrates OVHcloud through its public API
// (https://api.ovh.com/): dedicated servers (/dedicated/server), VPS
// (/vps) and Public Cloud instances (/cloud/project/*/instance) are listed
// from one account, because one set of API keys covers all three.
//
// The client is hand-written: request signing is a SHA-1 over a few
// strings, and UMP needs only a few read calls. The setup wizard can
// request a consumer key for the user (POST /auth/credential), which is
// how OVH's own tools bootstrap access.
package ovh

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "ovh"

// Endpoints maps the endpoint names OVH's tools use to API base URLs.
var Endpoints = map[string]string{
	"ovh-eu": "https://eu.api.ovh.com/1.0",
	"ovh-ca": "https://ca.api.ovh.com/1.0",
	"ovh-us": "https://api.us.ovhcloud.com/1.0",
}

// DefaultAccessRules are the API permissions the wizard requests for a new
// consumer key: reading dedicated servers and Public Cloud projects for
// get and describe, and updating instances for rename.
var DefaultAccessRules = []AccessRule{
	{Method: "GET", Path: "/me"},
	{Method: "GET", Path: "/dedicated/server"},
	{Method: "GET", Path: "/dedicated/server/*"},
	{Method: "GET", Path: "/cloud/project"},
	{Method: "GET", Path: "/cloud/project/*"},
	{Method: "PUT", Path: "/cloud/project/*/instance/*"},
	{Method: "GET", Path: "/ip/*"},
	{Method: "POST", Path: "/ip/*/reverse"},
	{Method: "DELETE", Path: "/ip/*/reverse/*"},
	{Method: "PUT", Path: "/dedicated/server/*"},
	{Method: "POST", Path: "/dedicated/server/*/reboot"},
	{Method: "POST", Path: "/cloud/project/*/instance/*/reboot"},
	{Method: "POST", Path: "/cloud/project/*/instance/*/rescueMode"},
	{Method: "POST", Path: "/cloud/project/*/instance/*/start"},
	{Method: "POST", Path: "/cloud/project/*/instance/*/stop"},
	{Method: "GET", Path: "/vps"},
	{Method: "GET", Path: "/vps/*"},
	{Method: "PUT", Path: "/vps/*"},
	{Method: "POST", Path: "/vps/*/reboot"},
	{Method: "POST", Path: "/vps/*/start"},
	{Method: "POST", Path: "/vps/*/stop"},
	{Method: "GET", Path: "/vps/*/tasks"},
	{Method: "GET", Path: "/vps/*/tasks/*"},
	{Method: "POST", Path: "/vps/*/rebuild"},
}

const listParallelism = 8

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindMixed,
	Description:  "OVHcloud dedicated servers and Public Cloud instances",
	Capabilities: capabilities,
	Fields: []provider.Field{
		{
			Key:     "endpoint",
			Label:   "API endpoint",
			Help:    "ovh-eu (Europe), ovh-ca (Canada), ovh-us (United States), or a full URL",
			Default: "ovh-eu",
			Choices: []string{"ovh-eu", "ovh-ca", "ovh-us"},
		},
		{
			Key:      "application_key",
			Label:    "Application key",
			Help:     "create an application at https://eu.api.ovh.com/createApp/ (ca./us. for the other endpoints)",
			Required: true,
		},
		{
			Key:      "application_secret",
			Label:    "Application secret",
			Secret:   true,
			Required: true,
		},
		{
			Key:        "consumer_key",
			Label:      "Consumer key",
			Help:       "leave empty to request a new, read-only key now",
			Secret:     true,
			Required:   true,
			Deferrable: true,
		},
	},
	New:      newProvider,
	Complete: complete,
}

var capabilities = []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapReverseDNS, provider.CapPower, provider.CapRescue}

// Provider is a configured OVHcloud account.
type Provider struct {
	account string
	client  *Client
	logf    func(format string, args ...any)
}

func newProvider(account string, settings map[string]string, opts provider.Options) (provider.Provider, error) {
	base, err := ResolveEndpoint(settings["endpoint"])
	if err != nil {
		return nil, err
	}
	return &Provider{
		account: account,
		client: &Client{
			BaseURL:     base,
			AppKey:      settings["application_key"],
			AppSecret:   settings["application_secret"],
			ConsumerKey: settings["consumer_key"],
			HTTP:        opts.Client(),
			UserAgent:   opts.UserAgent,
		},
		logf: opts.Logf,
	}, nil
}

// ResolveEndpoint turns an endpoint name (ovh-eu, ...) or a URL into an API
// base URL.
func ResolveEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		endpoint = "ovh-eu"
	}
	if u, ok := Endpoints[endpoint]; ok {
		return u, nil
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return strings.TrimSuffix(endpoint, "/"), nil
	}
	names := make([]string, 0, len(Endpoints))
	for n := range Endpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", &provider.FieldError{Type: TypeName, Key: "endpoint", Msg: "must be one of " + strings.Join(names, ", ") + " or a URL"}
}

// complete requests a consumer key when the user left it empty.
func complete(ctx context.Context, ui provider.Prompter, settings map[string]string, opts provider.Options) error {
	if strings.TrimSpace(settings["consumer_key"]) != "" {
		return nil
	}
	base, err := ResolveEndpoint(settings["endpoint"])
	if err != nil {
		return err
	}
	c := &Client{BaseURL: base, AppKey: settings["application_key"], HTTP: opts.Client(), UserAgent: opts.UserAgent}
	ui.Printf("Requesting a consumer key from %s ...\n", base)
	cred, err := c.RequestConsumerKey(ctx, DefaultAccessRules, "")
	if err != nil {
		return err
	}
	ui.Printf("\nOpen this URL, log in to your OVHcloud account and validate the key:\n\n    %s\n\n", cred.ValidationURL)
	if _, err := ui.Line("Press Enter when the key is validated", ""); err != nil {
		return err
	}
	settings["consumer_key"] = cred.ConsumerKey
	return nil
}

// Type implements provider.Provider.
func (p *Provider) Type() string { return TypeName }

// Account implements provider.Provider.
func (p *Provider) Account() string { return p.account }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() []provider.Capability { return capabilities }

func (p *Provider) projectID(m provider.Machine) (string, error) {
	id := m.Labels["project_id"]
	if id == "" {
		return "", fmt.Errorf("%s: instance %s has no project_id label", TypeName, m.ID)
	}
	return id, nil
}

// isVPS reports whether a machine is a VPS (the listing labels them).
func isVPS(m provider.Machine) bool { return m.Labels["family"] == "vps" }

// RescueTakesNoKey implements provider.KeylessRescue: a VPS boots its
// rescue system with a password OVH mails, nothing else.
func (p *Provider) RescueTakesNoKey(m provider.Machine) bool { return isVPS(m) }

// vpsTaskWait is how long a VPS gets to finish its running tasks before
// an action; OVH refuses actions while a task runs. A rebuild takes
// longer, vpsRebuildWait.
var (
	vpsTaskWait    = 3 * time.Minute
	vpsRebuildWait = 30 * time.Minute
)

// vpsPostInstall runs once on the first boot of a rebuilt VPS: OVH puts
// the key into its default user's authorized_keys and keeps root closed,
// UMP needs root over SSH.
const vpsPostInstall = `#!/bin/bash
mkdir -p -m 700 /root/.ssh
cat /home/*/.ssh/authorized_keys >> /root/.ssh/authorized_keys 2>/dev/null
chmod 600 /root/.ssh/authorized_keys
mkdir -p /etc/ssh/sshd_config.d
printf 'PermitRootLogin prohibit-password\n' > /etc/ssh/sshd_config.d/00-ump-root.conf
systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || true
`

// InstallStock implements provider.StockInstaller for a VPS: OVH
// rebuilds it with its newest Debian image (Ubuntu when there is none)
// and UMP's key, and a script of the first boot opens root to that key.
func (p *Provider) InstallStock(ctx context.Context, m provider.Machine, keys []provider.PublicKey) (*provider.StockInstall, error) {
	if !isVPS(m) {
		return nil, &provider.APIError{Provider: TypeName, Message: "only a VPS can be reinstalled with a stock image through the OVHcloud API"}
	}
	if len(keys) == 0 {
		return nil, &provider.APIError{Provider: TypeName, Message: "a stock installation needs a key for root"}
	}
	img, err := p.stockImage(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
		return nil, err
	}
	if _, err := p.client.RebuildVPS(ctx, m.ID, map[string]any{
		"imageId": img.ID, "publicSshKey": keys[0].Data, "doNotSendPassword": true, "postInstallScript": vpsPostInstall,
	}); err != nil {
		return nil, err
	}
	if err := p.waitVPSIdle(ctx, m.ID, vpsRebuildWait); err != nil {
		return nil, err
	}
	return &provider.StockInstall{Image: img.Name, Address: m.IPv4}, nil
}

// stockImage picks the image a VPS is rebuilt with: the newest Debian,
// else the newest Ubuntu, among the plain ones (no Plesk, Docker, ...).
func (p *Provider) stockImage(ctx context.Context, name string) (*VPSImage, error) {
	ids, err := p.client.VPSImages(ctx, name)
	if err != nil {
		return nil, err
	}
	var best *VPSImage
	bestRank := -1.0
	for _, id := range ids {
		img, err := p.client.GetVPSImage(ctx, name, id)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(img.Name)
		if len(fields) != 2 {
			continue
		}
		var rank float64
		switch fields[0] {
		case "Debian":
			rank = 1000
		case "Ubuntu":
			rank = 0
		default:
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		rank += v
		if rank > bestRank {
			bestRank, best = rank, img
		}
	}
	if best == nil {
		return nil, &provider.APIError{Provider: TypeName, Message: "no plain Debian or Ubuntu image is available for VPS " + name}
	}
	return best, nil
}

// vpsTaskPoll is how often the tasks are looked at while waiting.
var vpsTaskPoll = 3 * time.Second

// waitVPSIdle waits until the VPS has no task in progress.
func (p *Provider) waitVPSIdle(ctx context.Context, name string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		ids, err := p.client.VPSTasks(ctx, name)
		if err != nil {
			return err
		}
		busy := false
		for _, id := range ids {
			t, err := p.client.GetVPSTask(ctx, name, id)
			if err != nil {
				return err
			}
			switch t.State {
			case "todo", "doing", "waitingAck", "paused":
				busy = true
			}
		}
		if !busy {
			return nil
		}
		if time.Now().After(deadline) {
			return &provider.APIError{Provider: TypeName, Message: "VPS " + name + " still has tasks running after " + wait.String()}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(vpsTaskPoll):
		}
	}
}

var errDedicatedPower = &provider.APIError{Provider: TypeName,
	Message: "dedicated servers cannot be switched on or off through the OVHcloud API; use the control panel (or reboot)"}

// PowerState implements provider.Power.
func (p *Provider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	if isVPS(m) {
		v, err := p.client.GetVPS(ctx, m.ID)
		if err != nil {
			return provider.PowerUnknown, err
		}
		return vpsPower(v.State), nil
	}
	if m.Kind != provider.KindCloud {
		s, err := p.client.GetDedicatedServer(ctx, m.ID)
		if err != nil {
			return provider.PowerUnknown, err
		}
		switch s.PowerState {
		case "poweredOn":
			return provider.PowerOn, nil
		case "poweredOff":
			return provider.PowerOff, nil
		}
		return provider.PowerUnknown, nil
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return provider.PowerUnknown, err
	}
	in, err := p.client.GetInstance(ctx, projectID, m.ID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return instancePower(in.Status), nil
}

func vpsPower(state string) provider.PowerState {
	switch strings.ToLower(state) {
	case "running", "rescued":
		return provider.PowerOn
	case "stopped":
		return provider.PowerOff
	}
	return provider.PowerUnknown
}

func instancePower(status string) provider.PowerState {
	switch strings.ToUpper(status) {
	case "ACTIVE", "RESCUE", "RESCUED":
		return provider.PowerOn
	case "SHUTOFF", "STOPPED", "PAUSED", "SUSPENDED", "SHELVED", "SHELVED_OFFLOADED":
		return provider.PowerOff
	}
	return provider.PowerUnknown
}

// Reboot implements provider.Power. Dedicated servers know one reboot
// (a hardware reset, started as a task); instances reboot soft or hard,
// and a power cycle is stop then start.
func (p *Provider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	if isVPS(m) {
		if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
			return err
		}
		if mode == provider.RebootPowerCycle {
			if _, err := p.client.VPSAction(ctx, m.ID, "stop"); err != nil {
				return err
			}
			if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
				return err
			}
			_, err := p.client.VPSAction(ctx, m.ID, "start")
			return err
		}
		_, err := p.client.VPSAction(ctx, m.ID, "reboot")
		return err
	}
	if m.Kind != provider.KindCloud {
		_, err := p.client.RebootDedicated(ctx, m.ID)
		return err
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return err
	}
	switch mode {
	case provider.RebootSoft:
		return p.client.RebootInstance(ctx, projectID, m.ID, "soft")
	case provider.RebootPowerCycle:
		if err := p.client.InstanceAction(ctx, projectID, m.ID, "stop"); err != nil {
			return err
		}
		return p.client.InstanceAction(ctx, projectID, m.ID, "start")
	}
	return p.client.RebootInstance(ctx, projectID, m.ID, "hard")
}

// PowerOn implements provider.Power for instances.
func (p *Provider) PowerOn(ctx context.Context, m provider.Machine) error {
	if isVPS(m) {
		_, err := p.client.VPSAction(ctx, m.ID, "start")
		return err
	}
	if m.Kind != provider.KindCloud {
		return errDedicatedPower
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return err
	}
	return p.client.InstanceAction(ctx, projectID, m.ID, "start")
}

// PowerOff implements provider.Power for instances (OVH knows one stop).
func (p *Provider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	if isVPS(m) {
		_, err := p.client.VPSAction(ctx, m.ID, "stop")
		return err
	}
	if m.Kind != provider.KindCloud {
		return errDedicatedPower
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return err
	}
	return p.client.InstanceAction(ctx, projectID, m.ID, "stop")
}

// RescueEnabled implements provider.Rescue: a dedicated server whose boot
// option is of type rescue, or an instance in a rescue status.
func (p *Provider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	if isVPS(m) {
		v, err := p.client.GetVPS(ctx, m.ID)
		if err != nil {
			return false, err
		}
		return v.NetbootMode == "rescue", nil
	}
	if m.Kind != provider.KindCloud {
		s, err := p.client.GetDedicatedServer(ctx, m.ID)
		if err != nil {
			return false, err
		}
		if s.BootID == 0 {
			return false, nil
		}
		b, err := p.client.GetBoot(ctx, m.ID, s.BootID)
		if err != nil {
			return false, err
		}
		return b.BootType == "rescue", nil
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return false, err
	}
	in, err := p.client.GetInstance(ctx, projectID, m.ID)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToUpper(in.Status), "RESCU"), nil
}

// EnableRescue implements provider.Rescue. A dedicated server gets the
// rescue boot option and the first key as rescue SSH key (OVH mails the
// password to the rescue mail otherwise); the caller reboots it. An
// instance is put into rescue mode by OVH itself, which reboots it.
func (p *Provider) EnableRescue(ctx context.Context, m provider.Machine, opts provider.RescueOptions) (*provider.RescueInfo, error) {
	if isVPS(m) {
		// a VPS boots its rescue system when its netboot mode says so; the
		// change is a task, and nothing else is accepted until it is done;
		// the API takes no key, OVH mails the password
		if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
			return nil, err
		}
		if err := p.client.UpdateVPS(ctx, m.ID, map[string]any{"netbootMode": "rescue"}); err != nil {
			return nil, err
		}
		if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
			return nil, err
		}
		return &provider.RescueInfo{Address: m.IPv4, User: "root",
			Note: "OVHcloud mails the rescue password of a VPS; no SSH key can be set through the API, so UMP cannot log in by itself"}, nil
	}
	if m.Kind != provider.KindCloud {
		ids, err := p.client.ListBoots(ctx, m.ID, "rescue")
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, &provider.APIError{Provider: TypeName, Message: "server " + m.ID + " has no rescue boot option"}
		}
		update := map[string]any{"bootId": ids[0]}
		info := &provider.RescueInfo{Address: m.IPv4, User: "root"}
		if len(opts.Keys) > 0 {
			update["rescueSshKey"] = opts.Keys[0].Data
			if len(opts.Keys) > 1 {
				info.Note = "OVHcloud takes one rescue SSH key; the first one was used"
			}
		} else {
			info.Note = "OVHcloud mails the rescue password to the server's rescue mail address"
		}
		if err := p.client.UpdateDedicated(ctx, m.ID, update); err != nil {
			return nil, err
		}
		return info, nil
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return nil, err
	}
	password, err := p.client.SetInstanceRescue(ctx, projectID, m.ID, true)
	if err != nil {
		return nil, err
	}
	return &provider.RescueInfo{Address: m.IPv4, User: "root", Password: password, Reboots: true,
		Note: "OVHcloud reboots the instance into rescue mode itself; the SSH keys of the instance apply"}, nil
}

// DisableRescue implements provider.Rescue: the hard disk boot option
// for a dedicated server, rescue mode off for an instance (which OVH
// reboots).
func (p *Provider) DisableRescue(ctx context.Context, m provider.Machine) error {
	if isVPS(m) {
		if err := p.waitVPSIdle(ctx, m.ID, vpsTaskWait); err != nil {
			return err
		}
		if err := p.client.UpdateVPS(ctx, m.ID, map[string]any{"netbootMode": "local"}); err != nil {
			return err
		}
		return p.waitVPSIdle(ctx, m.ID, vpsTaskWait)
	}
	if m.Kind != provider.KindCloud {
		ids, err := p.client.ListBoots(ctx, m.ID, "harddisk")
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return &provider.APIError{Provider: TypeName, Message: "server " + m.ID + " has no hard disk boot option"}
		}
		return p.client.UpdateDedicated(ctx, m.ID, map[string]any{"bootId": ids[0]})
	}
	projectID, err := p.projectID(m)
	if err != nil {
		return err
	}
	_, err = p.client.SetInstanceRescue(ctx, projectID, m.ID, false)
	return err
}

// blocks returns the IP blocks of a machine, the unit the OVH reverse DNS
// API works with: a dedicated server's routed blocks, or /32 and /64
// blocks around an instance's addresses.
func (p *Provider) blocks(ctx context.Context, m provider.Machine) (primary string, blocks []string, err error) {
	if isVPS(m) {
		ips, err := p.client.VPSIPs(ctx, m.ID)
		if err != nil {
			return "", nil, err
		}
		for _, ip := range ips {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				continue
			}
			if addr.Is4() {
				if primary == "" {
					primary = addr.String()
				}
				blocks = append(blocks, addr.String()+"/32")
			} else if pfx, err := addr.Prefix(64); err == nil {
				blocks = append(blocks, pfx.String())
			}
		}
		return primary, blocks, nil
	}
	if m.Kind != provider.KindCloud {
		s, err := p.client.GetDedicatedServer(ctx, m.ID)
		if err != nil {
			return "", nil, err
		}
		blocks, err := p.client.DedicatedServerIPs(ctx, m.ID)
		if err != nil {
			return "", nil, err
		}
		return s.IP, blocks, nil
	}
	projectID := m.Labels["project_id"]
	if projectID == "" {
		return "", nil, fmt.Errorf("%s: instance %s has no project_id label", TypeName, m.ID)
	}
	in, err := p.client.GetInstance(ctx, projectID, m.ID)
	if err != nil {
		return "", nil, err
	}
	for _, ip := range in.IPAddresses {
		if ip.Type != "" && ip.Type != "public" {
			continue
		}
		addr, err := netip.ParseAddr(ip.IP)
		if err != nil {
			continue
		}
		switch {
		case addr.Is4():
			if primary == "" {
				primary = addr.String()
			}
			blocks = append(blocks, addr.String()+"/32")
		default:
			if pfx, err := addr.Prefix(64); err == nil {
				blocks = append(blocks, pfx.String())
			}
		}
	}
	return primary, blocks, nil
}

// PTRs implements provider.ReverseDNS.
func (p *Provider) PTRs(ctx context.Context, m provider.Machine) ([]provider.PTR, error) {
	primary, blocks, err := p.blocks(ctx, m)
	if err != nil {
		return nil, err
	}
	byIP := map[string]string{}
	for _, block := range blocks {
		ips, err := p.client.ListReverse(ctx, block)
		if err != nil {
			if isStatus(err, http.StatusNotFound) {
				continue
			}
			return nil, err
		}
		for _, ip := range ips {
			r, err := p.client.GetReverse(ctx, block, ip)
			if err != nil {
				return nil, err
			}
			byIP[ip] = strings.TrimSuffix(r.Reverse, ".")
		}
	}
	var out []provider.PTR
	if primary != "" {
		out = append(out, provider.PTR{IP: primary, Name: byIP[primary]})
		delete(byIP, primary)
	}
	others := make([]string, 0, len(byIP))
	for ip := range byIP {
		others = append(others, ip)
	}
	sort.Strings(others)
	for _, ip := range others {
		out = append(out, provider.PTR{IP: ip, Name: byIP[ip]})
	}
	return out, nil
}

// SetPTR implements provider.ReverseDNS. OVH checks that the name
// resolves back to the address before it accepts the entry.
func (p *Provider) SetPTR(ctx context.Context, m provider.Machine, ip netip.Addr, name string) error {
	_, blocks, err := p.blocks(ctx, m)
	if err != nil {
		return err
	}
	var block string
	for _, b := range blocks {
		if pfx, err := netip.ParsePrefix(b); err == nil && pfx.Contains(ip) {
			block = b
			break
		}
	}
	if block == "" {
		return fmt.Errorf("%s: address %s does not belong to %s", TypeName, ip, m.ID)
	}
	if name == "" {
		err := p.client.DeleteReverse(ctx, block, ip.String())
		if isStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return p.client.SetReverse(ctx, block, ip.String(), name)
}

func isStatus(err error, status int) bool {
	var apiErr *provider.APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// Rename implements provider.Renamer for Public Cloud instances. Dedicated
// servers cannot be renamed through the API: their display name is
// read-only in every public API model (the IAM resource included), so a
// clear error is better than a call that changes nothing.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	if isVPS(m) {
		if err := p.client.UpdateVPS(ctx, m.ID, map[string]any{"displayName": name}); err != nil {
			return provider.Machine{}, err
		}
		v, err := p.client.GetVPS(ctx, m.ID)
		if err != nil {
			return provider.Machine{}, err
		}
		return p.vpsMachine(*v, []VPSIP{{IPAddress: m.IPv4, Version: 4}, {IPAddress: m.IPv6, Version: 6}}), nil
	}
	if m.Kind != provider.KindCloud {
		return provider.Machine{}, &provider.APIError{Provider: TypeName,
			Message: "dedicated servers cannot be renamed through the OVHcloud API (the display name is read-only there); set it in the OVHcloud control panel"}
	}
	projectID := m.Labels["project_id"]
	if projectID == "" {
		return provider.Machine{}, fmt.Errorf("%s: instance %s has no project_id label", TypeName, m.ID)
	}
	if err := p.client.UpdateInstanceName(ctx, projectID, m.ID, name); err != nil {
		return provider.Machine{}, err
	}
	in, err := p.client.GetInstance(ctx, projectID, m.ID)
	if err != nil {
		return provider.Machine{}, err
	}
	return p.instanceMachine(CloudProject{ProjectID: projectID, Description: m.Labels["project"]}, in.Instance), nil
}

// Describe implements provider.Describer. A dedicated server is described
// from its service name (the machine ID); a Public Cloud instance needs
// the project, which the listing put into the "project_id" label.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	if isVPS(m) {
		return p.describeVPS(ctx, m.ID)
	}
	if m.Kind == provider.KindCloud {
		projectID := m.Labels["project_id"]
		if projectID == "" {
			return nil, fmt.Errorf("%s: instance %s has no project_id label", TypeName, m.ID)
		}
		return p.describeInstance(ctx, projectID, m.Labels["project"], m.ID)
	}
	return p.describeDedicated(ctx, m.ID)
}

func (p *Provider) describeVPS(ctx context.Context, name string) (*provider.Details, error) {
	v, err := p.client.GetVPS(ctx, name)
	if err != nil {
		return nil, err
	}
	ips, err := p.vpsAddresses(ctx, name)
	if err != nil {
		return nil, err
	}
	d := &provider.Details{Machine: p.vpsMachine(*v, ips)}
	server := provider.Section{Name: "Server"}
	server.AddLabeled("name", "Name", v.Name)
	server.AddLabeled("display_name", "Display name", v.DisplayName)
	server.AddLabeled("state", "State", v.State)
	server.AddLabeled("zone", "Zone", v.Zone)
	server.AddLabeled("cluster", "Cluster", v.Cluster)
	server.AddLabeled("netboot", "Netboot", v.NetbootMode)
	server.AddLabeled("offer_type", "Offer type", v.OfferType)
	d.Sections = append(d.Sections, server)
	model := provider.Section{Name: "Model"}
	model.AddLabeled("model", "Model", v.Model.Name)
	model.AddLabeled("offer", "Offer", v.Model.Offer)
	model.AddLabeled("version", "Version", v.Model.Version)
	model.AddLabeled("vcores", "vCores", strconv.Itoa(v.Vcore))
	model.AddLabeled("memory", "Memory", fmt.Sprintf("%d MB", v.MemoryLimit))
	model.AddLabeled("disk", "Disk", fmt.Sprintf("%d GB", v.Model.Disk))
	d.Sections = append(d.Sections, model)
	addrs := provider.Section{Name: "Addresses"}
	for _, ip := range ips {
		what := ip.Type
		if what == "" {
			what = "address"
		}
		if ip.Reverse != "" {
			what += ", reverse " + ip.Reverse
		}
		addrs.AddLabeled("ip_"+ip.IPAddress, ip.IPAddress, what)
	}
	d.Sections = append(d.Sections, addrs)
	if si, err := p.client.GetVPSServiceInfos(ctx, name); err == nil {
		svc := provider.Section{Name: "Service"}
		svc.AddLabeled("created", "Created", si.Creation)
		svc.AddLabeled("expires", "Expires", si.Expiration)
		svc.AddLabeled("renewal", "Renewal", si.RenewalType)
		d.Sections = append(d.Sections, svc)
	}
	return d, nil
}

// vpsAddresses returns the addresses of a VPS with their details.
func (p *Provider) vpsAddresses(ctx context.Context, name string) ([]VPSIP, error) {
	ips, err := p.client.VPSIPs(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]VPSIP, 0, len(ips))
	for _, ip := range ips {
		detail, err := p.client.GetVPSIP(ctx, name, ip)
		if err != nil {
			// the address alone is better than nothing
			v := 4
			if strings.Contains(ip, ":") {
				v = 6
			}
			out = append(out, VPSIP{IPAddress: ip, Version: v})
			continue
		}
		out = append(out, *detail)
	}
	return out, nil
}

func (p *Provider) describeDedicated(ctx context.Context, name string) (*provider.Details, error) {
	s, err := p.client.GetDedicatedServer(ctx, name)
	if err != nil {
		return nil, err
	}
	blocks, err := p.client.DedicatedServerIPs(ctx, name)
	if err != nil && !isNotGranted(err) {
		return nil, err
	}
	d := &provider.Details{Machine: p.dedicatedMachine(*s, blocks)}

	server := provider.Section{Name: "Server"}
	var display any
	if s.IAM != nil && s.IAM.DisplayName != "" {
		display = s.IAM.DisplayName
	}
	server.Add("service_name", s.Name).Add("display_name", display).Add("server_id", s.ServerID).
		Add("commercial_range", s.CommercialRange).Add("os", s.OS).Add("state", s.State).Add("power_state", s.PowerState).
		Add("monitoring", s.Monitoring).Add("support_level", s.SupportLevel).Add("link_speed_mbps", s.LinkSpeed).
		Add("reverse", s.Reverse).Add("professional_use", s.ProfessionalUse).Add("no_intervention", s.NoIntervention)

	location := provider.Section{Name: "Location"}
	location.Add("datacenter", s.Datacenter).Add("region", s.Region).Add("availability_zone", s.AvailabilityZone).Add("rack", s.Rack)

	boot := provider.Section{Name: "Boot"}
	boot.Add("boot_id", s.BootID)
	if s.BootID != 0 {
		if b, err := p.client.GetBoot(ctx, name, s.BootID); err == nil {
			boot.Add("boot_type", b.BootType).Add("description", b.Description).Add("kernel", b.Kernel)
		} else {
			boot.Add("error", err.Error())
		}
	}
	boot.Add("root_device", s.RootDevice).Add("rescue_mail", s.RescueMail)

	addresses := provider.Section{Name: "Addresses"}
	if blocks == nil {
		blocks = []string{}
	}
	addresses.Add("ip", s.IP).AddLabeled("ip_blocks", "IP blocks", blocks)
	d.Sections = append(d.Sections, server, location, boot, addresses)

	if si, err := p.client.GetServiceInfos(ctx, name); err == nil {
		renewal := "manual"
		if si.Renew != nil && si.Renew.Automatic {
			renewal = "automatic"
		}
		service := provider.Section{Name: "Service"}
		service.Add("creation", si.Creation).Add("expiration", si.Expiration).Add("renewal", renewal).
			Add("renewal_type", si.RenewalType).Add("status", si.Status)
		d.Sections = append(d.Sections, service)
	} else {
		d.Sections = append(d.Sections, provider.ErrorSection("Service", err))
	}

	if hw, err := p.client.GetHardware(ctx, name); err == nil {
		disks := []string{}
		for _, g := range hw.DiskGroups {
			desc := g.Description
			if desc == "" && g.DiskSize != nil {
				desc = fmt.Sprintf("%d x %s %s", g.NumberOfDisks, g.DiskSize.Bytes(), g.DiskType)
			}
			if g.RaidController != "" {
				desc += " (" + g.RaidController + ")"
			}
			disks = append(disks, desc)
		}
		var memory any
		if hw.MemorySize != nil {
			memory = hw.MemorySize.Bytes()
		}
		hardware := provider.Section{Name: "Hardware"}
		hardware.Add("description", hw.Description).Add("memory", memory).
			Add("processor", fmt.Sprintf("%d x %s", hw.NumberOfProcessors, hw.ProcessorName)).
			Add("cores", hw.NumberOfProcessors*hw.CoresPerProcessor).Add("threads", hw.NumberOfProcessors*hw.ThreadsPerProcessor).
			Add("disks", disks).Add("boot_mode", hw.BootMode).Add("form_factor", hw.FormFactor)
		d.Sections = append(d.Sections, hardware)
	} else {
		d.Sections = append(d.Sections, provider.ErrorSection("Hardware", err))
	}
	return d, nil
}

func (p *Provider) describeInstance(ctx context.Context, projectID, projectName, id string) (*provider.Details, error) {
	in, err := p.client.GetInstance(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	project := CloudProject{ProjectID: projectID, Description: projectName}
	d := &provider.Details{Machine: p.instanceMachine(project, in.Instance)}

	billing := "hourly"
	if in.MonthlyBilling != nil {
		billing = "monthly (" + in.MonthlyBilling.Status + ")"
	}
	instance := provider.Section{Name: "Instance"}
	instance.Add("id", in.ID).Add("name", in.Name).Add("status", in.Status).Add("region", in.Region).
		Add("created", in.Created).Add("plan_code", in.PlanCode).Add("billing", billing)

	flavor := provider.Section{Name: "Flavor"}
	if in.Flavor != nil {
		flavor.Add("name", in.Flavor.Name).Add("type", in.Flavor.Type).Add("vcpus", in.Flavor.VCPUs).
			Add("ram", provider.Bytes(in.Flavor.RAM)<<20).Add("disk", provider.Bytes(in.Flavor.Disk)<<30).Add("os_type", in.Flavor.OSType)
	} else {
		flavor.Add("id", in.FlavorID)
	}

	image := provider.Section{Name: "Image"}
	if in.Image != nil {
		image.Add("name", in.Image.Name).Add("type", in.Image.Type).Add("user", in.Image.User).Add("visibility", in.Image.Visibility)
	} else {
		image.Add("id", in.ImageID)
	}

	public4, public6, private := []string{}, []string{}, []string{}
	for _, ip := range in.IPAddresses {
		switch {
		case ip.Type == "private":
			private = append(private, fmt.Sprintf("%s (network %s)", ip.IP, ip.NetworkID))
		case ip.Version == 6:
			public6 = append(public6, ip.IP)
		default:
			public4 = append(public4, ip.IP)
		}
	}
	addresses := provider.Section{Name: "Addresses"}
	addresses.AddLabeled("public_ipv4", "Public IPv4", public4).AddLabeled("public_ipv6", "Public IPv6", public6).Add("private", private)

	var key any
	if in.SSHKey != nil {
		key = in.SSHKey.Name
	}
	access := provider.Section{Name: "Access"}
	access.AddLabeled("ssh_key", "SSH key", key)

	proj := provider.Section{Name: "Project"}
	proj.Add("name", projectName).Add("id", projectID)

	d.Sections = append(d.Sections, instance, flavor, image, addresses, access, proj)
	return d, nil
}

// List implements provider.Inventory. Dedicated servers and Public Cloud
// instances are listed concurrently. A consumer key that is not allowed to
// see one of the two families (403 NOT_GRANTED_CALL on the list call) is
// tolerated: that family is skipped, and mentioned in verbose output.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	var (
		wg                          sync.WaitGroup
		dedicated, cloud, vps       []provider.Machine
		dedErr, cloudErr, vpsErr    error
		skippedDed, skipCl, skipVPS bool
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		vps, vpsErr = p.listVPS(ctx)
		if isNotGranted(vpsErr) {
			skipVPS, vpsErr = true, nil
		}
	}()
	go func() {
		defer wg.Done()
		dedicated, dedErr = p.listDedicated(ctx)
		if isNotGranted(dedErr) {
			skippedDed, dedErr = true, nil
		}
	}()
	go func() {
		defer wg.Done()
		cloud, cloudErr = p.listCloud(ctx)
		if isNotGranted(cloudErr) {
			skipCl, cloudErr = true, nil
		}
	}()
	wg.Wait()
	if dedErr != nil {
		return nil, dedErr
	}
	if cloudErr != nil {
		return nil, cloudErr
	}
	if vpsErr != nil {
		return nil, vpsErr
	}
	if skippedDed && skipCl && skipVPS {
		return nil, &provider.APIError{Provider: TypeName, Status: http.StatusForbidden, Code: "NOT_GRANTED_CALL",
			Message: "the consumer key may list neither dedicated servers, VPS nor Public Cloud projects (check its access rules)"}
	}
	if p.logf != nil {
		if skippedDed {
			p.logf("ovh: consumer key is not allowed to list dedicated servers; skipped")
		}
		if skipCl {
			p.logf("ovh: consumer key is not allowed to list Public Cloud projects; skipped")
		}
		if skipVPS {
			p.logf("ovh: consumer key is not allowed to list VPS (GET /vps); skipped, request a key with the /vps rules (ump config)")
		}
	}
	out := append(dedicated, vps...)
	return append(out, cloud...), nil
}

func (p *Provider) listVPS(ctx context.Context) ([]provider.Machine, error) {
	names, err := p.client.ListVPS(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Machine, len(names))
	errs := make([]error, len(names))
	sem := make(chan struct{}, listParallelism)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := p.client.GetVPS(ctx, name)
			if err != nil {
				errs[i] = err
				return
			}
			ips, err := p.vpsAddresses(ctx, name)
			if err != nil && !isNotGranted(err) {
				errs[i] = err
				return
			}
			out[i] = p.vpsMachine(*v, ips)
		}(i, name)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// vpsZone shortens OVH's zone sentence ("Region OpenStack: os-de2") to
// the zone itself.
func vpsZone(zone string) string {
	if _, after, ok := strings.Cut(zone, ":"); ok {
		zone = after
	}
	return strings.ToLower(strings.TrimSpace(zone))
}

func (p *Provider) vpsMachine(v VPS, ips []VPSIP) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       v.Name,
		Name:     v.Name,
		Kind:     provider.KindCloud,
		Location: vpsZone(v.Zone),
		Product:  v.Model.Name,
		Status:   strings.ToLower(v.State),
		Labels:   map[string]string{"family": "vps"},
	}
	if strings.TrimSpace(v.DisplayName) != "" {
		m.Name = v.DisplayName
	} else if v.IAM != nil && strings.TrimSpace(v.IAM.DisplayName) != "" {
		m.Name = v.IAM.DisplayName
	}
	if m.Product == "" {
		m.Product = v.Model.Offer
	}
	if v.NetbootMode == "rescue" {
		m.Status = "rescue"
	}
	// the primary address first, then any other
	for _, pass := range []bool{true, false} {
		for _, ip := range ips {
			if ip.IPAddress == "" || (ip.Type == "primary") != pass {
				continue
			}
			switch {
			case ip.Version == 4 && m.IPv4 == "":
				m.IPv4 = ip.IPAddress
			case ip.Version == 6 && m.IPv6 == "":
				m.IPv6 = ip.IPAddress
			}
		}
	}
	return m
}

func isNotGranted(err error) bool {
	var apiErr *provider.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden && apiErr.Code == "NOT_GRANTED_CALL"
}

func (p *Provider) listDedicated(ctx context.Context) ([]provider.Machine, error) {
	names, err := p.client.ListDedicatedServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Machine, len(names))
	errs := make([]error, len(names))
	sem := make(chan struct{}, listParallelism)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := p.client.GetDedicatedServer(ctx, name)
			if err != nil {
				errs[i] = err
				return
			}
			ips, err := p.client.DedicatedServerIPs(ctx, name)
			if err != nil && !isNotGranted(err) {
				errs[i] = err
				return
			}
			out[i] = p.dedicatedMachine(*s, ips)
		}(i, name)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (p *Provider) dedicatedMachine(s DedicatedServer, ipBlocks []string) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       s.Name,
		Name:     s.Name,
		Kind:     provider.KindBareMetal,
		IPv4:     s.IP,
		Location: strings.ToLower(s.Datacenter),
		Product:  s.CommercialRange,
		Status:   strings.ToLower(s.State),
	}
	if s.IAM != nil && strings.TrimSpace(s.IAM.DisplayName) != "" {
		m.Name = s.IAM.DisplayName
	}
	if m.Location == "" {
		m.Location = strings.ToLower(s.Region)
	}
	if s.PowerState == "poweredOff" {
		m.Status = "off"
	}
	for _, b := range ipBlocks {
		if strings.Contains(b, ":") {
			m.IPv6 = b
			break
		}
	}
	return m
}

func (p *Provider) listCloud(ctx context.Context) ([]provider.Machine, error) {
	ids, err := p.client.ListCloudProjects(ctx)
	if err != nil {
		return nil, err
	}
	results := make([][]provider.Machine, len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, listParallelism)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			project, err := p.client.GetCloudProject(ctx, id)
			if err != nil {
				errs[i] = err
				return
			}
			instances, err := p.client.ListInstances(ctx, id)
			if err != nil {
				errs[i] = err
				return
			}
			for _, in := range instances {
				results[i] = append(results[i], p.instanceMachine(*project, in))
			}
		}(i, id)
	}
	wg.Wait()
	var out []provider.Machine
	for i := range ids {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

func (p *Provider) instanceMachine(project CloudProject, in Instance) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       in.ID,
		Name:     in.Name,
		Kind:     provider.KindCloud,
		Location: strings.ToLower(in.Region),
		Product:  in.FlavorID,
		Status:   strings.ToLower(in.Status),
		Labels: map[string]string{
			"project":    project.Description,
			"project_id": project.ProjectID,
		},
	}
	if m.Labels["project"] == "" {
		m.Labels["project"] = project.ProjectID
	}
	if in.PlanCode != "" {
		// planCode is "<flavor>.<billing>", e.g. "b3-8.consumption".
		m.Product = strings.SplitN(in.PlanCode, ".", 2)[0]
	}
	for _, ip := range in.IPAddresses {
		if ip.Type != "" && ip.Type != "public" {
			continue
		}
		switch {
		case ip.Version == 4 && m.IPv4 == "":
			m.IPv4 = ip.IP
		case ip.Version == 6 && m.IPv6 == "":
			m.IPv6 = ip.IP
		}
	}
	return m
}

// VPS is GET /vps/{serviceName}.
type VPS struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	Zone        string `json:"zone"`
	Cluster     string `json:"cluster"`
	NetbootMode string `json:"netbootMode"`
	OfferType   string `json:"offerType"`
	Vcore       int    `json:"vcore"`
	MemoryLimit int    `json:"memoryLimit"`
	Model       struct {
		Name    string `json:"name"`
		Offer   string `json:"offer"`
		Version string `json:"version"`
		Vcore   int    `json:"vcore"`
		Memory  int    `json:"memory"`
		Disk    int    `json:"disk"`
	} `json:"model"`
	IAM *struct {
		DisplayName string `json:"displayName"`
	} `json:"iam"`
}

// VPSIP is GET /vps/{serviceName}/ips/{ipAddress}.
type VPSIP struct {
	IPAddress  string `json:"ipAddress"`
	Version    int    `json:"version"`
	Type       string `json:"type"`
	Reverse    string `json:"reverse"`
	Gateway    string `json:"gateway"`
	MACAddress string `json:"macAddress"`
}

// DedicatedServer is GET /dedicated/server/{serviceName}.
type DedicatedServer struct {
	Name             string `json:"name"`
	IP               string `json:"ip"`
	Datacenter       string `json:"datacenter"`
	Region           string `json:"region"`
	AvailabilityZone string `json:"availabilityZone"`
	Rack             string `json:"rack"`
	State            string `json:"state"`
	PowerState       string `json:"powerState"`
	CommercialRange  string `json:"commercialRange"`
	OS               string `json:"os"`
	Reverse          string `json:"reverse"`
	ServerID         int64  `json:"serverId"`
	BootID           int    `json:"bootId"`
	RootDevice       string `json:"rootDevice"`
	RescueMail       string `json:"rescueMail"`
	Monitoring       bool   `json:"monitoring"`
	SupportLevel     string `json:"supportLevel"`
	LinkSpeed        int    `json:"linkSpeed"`
	ProfessionalUse  bool   `json:"professionalUse"`
	NoIntervention   bool   `json:"noIntervention"`
	IAM              *struct {
		DisplayName string `json:"displayName"`
		ID          string `json:"id"`
	} `json:"iam"`
}

// ServiceInfos is GET /dedicated/server/{serviceName}/serviceInfos.
type ServiceInfos struct {
	Creation    string `json:"creation"`
	Expiration  string `json:"expiration"`
	Status      string `json:"status"`
	RenewalType string `json:"renewalType"`
	Renew       *struct {
		Automatic          bool `json:"automatic"`
		DeleteAtExpiration bool `json:"deleteAtExpiration"`
		Forced             bool `json:"forced"`
	} `json:"renew"`
}

// Boot is GET /dedicated/server/{serviceName}/boot/{bootId}.
type Boot struct {
	BootID      int    `json:"bootId"`
	BootType    string `json:"bootType"`
	Description string `json:"description"`
	Kernel      string `json:"kernel"`
}

// UnitValue is OVH's {unit, value} size.
type UnitValue struct {
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
}

// Bytes converts the size to bytes (1024-based units).
func (u UnitValue) Bytes() provider.Bytes {
	mult := map[string]float64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}[strings.ToUpper(u.Unit)]
	if mult == 0 {
		mult = 1
	}
	return provider.Bytes(u.Value * mult)
}

// Hardware is GET /dedicated/server/{serviceName}/specifications/hardware.
type Hardware struct {
	Description         string     `json:"description"`
	MemorySize          *UnitValue `json:"memorySize"`
	NumberOfProcessors  int        `json:"numberOfProcessors"`
	ProcessorName       string     `json:"processorName"`
	CoresPerProcessor   int        `json:"coresPerProcessor"`
	ThreadsPerProcessor int        `json:"threadsPerProcessor"`
	DiskGroups          []struct {
		Description    string     `json:"description"`
		NumberOfDisks  int        `json:"numberOfDisks"`
		DiskSize       *UnitValue `json:"diskSize"`
		DiskType       string     `json:"diskType"`
		RaidController string     `json:"raidController"`
	} `json:"diskGroups"`
	BootMode   string `json:"bootMode"`
	FormFactor string `json:"formFactor"`
}

// InstanceDetail is GET /cloud/project/{serviceName}/instance/{instanceId}:
// the listing fields with the flavor and image resolved.
type InstanceDetail struct {
	Instance
	Flavor *struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		VCPUs  int    `json:"vcpus"`
		RAM    int    `json:"ram"`
		Disk   int    `json:"disk"`
		OSType string `json:"osType"`
	} `json:"flavor"`
	Image *struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		User       string `json:"user"`
		Visibility string `json:"visibility"`
	} `json:"image"`
	SSHKey *struct {
		Name string `json:"name"`
	} `json:"sshKey"`
	MonthlyBilling *struct {
		Status string `json:"status"`
		Since  string `json:"since"`
	} `json:"monthlyBilling"`
}

// CloudProject is GET /cloud/project/{serviceName}.
type CloudProject struct {
	ProjectID   string `json:"project_id"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// Instance is one element of GET /cloud/project/{serviceName}/instance.
type Instance struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Region      string `json:"region"`
	FlavorID    string `json:"flavorId"`
	ImageID     string `json:"imageId"`
	PlanCode    string `json:"planCode"`
	Created     string `json:"created"`
	IPAddresses []struct {
		IP        string `json:"ip"`
		Type      string `json:"type"`
		Version   int    `json:"version"`
		GatewayIP string `json:"gatewayIp"`
		NetworkID string `json:"networkId"`
	} `json:"ipAddresses"`
}

// AccessRule is one permission of a consumer key.
type AccessRule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Credential is the answer to POST /auth/credential.
type Credential struct {
	ValidationURL string `json:"validationUrl"`
	ConsumerKey   string `json:"consumerKey"`
	State         string `json:"state"`
}

// Client is a minimal OVH API client with request signing.
type Client struct {
	BaseURL     string
	AppKey      string
	AppSecret   string
	ConsumerKey string
	UserAgent   string
	HTTP        *http.Client

	mu        sync.Mutex
	timeDelta time.Duration
	timeKnown bool
}

// ListDedicatedServers returns the service names of all dedicated servers.
func (c *Client) ListDedicatedServers(ctx context.Context) ([]string, error) {
	var names []string
	if err := c.do(ctx, http.MethodGet, "/dedicated/server", nil, &names); err != nil {
		return nil, err
	}
	return names, nil
}

// GetDedicatedServer returns one dedicated server.
func (c *Client) GetDedicatedServer(ctx context.Context, name string) (*DedicatedServer, error) {
	var s DedicatedServer
	if err := c.do(ctx, http.MethodGet, "/dedicated/server/"+url.PathEscape(name), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DedicatedServerIPs returns the IP blocks routed to a dedicated server.
func (c *Client) DedicatedServerIPs(ctx context.Context, name string) ([]string, error) {
	var blocks []string
	if err := c.do(ctx, http.MethodGet, "/dedicated/server/"+url.PathEscape(name)+"/ips", nil, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// ListCloudProjects returns the IDs of all Public Cloud projects.
func (c *Client) ListCloudProjects(ctx context.Context) ([]string, error) {
	var ids []string
	if err := c.do(ctx, http.MethodGet, "/cloud/project", nil, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// GetCloudProject returns one Public Cloud project.
func (c *Client) GetCloudProject(ctx context.Context, id string) (*CloudProject, error) {
	var p CloudProject
	if err := c.do(ctx, http.MethodGet, "/cloud/project/"+url.PathEscape(id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListInstances returns the instances of a Public Cloud project.
func (c *Client) ListInstances(ctx context.Context, projectID string) ([]Instance, error) {
	var list []Instance
	if err := c.do(ctx, http.MethodGet, "/cloud/project/"+url.PathEscape(projectID)+"/instance", nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ListVPS returns the service names of all VPS.
func (c *Client) ListVPS(ctx context.Context) ([]string, error) {
	var names []string
	if err := c.do(ctx, http.MethodGet, "/vps", nil, &names); err != nil {
		return nil, err
	}
	return names, nil
}

// GetVPS returns one VPS.
func (c *Client) GetVPS(ctx context.Context, name string) (*VPS, error) {
	var v VPS
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// VPSIPs returns the addresses of a VPS.
func (c *Client) VPSIPs(ctx context.Context, name string) ([]string, error) {
	var ips []string
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/ips", nil, &ips); err != nil {
		return nil, err
	}
	return ips, nil
}

// GetVPSIP returns one address of a VPS with its details.
func (c *Client) GetVPSIP(ctx context.Context, name, ip string) (*VPSIP, error) {
	var v VPSIP
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/ips/"+url.PathEscape(ip), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// GetVPSServiceInfos returns the contract information of a VPS.
func (c *Client) GetVPSServiceInfos(ctx context.Context, name string) (*ServiceInfos, error) {
	var si ServiceInfos
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/serviceInfos", nil, &si); err != nil {
		return nil, err
	}
	return &si, nil
}

// VPSImage is GET /vps/{serviceName}/images/available/{id}.
type VPSImage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// VPSImages returns the ids of the images a VPS can be rebuilt with.
func (c *Client) VPSImages(ctx context.Context, name string) ([]string, error) {
	var ids []string
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/images/available", nil, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// GetVPSImage returns one image.
func (c *Client) GetVPSImage(ctx context.Context, name, id string) (*VPSImage, error) {
	var img VPSImage
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/images/available/"+url.PathEscape(id), nil, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// RebuildVPS reinstalls a VPS with an image (POST /vps/{serviceName}/rebuild).
func (c *Client) RebuildVPS(ctx context.Context, name string, body map[string]any) (*VPSTask, error) {
	var t VPSTask
	if err := c.do(ctx, http.MethodPost, "/vps/"+url.PathEscape(name)+"/rebuild", body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// VPSTask is GET /vps/{serviceName}/tasks/{id}.
type VPSTask struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	State    string `json:"state"`
	Progress int    `json:"progress"`
}

// VPSTasks returns the ids of the tasks of a VPS.
func (c *Client) VPSTasks(ctx context.Context, name string) ([]int64, error) {
	var ids []int64
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/tasks", nil, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// GetVPSTask returns one task of a VPS.
func (c *Client) GetVPSTask(ctx context.Context, name string, id int64) (*VPSTask, error) {
	var t VPSTask
	if err := c.do(ctx, http.MethodGet, "/vps/"+url.PathEscape(name)+"/tasks/"+strconv.FormatInt(id, 10), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// VPSAction runs reboot, start or stop on a VPS.
func (c *Client) VPSAction(ctx context.Context, name, action string) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodPost, "/vps/"+url.PathEscape(name)+"/"+action, struct{}{}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// UpdateVPS changes fields of a VPS (displayName, netbootMode, ...).
func (c *Client) UpdateVPS(ctx context.Context, name string, fields map[string]any) error {
	return c.do(ctx, http.MethodPut, "/vps/"+url.PathEscape(name), fields, nil)
}

// GetServiceInfos returns the contract information of a dedicated server.
func (c *Client) GetServiceInfos(ctx context.Context, name string) (*ServiceInfos, error) {
	var si ServiceInfos
	if err := c.do(ctx, http.MethodGet, "/dedicated/server/"+url.PathEscape(name)+"/serviceInfos", nil, &si); err != nil {
		return nil, err
	}
	return &si, nil
}

// GetBoot returns one boot option of a dedicated server.
func (c *Client) GetBoot(ctx context.Context, name string, bootID int) (*Boot, error) {
	var b Boot
	if err := c.do(ctx, http.MethodGet, "/dedicated/server/"+url.PathEscape(name)+"/boot/"+strconv.Itoa(bootID), nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// GetHardware returns the hardware specification of a dedicated server.
func (c *Client) GetHardware(ctx context.Context, name string) (*Hardware, error) {
	var hw Hardware
	if err := c.do(ctx, http.MethodGet, "/dedicated/server/"+url.PathEscape(name)+"/specifications/hardware", nil, &hw); err != nil {
		return nil, err
	}
	return &hw, nil
}

// GetInstance returns one Public Cloud instance with flavor and image.
func (c *Client) GetInstance(ctx context.Context, projectID, id string) (*InstanceDetail, error) {
	var in InstanceDetail
	if err := c.do(ctx, http.MethodGet, "/cloud/project/"+url.PathEscape(projectID)+"/instance/"+url.PathEscape(id), nil, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// Task is an asynchronous operation on a dedicated server.
type Task struct {
	TaskID   int64  `json:"taskId"`
	Function string `json:"function"`
	Status   string `json:"status"`
	Comment  string `json:"comment"`
}

// RebootDedicated starts a hardware reboot of a dedicated server.
func (c *Client) RebootDedicated(ctx context.Context, name string) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodPost, "/dedicated/server/"+url.PathEscape(name)+"/reboot", struct{}{}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListBoots returns the boot option IDs of a dedicated server of one type
// (harddisk, rescue, network, ...).
func (c *Client) ListBoots(ctx context.Context, name, bootType string) ([]int, error) {
	var ids []int
	path := "/dedicated/server/" + url.PathEscape(name) + "/boot"
	if bootType != "" {
		path += "?bootType=" + url.QueryEscape(bootType)
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// UpdateDedicated changes properties of a dedicated server (bootId,
// rescueSshKey, rescueMail, monitoring, ...).
func (c *Client) UpdateDedicated(ctx context.Context, name string, fields map[string]any) error {
	return c.do(ctx, http.MethodPut, "/dedicated/server/"+url.PathEscape(name), fields, nil)
}

// RebootInstance reboots a Public Cloud instance, type soft or hard.
func (c *Client) RebootInstance(ctx context.Context, projectID, id, typ string) error {
	body := struct {
		Type string `json:"type"`
	}{Type: typ}
	return c.do(ctx, http.MethodPost, "/cloud/project/"+url.PathEscape(projectID)+"/instance/"+url.PathEscape(id)+"/reboot", body, nil)
}

// InstanceAction runs a body-less instance action: start, stop.
func (c *Client) InstanceAction(ctx context.Context, projectID, id, action string) error {
	return c.do(ctx, http.MethodPost, "/cloud/project/"+url.PathEscape(projectID)+"/instance/"+url.PathEscape(id)+"/"+action, struct{}{}, nil)
}

// SetInstanceRescue enables or disables rescue mode of an instance; OVH
// reboots the instance and, when enabling, returns the root password.
func (c *Client) SetInstanceRescue(ctx context.Context, projectID, id string, enable bool) (string, error) {
	body := struct {
		Rescue bool `json:"rescue"`
	}{Rescue: enable}
	var raw struct {
		AdminPassword string `json:"adminPassword"`
	}
	if err := c.do(ctx, http.MethodPost, "/cloud/project/"+url.PathEscape(projectID)+"/instance/"+url.PathEscape(id)+"/rescueMode", body, &raw); err != nil {
		return "", err
	}
	return raw.AdminPassword, nil
}

// ReverseIP is one reverse DNS entry of an IP block.
type ReverseIP struct {
	IPReverse string `json:"ipReverse"`
	Reverse   string `json:"reverse"`
}

// ListReverse returns the addresses of a block that have a reverse DNS
// entry (GET /ip/{block}/reverse).
func (c *Client) ListReverse(ctx context.Context, block string) ([]string, error) {
	var ips []string
	if err := c.do(ctx, http.MethodGet, "/ip/"+url.PathEscape(block)+"/reverse", nil, &ips); err != nil {
		return nil, err
	}
	return ips, nil
}

// GetReverse returns the reverse DNS entry of one address.
func (c *Client) GetReverse(ctx context.Context, block, ip string) (*ReverseIP, error) {
	var r ReverseIP
	if err := c.do(ctx, http.MethodGet, "/ip/"+url.PathEscape(block)+"/reverse/"+url.PathEscape(ip), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SetReverse creates or replaces the reverse DNS entry of one address.
// OVH expects a fully qualified name with a trailing dot.
func (c *Client) SetReverse(ctx context.Context, block, ip, name string) error {
	body := ReverseIP{IPReverse: ip, Reverse: strings.TrimSuffix(name, ".") + "."}
	var r ReverseIP
	return c.do(ctx, http.MethodPost, "/ip/"+url.PathEscape(block)+"/reverse", body, &r)
}

// DeleteReverse removes the reverse DNS entry of one address.
func (c *Client) DeleteReverse(ctx context.Context, block, ip string) error {
	return c.do(ctx, http.MethodDelete, "/ip/"+url.PathEscape(block)+"/reverse/"+url.PathEscape(ip), nil, nil)
}

// UpdateInstanceName renames a Public Cloud instance
// (PUT /cloud/project/{serviceName}/instance/{instanceId}).
func (c *Client) UpdateInstanceName(ctx context.Context, projectID, id, name string) error {
	body := struct {
		InstanceName string `json:"instanceName"`
	}{InstanceName: name}
	return c.do(ctx, http.MethodPut, "/cloud/project/"+url.PathEscape(projectID)+"/instance/"+url.PathEscape(id), body, nil)
}

// RequestConsumerKey asks OVH for a new consumer key with the given access
// rules. Only the application key is needed. The key is unusable until the
// user opens the validation URL and approves it.
func (c *Client) RequestConsumerKey(ctx context.Context, rules []AccessRule, redirection string) (*Credential, error) {
	body := struct {
		AccessRules []AccessRule `json:"accessRules"`
		Redirection string       `json:"redirection,omitempty"`
	}{AccessRules: rules, Redirection: redirection}
	var cred Credential
	if err := c.do(ctx, http.MethodPost, "/auth/credential", body, &cred); err != nil {
		return nil, err
	}
	return &cred, nil
}

// Time returns the API server's idea of now, from GET /auth/time.
func (c *Client) Time(ctx context.Context) (time.Time, error) {
	var ts int64
	if err := c.doUnsigned(ctx, http.MethodGet, "/auth/time", nil, &ts); err != nil {
		return time.Time{}, err
	}
	return time.Unix(ts, 0), nil
}

func (c *Client) timestamp(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.timeKnown {
		server, err := c.Time(ctx)
		if err != nil {
			return 0, err
		}
		c.timeDelta = server.Sub(time.Now())
		c.timeKnown = true
	}
	return time.Now().Add(c.timeDelta).Unix(), nil
}

func (c *Client) sign(method, fullURL, body string, ts int64) string {
	h := sha1.New()
	io.WriteString(h, c.AppSecret+"+"+c.ConsumerKey+"+"+method+"+"+fullURL+"+"+body+"+"+strconv.FormatInt(ts, 10))
	return "$1$" + hex.EncodeToString(h.Sum(nil))
}

// do performs a request. Requests are signed when a consumer key is set;
// POST /auth/credential and GET /auth/time work without one.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if c.ConsumerKey == "" {
		return c.doUnsigned(ctx, method, path, in, out)
	}
	ts, err := c.timestamp(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, path, in, out, &ts)
}

func (c *Client) doUnsigned(ctx context.Context, method, path string, in, out any) error {
	return c.request(ctx, method, path, in, out, nil)
}

func (c *Client) request(ctx context.Context, method, path string, in, out any, ts *int64) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return &provider.APIError{Provider: TypeName, Message: "encoding request: " + err.Error()}
		}
	}
	fullURL := strings.TrimSuffix(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Ovh-Application", c.AppKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if ts != nil {
		req.Header.Set("X-Ovh-Consumer", c.ConsumerKey)
		req.Header.Set("X-Ovh-Timestamp", strconv.FormatInt(*ts, 10))
		req.Header.Set("X-Ovh-Signature", c.sign(method, fullURL, string(body), *ts))
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "reading response: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return parseError(resp.StatusCode, method, path, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "unexpected response: " + err.Error()}
	}
	return nil
}

func parseError(status int, method, path string, body []byte) error {
	apiErr := &provider.APIError{Provider: TypeName, Status: status}
	var e struct {
		Message   string `json:"message"`
		ErrorCode string `json:"errorCode"`
		Class     string `json:"class"`
	}
	if json.Unmarshal(body, &e) == nil {
		apiErr.Code = e.ErrorCode
		apiErr.Message = e.Message
	}
	switch {
	case apiErr.Code == "NOT_GRANTED_CALL":
		apiErr.Message = fmt.Sprintf("the consumer key is not allowed to call %s %s (check its access rules)", method, path)
	case apiErr.Code == "INVALID_CREDENTIAL" || apiErr.Code == "NOT_CREDENTIAL":
		apiErr.Message = "the consumer key is invalid or not validated yet"
	case apiErr.Code == "INVALID_KEY":
		apiErr.Message = "the application key is invalid"
	case apiErr.Code == "INVALID_SIGNATURE":
		apiErr.Message = "request signature rejected (check the application secret and the consumer key)"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
