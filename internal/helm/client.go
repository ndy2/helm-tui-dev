package helm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/mattn/go-runewidth"

	"github.com/deukyun/helm-tui/internal/config"
)

type Client struct {
	mu   sync.Mutex
	ptmx *os.File // pty master of the in-flight helm command, if any

	// browserOpened receives the login URL each time authPromptWatcher
	// opens the user's browser for an SSO login (see authwatch.go), so the
	// TUI can show a live notice while the command is still running.
	browserOpened chan string
}

func NewClient() *Client {
	return &Client{browserOpened: make(chan string, 1)}
}

// BrowserOpened is fed a URL each time an in-flight helm command's
// auth-prompt watcher opens the browser for an SSO login.
func (c *Client) BrowserOpened() <-chan string {
	return c.browserOpened
}

func (c *Client) notifyBrowserOpened(url string) {
	// Non-blocking: execute's pty-reading loop must never stall waiting
	// for a listener, whether or not the TUI is currently draining this.
	select {
	case c.browserOpened <- url:
	default:
	}
}

// Cancel interrupts the in-flight helm command, if any, by writing a
// Ctrl+C byte to its pty - the same signal sent if the user pressed
// Ctrl+C in a real terminal running helm directly.
func (c *Client) Cancel() {
	c.mu.Lock()
	ptmx := c.ptmx
	c.mu.Unlock()
	if ptmx != nil {
		_, _ = ptmx.Write([]byte{0x03})
	}
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

	c.mu.Lock()
	c.ptmx = ptmx
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ptmx = nil
		c.mu.Unlock()
	}()

	watcher := newAuthPromptWatcher(ptmx, c.notifyBrowserOpened)
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

const (
	mlcsFirstIndent = "  "
	mlcsContIndent  = "    "
	mlcsWrapIndent  = "      "
	mlcsContMarker  = " \\"
)

// MultilineCommandString renders args as a multi-line "helm ..." command,
// one flag (with its value) per line and "\" line continuations, so a long
// value (a chart URL, a values-file URL) doesn't force an unreadably wide
// single line in a narrow terminal.
//
// width is the display width (in columns) each rendered line should fit
// within; a flag/value segment wider than that (e.g. a long values-file
// URL) is further hard-wrapped, rune-width aware, with its own "\"
// continuations so the command stays syntactically pasteable. width <= 0
// disables this extra wrapping.
func MultilineCommandString(args []string, width int) string {
	var segments []string
	cur := []string{"helm"}
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			segments = append(segments, strings.Join(cur, " "))
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
	segments = append(segments, strings.Join(cur, " "))

	var lines []string
	for idx, seg := range segments {
		indent := mlcsContIndent
		if idx == 0 {
			indent = mlcsFirstIndent
		}
		lines = append(lines, wrapCommandSegment(seg, indent, width)...)
	}

	for idx := 0; idx < len(lines)-1; idx++ {
		lines[idx] += mlcsContMarker
	}
	return strings.Join(lines, "\n")
}

// wrapCommandSegment renders indent+seg as a single line, or, if that
// exceeds width, word-wraps seg (rune-width aware) across multiple lines:
// the first prefixed with indent, the rest with mlcsWrapIndent. A single
// word too long to fit even alone on a line (e.g. a long values-file URL)
// is hard-wrapped by character width; words are otherwise kept whole so
// e.g. a chart name never gets split mid-word.
func wrapCommandSegment(seg, indent string, width int) []string {
	full := indent + seg
	if width <= 0 || runewidth.StringWidth(full) <= width {
		return []string{full}
	}

	markerWidth := runewidth.StringWidth(mlcsContMarker)
	firstBudget := width - runewidth.StringWidth(indent) - markerWidth
	if firstBudget < 1 {
		firstBudget = 1
	}
	contBudget := width - runewidth.StringWidth(mlcsWrapIndent) - markerWidth
	if contBudget < 1 {
		contBudget = 1
	}

	var lines []string
	curIndent, curBudget := indent, firstBudget
	var cur []string
	curWidth := 0

	flush := func() {
		if len(cur) == 0 {
			return
		}
		lines = append(lines, curIndent+strings.Join(cur, " "))
		cur = nil
		curWidth = 0
		curIndent, curBudget = mlcsWrapIndent, contBudget
	}

	for _, word := range splitWords(seg) {
		wordWidth := runewidth.StringWidth(word)
		for wordWidth > curBudget {
			// The word alone doesn't fit even on an empty line: hard-wrap
			// it by character width rather than overflowing.
			flush()
			var chunk string
			chunk, word = takeRuneWidth(word, curBudget)
			lines = append(lines, curIndent+chunk)
			curIndent, curBudget = mlcsWrapIndent, contBudget
			wordWidth = runewidth.StringWidth(word)
		}
		sep := 0
		if len(cur) > 0 {
			sep = 1
		}
		if len(cur) > 0 && curWidth+sep+wordWidth > curBudget {
			flush()
			sep = 0
		}
		curWidth += sep + wordWidth
		cur = append(cur, word)
	}
	flush()
	return lines
}

// splitWords splits s on spaces that are not inside a double-quoted
// substring, so a quoted value with embedded spaces (see quoteIfNeeded)
// wraps as a single unit instead of having its quoting broken across lines.
func splitWords(s string) []string {
	var words []string
	var cur []rune
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur = append(cur, r)
		case r == ' ' && !inQuotes:
			if len(cur) > 0 {
				words = append(words, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

// takeRuneWidth splits off a prefix of s whose total display width fits
// within width (always taking at least one rune, so it makes progress even
// when a single wide rune exceeds width on its own).
func takeRuneWidth(s string, width int) (taken, rest string) {
	w := 0
	runes := []rune(s)
	for i, r := range runes {
		rw := runewidth.RuneWidth(r)
		if w+rw > width && i > 0 {
			return string(runes[:i]), string(runes[i:])
		}
		w += rw
	}
	return s, ""
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