package helm

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/creack/pty"

	"github.com/deukyun/helm-tui/internal/config"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

// ansiRe strips ANSI escape sequences from execute's output - running helm
// under a pty (see execute) can make it emit color/cursor codes it
// wouldn't when piped, which would otherwise show up as garbled text in
// the TUI's plain-text result view.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*(\x07|\x1b\\)|\x1b[=>]`)

// execute runs helm attached to a pseudo-terminal (rather than plain
// pipes) and returns its combined output. A real terminal is required
// because some kubeconfig exec-credential plugins (e.g. the Rancher CLI's
// login flow) read their interactive prompts via the controlling tty
// directly instead of the process's stdin - a plain pipe is invisible to
// them and they just hang or fail. Output is watched for that prompt (see
// authPromptWatcher) so it gets answered and its login link opened
// automatically instead of stalling forever waiting for input nobody can
// provide.
func (c *Client) execute(args []string) (string, error) {
	cmd := exec.Command("helm", args...)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 220})
	if err != nil {
		return "", fmt.Errorf("helm error: %w", err)
	}
	defer ptmx.Close()

	watcher := newAuthPromptWatcher(ptmx)
	if _, copyErr := io.Copy(watcher, ptmx); copyErr != nil && !errors.Is(copyErr, syscall.EIO) {
		// A pty master read after the child exits commonly surfaces as
		// EIO on Linux instead of a clean io.EOF; anything else here is
		// unexpected but cmd.Wait() below still gives the real result.
		_ = copyErr
	}

	runErr := cmd.Wait()
	out := ansiRe.ReplaceAllString(watcher.buf.String(), "")
	if runErr != nil {
		return out, fmt.Errorf("helm error: %w", runErr)
	}
	return out, nil
}

// HistoryArgs, RollbackArgs, DeleteArgs, UpgradeArgs and InstallArgs are
// exposed separately from the Client methods that run them so callers can
// display the exact command a confirmation or result screen refers to.
func HistoryArgs(p config.ReleaseProfile) []string {
	return []string{"history", p.ReleaseName, "-n", p.Namespace}
}

func (c *Client) History(p config.ReleaseProfile) (string, error) {
	return c.execute(HistoryArgs(p))
}
func UpgradeArgs(p config.ReleaseProfile, dryRun bool) []string {
	args := []string{
		"upgrade",
		p.ReleaseName,
		p.Chart,
		"-n", p.Namespace,
		"--version", p.Version,
		"-f", p.RemoteValues,
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return args
}

func InstallArgs(p config.ReleaseProfile, dryRun bool) []string {
	args := []string{
		"install",
		p.ReleaseName,
		p.Chart,
		"-n", p.Namespace,
		"--version", p.Version,
		"-f", p.RemoteValues,
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return args
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\n") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// CommandString renders args as the "helm ..." command line a user would
// type, quoting any argument containing whitespace.
func CommandString(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "helm")
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

// MultilineCommandString renders args as a multi-line "helm ..." command,
// one flag (with its value) per line and "\" line continuations, so a long
// value (a chart URL, a values-file URL) doesn't force an unreadably wide
// single line in a narrow terminal.
func MultilineCommandString(args []string) string {
	var lines []string
	cur := []string{"helm"}
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			lines = append(lines, strings.Join(cur, " "))
			cur = []string{quoteIfNeeded(a)}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				cur = append(cur, quoteIfNeeded(args[i+1]))
				i++
			}
		} else {
			cur = append(cur, quoteIfNeeded(a))
		}
		i++
	}
	lines = append(lines, strings.Join(cur, " "))

	for idx := 0; idx < len(lines)-1; idx++ {
		lines[idx] += " \\"
	}
	lines[0] = "  " + lines[0]
	for idx := 1; idx < len(lines); idx++ {
		lines[idx] = "    " + lines[idx]
	}
	return strings.Join(lines, "\n")
}

func (c *Client) Upgrade(p config.ReleaseProfile, dryRun bool) (string, error) {
	return c.execute(UpgradeArgs(p, dryRun))
}

func (c *Client) Install(p config.ReleaseProfile, dryRun bool) (string, error) {
	return c.execute(InstallArgs(p, dryRun))
}

func RollbackArgs(p config.ReleaseProfile, revision int) []string {
	return []string{
		"rollback",
		p.ReleaseName,
		fmt.Sprintf("%d", revision),
		"-n", p.Namespace,
	}
}

func (c *Client) Rollback(p config.ReleaseProfile, revision int) (string, error) {
	return c.execute(RollbackArgs(p, revision))
}

// DeleteArgs uses "uninstall" - Helm 3 renamed "delete" (the Helm 2 name)
// to "uninstall"; the CLI has no "delete" subcommand anymore.
func DeleteArgs(p config.ReleaseProfile) []string {
	return []string{"uninstall", p.ReleaseName, "-n", p.Namespace}
}

func (c *Client) Delete(p config.ReleaseProfile) (string, error) {
	return c.execute(DeleteArgs(p))
}