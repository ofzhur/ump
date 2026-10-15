package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/manual"
)

// prompter implements provider.Prompter on the app's stdin and stdout.
// Every read watches the context, so Ctrl-C (which cancels it) aborts a
// prompt immediately instead of being noticed after the next Enter.
type prompter struct {
	ctx        context.Context
	in         *bufio.Reader
	out        io.Writer
	readSecret func() (string, error)
	// saveTerm captures the terminal state before hidden input and returns
	// the function that restores it; needed when the read is abandoned.
	saveTerm func() func()
}

func (a *app) newPrompter(ctx context.Context) *prompter {
	return a.newPrompterTo(ctx, a.stdout)
}

// newPrompterTo returns a prompter that writes its questions to out.
func (a *app) newPrompterTo(ctx context.Context, out io.Writer) *prompter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &prompter{ctx: ctx, in: bufio.NewReader(a.stdin), out: out, readSecret: a.readSecret, saveTerm: a.saveTerm}
}

var (
	errInputClosed = withExit(exitAborted, errors.New("input closed"))
	// errInterrupted is returned by a prompt when the context is cancelled
	// (Ctrl-C). It prints as "interrupted" with exit code 3.
	errInterrupted = &exitError{code: exitAborted, err: context.Canceled}
	// errQuit is returned by a prompt when the user asks to leave the
	// wizard. Nothing that was being entered is saved.
	errQuit = errors.New("quit")
)

func isQuit(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "q", "quit", "exit":
		return true
	}
	return false
}

func (p *prompter) Printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

type readResult struct {
	s   string
	err error
}

// await runs read in a goroutine and returns its result, or
// errInterrupted when the context is cancelled first. An abandoned read
// stays blocked on stdin; that is fine, the process is about to exit.
func (p *prompter) await(read func() (string, error)) (string, error) {
	ch := make(chan readResult, 1)
	go func() {
		s, err := read()
		ch <- readResult{s, err}
	}()
	select {
	case <-p.ctx.Done():
		p.Printf("\n")
		return "", errInterrupted
	case r := <-ch:
		return r.s, r.err
	}
}

