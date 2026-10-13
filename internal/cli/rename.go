package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

var renameResources = []string{"server"}

func (a *app) newServerRenameCmd() *cobra.Command {
	var (
		output string
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "rename NAME|ID|IP|PROVIDER/ID NEW-NAME",
		Short: "Rename a server at its provider",
		Long: `Set the name a server is known by at its provider: the server name at
Hetzner Robot and Hetzner Cloud, the instance name at OVHcloud Public
Cloud (OVHcloud dedicated servers cannot be renamed through the API).

The name is a label at the provider; the hostname of an installed
operating system does not change. UMP uses the provider name as the
machine name everywhere, including the hostname it sets when it installs
the machine, so the new name must be a valid hostname: lower-case
letters, digits and hyphens, with dots between labels.

A name that another listed server already has is refused unless --force
is given, so that renames do not create the duplicates that make
references ambiguous.`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.renameServer(cmd, args[0], args[1], output, force)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "print the renamed server as json, yaml or name instead of a sentence")
	cmd.Flags().BoolVar(&force, "force", false, "rename even if another listed server already has the new name")
	return cmd
}

var hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validateServerName accepts hostnames: DNS labels of lower-case letters,
// digits and hyphens, joined by dots, at most 253 characters.
func validateServerName(name string) error {
	if name == "" || len(name) > 253 {
		return fmt.Errorf("invalid name %q: 1 to 253 characters", name)
	}
	for _, label := range strings.Split(name, ".") {
		if !hostnameLabel.MatchString(label) {
			return fmt.Errorf("invalid name %q: use lower-case letters, digits and hyphens, with dots between labels (the name becomes the hostname when the server is installed)", name)
		}
	}
	return nil
}

func (a *app) renameServer(cmd *cobra.Command, ref, newName, output string, force bool) error {
	if err := validateServerName(newName); err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	format := outputFormat("")
	if output != "" {
		f, err := parseFormat(output)
		if err != nil || f == formatTable || f == formatWide {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml, name)", output)
		}
		format = f
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured in %s; run `ump config` to add one", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	results := a.listAccounts(ctx, cfg, names, cache.Open(path))
	if err := ctx.Err(); err != nil {
		return err
	}
	var machines []provider.Machine
	byAccount := map[string]listResult{}
	failed := 0
	for _, r := range results {
		byAccount[r.account] = r
		if r.err != nil {
			failed++
			fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
			continue
		}
		machines = append(machines, r.machines...)
	}
	provider.SortMachines(machines)

	m, err := a.resolveServer(a.resolverUI(cmd), machines, ref)
	switch {
	case errors.Is(err, errQuit):
		return withExit(exitAborted, errors.New("aborted"))
	case err != nil:
		var nf *notFoundError
		if errors.As(err, &nf) && failed > 0 {
			return fmt.Errorf("%w (%d of %d accounts could not be listed)", err, failed, len(results))
		}
		return err
	}
	if m.Name == newName {
		fmt.Fprintf(a.stderr, "%s is already named %s; nothing to do.\n", m.Ref(), newName)
		return nil
	}
	renamer, ok := byAccount[m.Account].prov.(provider.Renamer)
	if !ok {
		return fmt.Errorf("account %s: provider type %s cannot rename servers", m.Account, m.Provider)
	}
	if taken := matching(machines, newName); len(taken) > 0 && !force {
		return fmt.Errorf("the name %q is already used by %s; pick another name or use --force", newName, candidates(taken))
	}

	renamed, err := renamer.Rename(ctx, m, newName)
	if err != nil {
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	if format != "" {
		return printMachines(a.stdout, []provider.Machine{renamed}, format, false)
	}
	fmt.Fprintf(a.stdout, "Renamed %s (account %s) from %s to %s\n", renamed.Ref(), renamed.Account, m.Name, renamed.Name)
	fmt.Fprintf(a.stderr, "The hostname of the installed system is unchanged; ump install applies the new name.\n")
	return nil
}
