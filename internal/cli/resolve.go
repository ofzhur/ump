package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/provider"
)

// matching returns the machines that ref denotes: by "provider/id", ID,
// name or IP address. Names are not unique across accounts, and some
// providers do not enforce them within one, so there can be several.
func matching(ms []provider.Machine, ref string) []provider.Machine {
	var out []provider.Machine
	for _, m := range ms {
		if m.Matches(ref) {
			out = append(out, m)
		}
	}
	return out
}

// notFoundError says a reference matched nothing.
type notFoundError struct{ ref string }

func (e *notFoundError) Error() string { return fmt.Sprintf("server %q not found", e.ref) }

// ambiguousError says a reference matched several machines and no
// terminal was there to ask.
type ambiguousError struct {
	ref     string
	matches []provider.Machine
}

func (e *ambiguousError) Error() string {
	return fmt.Sprintf("server %q is ambiguous: %s; use provider/id or --account", e.ref, candidates(e.matches))
}

func candidates(ms []provider.Machine) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Ref() + " (account " + m.Account + ")"
	}
	return strings.Join(parts, ", ")
}

// resolveServer picks the one machine that ref denotes. When several
// match, the user is asked on a terminal (ui is not nil) which one is
// meant; without a terminal the command fails with the candidates, so a
// script or an agent can retry with provider/id or --account. Leaving the
// prompt returns errQuit.
func (a *app) resolveServer(ui *prompter, ms []provider.Machine, ref string) (provider.Machine, error) {
	matches := matching(ms, ref)
	switch len(matches) {
	case 0:
		return provider.Machine{}, &notFoundError{ref: ref}
	case 1:
		return matches[0], nil
	}
	if ui == nil {
		return provider.Machine{}, &ambiguousError{ref: ref, matches: matches}
	}
	items := make([]string, len(matches))
	for i, m := range matches {
		ip := m.IPv4
		if ip == "" {
			ip = m.IPv6
		}
		if ip == "" {
			ip = "<none>"
		}
		items[i] = fmt.Sprintf("%-26s  account %-14s  %-16s  %-10s  %s", m.Ref(), m.Account, ip, m.Location, m.Status)
	}
	idx, err := ui.Choose(fmt.Sprintf("Server %q exists %d times. Which one?", ref, len(matches)), items, -1)
	if err != nil {
		return provider.Machine{}, err
	}
	return matches[idx], nil
}

// resolverUI returns a prompter on stderr when stdin is a terminal, so
// that questions never end up in piped output; nil otherwise.
func (a *app) resolverUI(cmd *cobra.Command) *prompter {
	if a.isTTY == nil || !a.isTTY() {
		return nil
	}
	return a.newPrompterTo(cmd.Context(), a.stderr)
}