func (p *prompter) readLine() (string, error) {
	s, err := p.await(func() (string, error) { return p.in.ReadString('\n') })
	if err != nil {
		if errors.Is(err, io.EOF) {
			if s != "" {
				return strings.TrimRight(s, "\r\n"), nil
			}
			return "", errInputClosed
		}
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Line implements provider.Prompter.
func (p *prompter) Line(label, def string) (string, error) {
	if def != "" {
		p.Printf("%s [%s]: ", label, def)
	} else {
		p.Printf("%s: ", label)
	}
	s, err := p.readLine()
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// Secret implements provider.Prompter: input without echo on a terminal.
// When the read is interrupted, the terminal state that the hidden read
// changed is restored by hand, because the abandoned read never returns.
func (p *prompter) Secret(label string) (string, error) {
	p.Printf("%s: ", label)
	if p.readSecret == nil {
		s, err := p.readLine()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(s), nil
	}
	restore := func() {}
	if p.saveTerm != nil {
		restore = p.saveTerm()
	}
	s, err := p.await(p.readSecret)
	if err != nil {
		if errors.Is(err, errInterrupted) {
			restore()
		} else {
			p.Printf("\n")
		}
		return "", err
	}
	p.Printf("\n")
	return strings.TrimSpace(s), nil
}

// Confirm implements provider.Prompter. "q" leaves the wizard.
func (p *prompter) Confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		p.Printf("%s [%s]: ", label, hint)
		s, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if isQuit(s) {
			return false, errQuit
		}
		p.Printf("Please answer y or n (or q to quit).\n")
	}
}

// Choose shows a numbered menu and returns the index of the choice. The
// first word of an item is accepted instead of its number; "q" leaves
// with errQuit. A negative def means there is no default and an answer
// is required.
func (p *prompter) Choose(label string, items []string, def int) (int, error) {
	p.Printf("%s\n", label)
	for i, it := range items {
		p.Printf("  %d) %s\n", i+1, it)
	}
	p.Printf("  q) quit\n")
	for {
		if def >= 0 {
			p.Printf("Choice [%d]: ", def+1)
		} else {
			p.Printf("Choice: ")
		}
		s, err := p.readLine()
		if err != nil {
			return 0, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			if def >= 0 {
				return def, nil
			}
			p.Printf("Please enter a number between 1 and %d, or q to quit.\n", len(items))
			continue
		}
		if isQuit(s) {
			return 0, errQuit
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(items) {
			return n - 1, nil
		}
		for i, it := range items {
			if strings.EqualFold(s, strings.Fields(it)[0]) {
				return i, nil
			}
		}
		p.Printf("Please enter a number between 1 and %d, or q to quit.\n", len(items))
	}
}

// runWizard is `ump config isp add` without arguments.
func (a *app) runWizard(cmd *cobra.Command) error {
	if a.isTTY == nil || !a.isTTY() {
		return withExit(exitAborted, errors.New("ump config isp add without arguments is interactive and needs a terminal; use `ump config isp add TYPE NAME --set KEY=VALUE ...` instead"))
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	ui := a.newPrompter(ctx)
	ui.Printf("UMP configuration wizard\n")
	// a fresh system: everything is about to be written to this machine;
	// a team, another machine or CI wants a backend first, and someone
	// who forgot UMP_STATE wants to notice now
	if a.home.Backend == nil {
		if _, err := os.Stat(path); err != nil {
			ui.Printf("No state backend is configured: everything ump keeps, this account's secrets included, stays in %s on this machine (a team, another machine or CI shares a state backend instead; UMP_STATE or ump config state setup).\n", config.AbbreviateHome(config.LocalDir()))
			local, err := ui.Confirm("Keep it on this machine?", true)
			if err != nil {
				return err
			}
			if !local {
				if err := a.stateWizard(cmd, ui); err != nil {
					return err
				}
				if err := a.resolveHome(ctx); err != nil {
					return err
				}
				if cfg, path, err = a.loadConfig(); err != nil {
					return err
				}
			}
			ui.Printf("\n")
		}
	}
	if a.home.Backend != nil {
		ui.Printf("Home: %s (mirror %s)\n", a.home.Backend, config.AbbreviateHome(a.home.Dir))
	} else {
		ui.Printf("Home: %s\n", config.AbbreviateHome(a.home.Dir))
	}
	if len(cfg.Accounts) == 0 {
		ui.Printf("No accounts configured yet.\n")
	} else {
		ui.Printf("Configured accounts:\n")
		for _, n := range cfg.AccountNames() {
			ui.Printf("  %-20s %s\n", n, cfg.Accounts[n].Type)
		}
	}
	ui.Printf("Type q at a menu or press Ctrl-C to quit. To test or remove accounts: ump config isp test, ump config isp remove NAME.\n\n")

	var types []provider.Type
	for _, t := range provider.Types() {
		if t.Name != manual.TypeName { // manual servers come with ump server add
			types = append(types, t)
		}
	}
	items := make([]string, len(types))
	for i, t := range types {
		items[i] = fmt.Sprintf("%-14s %s", t.Name, t.Description)
	}
	added := 0
	for {
		idx, err := ui.Choose("Provider type", items, 0)
		if errors.Is(err, errQuit) {
			break
		}
		if err != nil {
			return err
		}
		ui.Printf("\n")
		saved, err := a.wizardAddAccount(ctx, ui, cfg, path, types[idx])
		if errors.Is(err, errQuit) {
			ui.Printf("Nothing saved for this account.\n")
			break
		}
		if err != nil {
			return err
		}
		if saved {
			added++
		}
		again, err := ui.Confirm("Add another account?", false)
		if errors.Is(err, errQuit) || (err == nil && !again) {
			break
		}
		if err != nil {
			return err
		}
		ui.Printf("\n")
	}
	if added == 0 {
		ui.Printf("Nothing changed.\n")
		return nil
	}
	ui.Printf("\nDone. Try: ump server list\n")
	return nil
}

// wizardAddAccount asks for one account of type t, tests it and saves it.
// It reports whether the account was saved; errQuit means the user left.
func (a *app) wizardAddAccount(ctx context.Context, ui *prompter, cfg *config.Config, path string, t provider.Type) (bool, error) {
	var name string
	for {
		var err error
		name, err = ui.Line("Account name", suggestAccountName(cfg, t.Name))
		if err != nil {
			return false, err
		}
		if err := config.ValidateAccountName(name); err != nil {
			ui.Printf("%v\n", err)
			continue
		}
		if existing, ok := cfg.Accounts[name]; ok {
			replace, err := ui.Confirm(fmt.Sprintf("Account %q (%s) exists. Replace it?", name, existing.Type), false)
			if err != nil {
				return false, err
			}
			if !replace {
				continue
			}
		}
		break
	}

	hasSecrets := false
	for _, f := range t.Fields {
		if f.Secret && !f.Hidden {
			hasSecrets = true
		}
	}
	if hasSecrets {
		// the secrets are files in the home, wherever it lives, like all
		// the rest; env:NAME and command:CMD references are for
		// ump config isp add --set
		where := config.AbbreviateHome(config.SecretsDir())
		if a.home.Backend != nil {
			where = a.home.Backend.String()
		}
		ui.Printf("\nThe secrets are kept as files in the state (%s), readable only by you.\n", where)
	}
	ui.Printf("\n")

	values := map[string]string{} // resolved values, for the test and for Complete
	acct := config.Account{Type: t.Name, Settings: map[string]config.Value{}}
	var untestable []string         // environment variables that are not set
	unresolved := map[string]bool{} // keys whose value is unknown because of that
	var deferred []string           // keys left for t.Complete to fill in

	for _, f := range t.Fields {
		if f.Hidden {
			continue
		}
		if f.Help != "" {
			ui.Printf("  %s\n", f.Help)
		}
		switch {
		case len(f.Choices) > 0:
			def := 0
			for i, c := range f.Choices {
				if c == f.Default {
					def = i
				}
			}
			i, err := ui.Choose(f.Label, f.Choices, def)
			if err != nil {
				return false, err
			}
			values[f.Key] = f.Choices[i]
			acct.Settings[f.Key] = config.PlainValue(f.Choices[i])
		case !f.Secret:
			for {
				v, err := ui.Line(f.Label, f.Default)
				if err != nil {
					return false, err
				}
				if v == "" && f.Required {
					ui.Printf("A value is required.\n")
					continue
				}
				if v != "" {
					values[f.Key] = v
					acct.Settings[f.Key] = config.PlainValue(v)
				}
				break
			}
		default:
			for {
				v, err := ui.Secret(f.Label)
				if err != nil {
					return false, err
				}
				if v == "" && f.Required && !f.Deferrable {
					ui.Printf("A value is required.\n")
					continue
				}
				if v == "" {
					if f.Deferrable {
						deferred = append(deferred, f.Key)
					}
					break
				}
				values[f.Key] = v
				break
			}
		}
	}

	if t.Complete != nil {
		ui.Printf("\n")
		if err := t.Complete(ctx, ui, values, a.providerOptions()); err != nil {
			return false, err
		}
	}
	var generated []string // deferred keys that Complete filled in
	for _, k := range deferred {
		if values[k] == "" {
			return false, fmt.Errorf("%s: %s is required", t.Name, k)
		}
		generated = append(generated, k)
	}
	toValidate := make(map[string]string, len(values)+len(unresolved))
	for k, v := range values {
		toValidate[k] = v
	}
	for k := range unresolved {
		toValidate[k] = "(set later through the environment)"
	}
	if errs := t.Validate(toValidate); len(errs) > 0 {
		return false, errs[0]
	}

	ui.Printf("\n")
	if len(untestable) > 0 {
		ui.Printf("Skipping the connection test: %s not set.\n", strings.Join(untestable, ", "))
	} else {
		ui.Printf("Testing %s ... ", name)
		n, err := a.testValues(ctx, t, name, values)
		if err != nil {
			ui.Printf("failed\n  %v\n", err)
			save, err := ui.Confirm("Save the account anyway?", false)
			if err != nil {
				return false, err
			}
			if !save {
				ui.Printf("Not saved.\n")
				return false, nil
			}
		} else {
			ui.Printf("ok (%s)\n", plural(n, "server"))
		}
	}

	for _, f := range t.Fields {
		if !f.Secret || f.Hidden || values[f.Key] == "" {
			continue
		}
		p := secretFilePath(name, f.Key)
		if err := config.WriteSecretFile(p, values[f.Key]); err != nil {
			return false, err
		}
		acct.Settings[f.Key] = config.Value{File: config.AbbreviateHome(p)}
	}
	cfg.Accounts[name] = acct
	if err := config.Save(path, cfg); err != nil {
		return false, err
	}
	ui.Printf("Saved account %s (%s) to %s\n", name, t.Name, config.AbbreviateHome(path))
	return true, nil
}

func (a *app) testValues(ctx context.Context, t provider.Type, name string, values map[string]string) (int, error) {
	p, err := provider.Build(t.Name, name, values, a.providerOptions())
	if err != nil {
		return 0, err
	}
	inv, ok := p.(provider.Inventory)
	if !ok {
		return 0, fmt.Errorf("provider type %s cannot list servers", t.Name)
	}
	ms, err := inv.List(ctx)
	if err != nil {
		return 0, err
	}
	return len(ms), nil
}

func suggestAccountName(cfg *config.Config, typeName string) string {
	if _, taken := cfg.Accounts[typeName]; !taken {
		return typeName
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s-%d", typeName, i)
		if _, taken := cfg.Accounts[n]; !taken {
			return n
		}
	}
}

// envVarName suggests UMP_<ACCOUNT>_<KEY> for a secret.
func envVarName(account, key string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToUpper(s) {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	return "UMP_" + clean(account) + "_" + clean(key)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
