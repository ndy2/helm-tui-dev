package kube

import (
	"fmt"
	"os/exec"
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