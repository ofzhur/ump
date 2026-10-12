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
)

// prompter implements provider.Prompter on the app's stdin and stdout.
type prompter struct {
	in         *bufio.Reader
	out        io.Writer
	readSecret func() (string, error)
}

func (a *app) newPrompter() *prompter {
	return &prompter{in: bufio.NewReader(a.stdin), out: a.stdout, readSecret: a.readSecret}
}

var errInputClosed = withExit(exitAborted, errors.New("input closed"))

func (p *prompter) Printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

func (p *prompter) readLine() (string, error) {
	s, err := p.in.ReadString('\n')
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
func (p *prompter) Secret(label string) (string, error) {
	p.Printf("%s: ", label)
	if p.readSecret != nil {
		s, err := p.readSecret()
		p.Printf("\n")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(s), nil
	}
	s, err := p.readLine()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// Confirm implements provider.Prompter.
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
		p.Printf("Please answer y or n.\n")
	}
}

// Choose shows a numbered menu and returns the index of the choice. The
// first word of an item is accepted instead of its number.
func (p *prompter) Choose(label string, items []string, def int) (int, error) {
	p.Printf("%s\n", label)
	for i, it := range items {
		p.Printf("  %d) %s\n", i+1, it)
	}
	for {
		p.Printf("Choice [%d]: ", def+1)
		s, err := p.readLine()
		if err != nil {
			return 0, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(items) {
			return n - 1, nil
		}
		for i, it := range items {
			if strings.EqualFold(s, strings.Fields(it)[0]) {
				return i, nil
			}
		}
		p.Printf("Please enter a number between 1 and %d.\n", len(items))
	}
}

type secretStorage int

const (
	storeFile secretStorage = iota
	storeEnv
	storeCommand
)

// runWizard is `ump config` without a subcommand.
func (a *app) runWizard(cmd *cobra.Command) error {
	if a.isTTY == nil || !a.isTTY() {
		return withExit(exitAborted, errors.New("ump config is interactive and needs a terminal; use `ump config add TYPE NAME --set KEY=VALUE ...` instead"))
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	ui := a.newPrompter()
	ui.Printf("UMP configuration wizard\n")
	ui.Printf("Configuration file: %s\n", config.AbbreviateHome(path))
	if len(cfg.Accounts) == 0 {
		ui.Printf("No accounts configured yet.\n\n")
	} else {
		ui.Printf("Configured accounts:\n")
		for _, n := range cfg.AccountNames() {
			ui.Printf("  %-20s %s\n", n, cfg.Accounts[n].Type)
		}
		ui.Printf("\n")
	}
	for {
		if err := a.wizardAddAccount(cmd.Context(), ui, cfg, path); err != nil {
			return err
		}
		again, err := ui.Confirm("Add another account?", false)
		if err != nil {
			return err
		}
		if !again {
			break
		}
		ui.Printf("\n")
	}
	ui.Printf("\nDone. Try: ump get servers\n")
	return nil
}

func (a *app) wizardAddAccount(ctx context.Context, ui *prompter, cfg *config.Config, path string) error {
	types := provider.Types()
	items := make([]string, len(types))
	for i, t := range types {
		items[i] = fmt.Sprintf("%-14s %s", t.Name, t.Description)
	}
	idx, err := ui.Choose("Provider type", items, 0)
	if err != nil {
		return err
	}
	t := types[idx]
	ui.Printf("\n")

	var name string
	for {
		name, err = ui.Line("Account name", suggestAccountName(cfg, t.Name))
		if err != nil {
			return err
		}
		if err := config.ValidateAccountName(name); err != nil {
			ui.Printf("%v\n", err)
			continue
		}
		if existing, ok := cfg.Accounts[name]; ok {
			replace, err := ui.Confirm(fmt.Sprintf("Account %q (%s) exists. Replace it?", name, existing.Type), false)
			if err != nil {
				return err
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
	storage := storeFile
	if hasSecrets {
		ui.Printf("\n")
		i, err := ui.Choose("Where should this account's secrets be kept?", []string{
			fmt.Sprintf("files under %s, readable only by you", config.AbbreviateHome(config.SecretsDir())),
			"environment variables that you set in your shell",
			"a command that prints the secret (pass, op, sops, vault, ...)",
		}, 0)
		if err != nil {
			return err
		}
		storage = secretStorage(i)
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
				return err
			}
			values[f.Key] = f.Choices[i]
			acct.Settings[f.Key] = config.PlainValue(f.Choices[i])
		case !f.Secret:
			for {
				v, err := ui.Line(f.Label, f.Default)
				if err != nil {
					return err
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
			switch storage {
			case storeFile:
				for {
					v, err := ui.Secret(f.Label)
					if err != nil {
						return err
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
			case storeEnv:
				env, err := ui.Line("Environment variable holding the "+strings.ToLower(f.Label), envVarName(name, f.Key))
				if err != nil {
					return err
				}
				acct.Settings[f.Key] = config.Value{Env: env}
				switch v, ok := os.LookupEnv(env); {
				case ok && v != "":
					values[f.Key] = v
				case f.Deferrable:
					deferred = append(deferred, f.Key)
				default:
					untestable = append(untestable, env)
					unresolved[f.Key] = true
					ui.Printf("  %s is not set in this shell; the connection test will be skipped.\n", env)
				}
			case storeCommand:
				for {
					c, err := ui.Line("Command that prints the "+strings.ToLower(f.Label), "")
					if err != nil {
						return err
					}
					if c == "" {
						ui.Printf("A command is required.\n")
						continue
					}
					v, err := config.Value{Command: c}.Resolve()
					if err == nil && v == "" {
						err = fmt.Errorf("command %q printed nothing", c)
					}
					if err != nil {
						ui.Printf("  %v\n", err)
						retry, err := ui.Confirm("Try another command?", true)
						if err != nil {
							return err
						}
						if retry {
							continue
						}
						return withExit(exitAborted, errors.New("aborted"))
					}
					acct.Settings[f.Key] = config.Value{Command: c}
					values[f.Key] = v
					break
				}
			}
		}
	}

	if t.Complete != nil {
		ui.Printf("\n")
		if err := t.Complete(ctx, ui, values, a.providerOptions()); err != nil {
			return err
		}
	}
	var generated []string // deferred keys that Complete filled in
	for _, k := range deferred {
		if values[k] == "" {
			return fmt.Errorf("%s: %s is required", t.Name, k)
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
		return errs[0]
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
				return err
			}
			if !save {
				ui.Printf("Not saved.\n")
				return nil
			}
		} else {
			ui.Printf("ok (%s)\n", plural(n, "server"))
		}
	}

	if storage == storeFile {
		for _, f := range t.Fields {
			if !f.Secret || f.Hidden || values[f.Key] == "" {
				continue
			}
			p := secretFilePath(name, f.Key)
			if err := config.WriteSecretFile(p, values[f.Key]); err != nil {
				return err
			}
			acct.Settings[f.Key] = config.Value{File: config.AbbreviateHome(p)}
		}
	}
	cfg.Accounts[name] = acct
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	ui.Printf("Saved account %s (%s) to %s\n", name, t.Name, config.AbbreviateHome(path))
	if storage == storeEnv {
		var lines []string
		for _, f := range t.Fields {
			if !f.Secret || f.Hidden {
				continue
			}
			env := acct.Settings[f.Key].Env
			switch {
			case containsString(generated, f.Key):
				// The value was just issued by the provider and exists nowhere
				// else; the user has to see it once to keep it.
				lines = append(lines, fmt.Sprintf("  export %s=%s", env, shellQuote(values[f.Key])))
			case containsString(untestable, env):
				lines = append(lines, fmt.Sprintf("  export %s=<%s>", env, strings.ToLower(f.Label)))
			}
		}
		if len(lines) > 0 {
			ui.Printf("Before running ump, set in your shell:\n%s\n", strings.Join(lines, "\n"))
		}
	}
	return nil
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
