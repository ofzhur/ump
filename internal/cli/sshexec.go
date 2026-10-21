package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// sshRunner runs the local ssh client with argv (argv[0] is "ssh") and
// returns its exit status. With interactive set, ssh gets the terminal.
type sshRunner func(argv []string, interactive bool) (int, error)

// runSSH is the real sshRunner.
func runSSH(argv []string, interactive bool) (int, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return 0, errors.New("no ssh client found in PATH")
	}
	// a child process, never an exec: what ump does after the session
	// (releasing a lock, the push of the home) must still run
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// sshExecer runs the local ssh client without a terminal, with the given
// streams (a nil stdin means no input), and returns its exit status; the
// context ends it. This is how ump talks to machines by itself: probes,
// the entry script, version checks.
type sshExecer func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// execSSH is the real sshExecer.
func execSSH(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return 0, errors.New("no ssh client found in PATH")
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// withSSHOptions adds -o options right after the program name.
func withSSHOptions(argv []string, opts ...string) []string {
	out := make([]string, 0, len(argv)+2*len(opts))
	out = append(out, argv[0])
	for _, o := range opts {
		out = append(out, "-o", o)
	}
	return append(out, argv[1:]...)
}

// sshArgs builds the ssh command line. identities are key files offered
// in order; with identitiesOnly the agent's other keys are not, so that
// they cannot exhaust the server's authentication attempts first. Rescue
// and staging systems have a fresh host key every boot, so
// insecureHostKey skips host key checks for them.
func sshArgs(user, addr string, identities []string, identitiesOnly, insecureHostKey bool, command []string) []string {
	// Keepalives: a command that kexecs or reboots the machine ends the
	// connection without a close, and the client must notice in seconds
	// rather than wait for the TCP timeout.
	argv := []string{"ssh", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"}
	for _, id := range identities {
		argv = append(argv, "-i", id)
	}
	if identitiesOnly && len(identities) > 0 {
		argv = append(argv, "-o", "IdentitiesOnly=yes")
	}
	if insecureHostKey {
		argv = append(argv, "-o", "UserKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=no", "-o", "LogLevel=ERROR")
	}
	if user == "" {
		user = "root"
	}
	argv = append(argv, user+"@"+addr)
	if len(command) > 0 {
		argv = append(argv, "--")
		argv = append(argv, command...)
	}
	return argv
}

// shellJoin renders argv for copy and paste, quoting what needs it.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t'\"$`\\|&;<>()*?[]#~") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

// exitCode ends the program with a status and no message, for example the
// exit status of a command run over SSH.
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// splitDash separates positional arguments from a command given after
// "--".
func splitDash(cmdArgsLenAtDash int, args []string) (positional, command []string) {
	if cmdArgsLenAtDash < 0 {
		return args, nil
	}
	return args[:cmdArgsLenAtDash], args[cmdArgsLenAtDash:]
}
