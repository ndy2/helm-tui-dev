package helm

import (
	"fmt"
	"os/exec"

	"github.com/deukyun/helm-tui/internal/config"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) execute(args []string) (string, error) {
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("helm error: %w", err)
	}
	return string(out), nil
}

func (c *Client) History(p config.ReleaseProfile) (string, error) {
	args := []string{"history", p.ReleaseName, "-n", p.Namespace}
	return c.execute(args)
}

func (c *Client) Upgrade(p config.ReleaseProfile) (string, error) {
	args := []string{
		"upgrade",
		p.ReleaseName,
		p.Chart,
		"-n", p.Namespace,
		"--version", p.Version,
		"-f", p.RemoteValues,
	}
	return c.execute(args)
}

func (c *Client) Install(p config.ReleaseProfile) (string, error) {
	args := []string{
		"install",
		p.ReleaseName,
		p.Chart,
		"-n", p.Namespace,
		"--version", p.Version,
		"-f", p.RemoteValues,
	}
	return c.execute(args)
}

func (c *Client) Rollback(p config.ReleaseProfile, revision int) (string, error) {
	args := []string{
		"rollback",
		p.ReleaseName,
		fmt.Sprintf("%d", revision),
		"-n", p.Namespace,
	}
	return c.execute(args)
}

func (c *Client) Delete(p config.ReleaseProfile) (string, error) {
	args := []string{"delete", p.ReleaseName, "-n", p.Namespace}
	return c.execute(args)
}