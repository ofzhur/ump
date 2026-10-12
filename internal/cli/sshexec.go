package cli

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// sshRunner runs the local ssh client with argv (argv[0] is "ssh") and
// returns its exit status. With interactive set, the process is replaced
// by ssh where the platform allows it, so the terminal is handed over.
type sshRunner func(argv []string, interactive bool) (int, error)

// runSSH is the real sshRunner.
func runSSH(argv []string, interactive bool) (int, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return 0, errors.New("no ssh client found in PATH")
	}
	if interactive && runtime.GOOS != "windows" {
		// Only returns on failure.
		return 0, syscall.Exec(path, argv, os.Environ())
	}
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

// sshArgs builds the ssh command line. identities are key files offered
// in order; with identitiesOnly the agent's other keys are not, so that
// they cannot exhaust the server's authentication attempts first. Rescue
// and staging systems have a fresh host key every boot, so
// insecureHostKey skips host key checks for them.
func sshArgs(user, addr string, identities []string, identitiesOnly, insecureHostKey bool, command []string) []string {
	argv := []string{"ssh"}
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
