package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/packaging"
)

// The PXE server as a systemd service: ump pxe unit prints its units as
// the packages install them (packaging/), for a hand install, and the
// server takes the backend's secrets from systemd's credentials.

func (a *app) newPXEUnitCmd() *cobra.Command {
	var check bool
	var binary string
	cmd := &cobra.Command{
		Use:   "unit [--check] [--exec PATH]",
		Short: "Print the systemd unit of the PXE server, or of its check",
		Long: `Print ump-pxe@.service, ump pxe serve as a systemd service for the site
its instance names (ump-pxe@fra1), or with --check ump-pxe-check@.service,
which runs ump pxe serve --check once with the service's settings, user
and sandbox. They are the units the deb and rpm packages install; for a
hand install, write them to /etc/systemd/system/ with --exec naming
where ump is (default /usr/bin/ump, the packages'). Both read the site's
settings from /etc/ump/pxe/SITE.env (docs/pxe.md).`,
		Example: `  ump pxe unit --exec /usr/local/bin/ump > /etc/systemd/system/ump-pxe@.service
  ump pxe unit --check --exec /usr/local/bin/ump > /etc/systemd/system/ump-pxe-check@.service`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !path.IsAbs(binary) || path.Clean(binary) != binary || strings.ContainsFunc(binary, func(r rune) bool {
				return r <= ' ' || r == 0x7f || strings.ContainsRune(`"'\%$;`, r)
			}) {
				return usageErrorf(cmd, "--exec %q: an absolute path, without spaces, quotes, backslashes, %% or $", binary)
			}
			unit := packaging.Service
			if check {
				unit = packaging.Check
			}
			_, err := fmt.Fprint(a.stdout, packaging.WithBinary(unit, binary))
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "the check's unit, ump-pxe-check@.service")
	cmd.Flags().StringVar(&binary, "exec", packaging.Binary, "where ump is")
	return cmd
}

// pxeCredentialVars are the backend's secrets, as home.credentials reads
// them from the environment.
var pxeCredentialVars = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "UMP_STATE_TOKEN", "VAULT_TOKEN", "CONSUL_HTTP_TOKEN", "UMP_STATE_USER", "UMP_STATE_PASSWORD"}

// pxeCredentials takes the backend's secrets from the credentials systemd
// passes the PXE server ($CREDENTIALS_DIRECTORY; LoadCredential= or
// LoadCredentialEncrypted= in a drop-in of its unit): a credential named
// like one of the variables wins over the variable, which the site's env
// file may set too.
func pxeCredentials() error {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return nil
	}
	for _, name := range pxeCredentialVars {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("the credential %s: %w", name, err)
		}
		if v := strings.TrimRight(string(data), "\r\n"); v != "" {
			os.Setenv(name, v)
		}
	}
	return nil
}
