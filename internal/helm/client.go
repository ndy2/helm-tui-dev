package helm

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/deukyun/helm-tui/internal/config"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

// execute runs helm and returns its combined output. Stdout/stderr are
// routed through an authPromptWatcher (rather than CombinedOutput) so a
// Rancher CLI login prompt from an expired kubeconfig exec-credential
// plugin - which can appear mid-command on any cluster-talking call - gets
// answered and its login link opened automatically instead of hanging
// forever waiting for terminal input nobody can provide.
func (c *Client) execute(args []string) (string, error) {
	cmd := exec.Command("helm", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("helm error: %w", err)
	}

	watcher := newAuthPromptWatcher(stdin)
	cmd.Stdout = watcher
	cmd.Stderr = watcher

	runErr := cmd.Run()
	out := watcher.buf.String()
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

func DeleteArgs(p config.ReleaseProfile) []string {
	return []string{"delete", p.ReleaseName, "-n", p.Namespace}
}

func (c *Client) Delete(p config.ReleaseProfile) (string, error) {
	return c.execute(DeleteArgs(p))
}