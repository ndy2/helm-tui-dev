package helm

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// authPromptWatcher watches a running helm process's pty output for a
// Rancher CLI exec-credential login prompt (shown when the cached auth
// token has expired) and answers it automatically: it always picks the
// keyCloakProvider entry, writing the choice back to the same pty, then
// opens the printed login URL in the user's browser so they only have to
// complete the SSO login there instead of babysitting the terminal.
// Example of the prompt this reacts to:
//
//	Auth providers:
//	0 - localProvider
//	1 - keyCloakProvider
//	Select auth provider:
//	Login to Rancher Server at https://.../auth/login?cli=true&...
type authPromptWatcher struct {
	buf     bytes.Buffer
	stdin   io.Writer
	pending string

	keycloakIndex string
	answered      bool
	opened        bool
}

func newAuthPromptWatcher(stdin io.Writer) *authPromptWatcher {
	return &authPromptWatcher{stdin: stdin, keycloakIndex: "1"}
}

var (
	authProviderLineRe = regexp.MustCompile(`(?m)^\s*(\d+)\s*-\s*(\S+)\s*$`)
	authLoginURLRe     = regexp.MustCompile(`Login to [^\n]*?(https?://\S+)`)
)

// Write implements io.Writer, fed the pty's output as execute reads it.
func (w *authPromptWatcher) Write(p []byte) (int, error) {
	w.buf.Write(p)
	w.pending += string(p)

	for _, m := range authProviderLineRe.FindAllStringSubmatch(w.pending, -1) {
		if strings.Contains(strings.ToLower(m[2]), "keycloak") {
			w.keycloakIndex = m[1]
		}
	}

	if !w.answered && strings.Contains(w.pending, "Select auth provider") {
		w.answered = true
		fmt.Fprintln(w.stdin, w.keycloakIndex)
	}

	if !w.opened {
		if m := authLoginURLRe.FindStringSubmatch(w.pending); m != nil {
			w.opened = true
			_ = openBrowser(m[1])
		}
	}

	return len(p), nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
