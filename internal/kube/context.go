package kube

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// GetCurrentContext returns the current kubernetes context using kubectl.
func GetCurrentContext() (string, error) {
	cmd := exec.Command("kubectl", "config", "current-context")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current kubernetes context: %w", err)
	}

	context := strings.TrimSpace(string(out))
	if context == "" {
		return "", fmt.Errorf("current context is empty")
	}

	return context, nil
}

// ListContexts returns every kubernetes context name known to kubectl,
// sorted alphabetically.
func ListContexts() ([]string, error) {
	cmd := exec.Command("kubectl", "config", "get-contexts", "-o", "name")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list kubernetes contexts: %w", err)
	}

	var contexts []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			contexts = append(contexts, line)
		}
	}
	sort.Strings(contexts)
	return contexts, nil
}

// UseContext switches the active kubernetes context via kubectl, so the
// change also applies outside the TUI (e.g. to any other kubectl/helm
// invocation on the same kubeconfig).
func UseContext(name string) error {
	cmd := exec.Command("kubectl", "config", "use-context", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to switch to context %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}