# helm-tui

A terminal UI (built with [Bubble Tea](https://github.com/charmbracelet/bubbletea)) for managing Helm releases from a set of pre-defined profiles, across multiple Kubernetes contexts, as a `helm` plugin.

Instead of hand-typing `helm upgrade <release> <chart> -n <namespace> --version <ver> -f <values-url>` every time, you save each release as a profile once, then upgrade/install/rollback it with a couple of keystrokes - with the exact command always shown and confirmed before it runs.

## Features

- **Two views on the same data**
  - **Context tab** - releases grouped under the currently active kube context, with `[` / `]` to cycle contexts.
  - **Release tab** - a flat list of every (release, context) pair across *all* contexts, so you can see and act on the same release everywhere it's deployed.
- **Search** (`s`) - live-filters either tab's list by release name or namespace; stays applied across tab switches until cleared.
- **Quick edit** (`e`) - bump just the Chart Version or App Version inline, without opening the full edit form.
- **Full edit / add profile** - edit every field of a profile, or add a new one (from either tab; adding from the Release tab lets you pick which context it belongs to).
- **Confirm-before-run** - Upgrade, Install, and Delete always show the exact `helm ...` command (wrapped across multiple lines when it's long) and require an explicit yes.
- **Dry-run** - from the upgrade/install confirmation, run the same command with `--dry-run` instead of applying it.
- **Snapshot/release auto-switch** - editing the App Version to a value containing `SNAPSHOT` automatically repoints an Artifactory-style values URL from its `release` channel segment to `snapshot` (and back), e.g. `.../generic-release-local/...` &harr; `.../generic-snapshot-local/...`.
- **History, Rollback, Delete** - with the command that ran shown alongside the result.
- **Rancher/Keycloak SSO auto-login** - if a `helm`/`kubectl` call hits an expired-token login prompt (Rancher CLI's `Select auth provider:` flow), it's answered automatically (always picks the Keycloak provider) and the login URL is opened in your browser for you.
- Works without a live cluster/`kubectl` for browsing and editing your saved profiles - useful for setting up or testing a multi-context config offline.

## Requirements

- [`helm`](https://helm.sh/) on your `PATH`.
- `kubectl` configured with the contexts you want to use (optional - without it, the app falls back to the contexts already defined in your config file, so you can still browse/edit).
- Go 1.24+ (only to build from source).

## Install

As a Helm plugin:

```sh
git clone <this-repo> helm-tui
cd helm-tui
go build -o bin/helm-tui ./cmd/helm-tui
helm plugin install .
```

Then run it with:

```sh
helm tui
```

### Local development

To iterate without reinstalling the plugin each time, symlink the plugin directory to your checkout instead of copying it:

```sh
ln -s "$(pwd)" "$(helm env HELM_PLUGINS)/helm-tui"
```

After that, `go build -o bin/helm-tui ./cmd/helm-tui` is all you need between changes - `helm tui` will pick up the new binary immediately.

## Configuration

Release profiles are stored in `~/.helm-tui.yaml`, grouped by kube context:

```yaml
contexts:
  dev-cluster:
    - namespace: my-namespace
      releaseName: my-app
      chart: my-repo/my-app
      version: 1.2.3
      remoteValues: https://example.com/artifactory/generic-release-local/my-app/1.2.3.4/values.yaml
      lastSelected: 0
  prod-cluster:
    - namespace: my-namespace
      releaseName: my-app
      chart: my-repo/my-app
      version: 1.2.0
      remoteValues: https://example.com/artifactory/generic-release-local/my-app/1.2.0.4/values.yaml
      lastSelected: 0
```

You can create/edit entries entirely from the TUI (`a` to add, `e` to edit) - hand-editing the file is only needed for bulk setup.

## Keybindings

**Global**

| Key | Action |
| --- | --- |
| `tab` / `shift+tab` | Switch between Context and Release tabs |
| `s` | Start/edit search |
| `ctrl+c` | Quit |
| `q` | Quit (outside of any text input) |

**Browsing a list** (Context or Release tab)

| Key | Action |
| --- | --- |
| `↑` / `↓` | Move selection (wraps past the ends) |
| `enter` | Select a release, opening its action menu |
| `a` | Add a new profile |
| `e` | Quick-edit Chart Version / App Version |
| `u` | Upgrade (with confirmation) |
| `[` / `]` | Previous/next kube context (Context tab only) |
| `esc` | Clear an active search filter |

**Quick edit**

| Key | Action |
| --- | --- |
| `tab` / `shift+tab` | Switch between Chart Version and App Version |
| `enter` | Save |
| `esc` | Cancel |

**Selected release (action menu)**

| Key | Action |
| --- | --- |
| `h` | History |
| `u` | Upgrade |
| `i` | Install |
| `r` | Rollback |
| `d` | Delete release |
| `e` | Edit profile (full form) |
| `x` | Delete profile (just the saved entry, not the release) |
| `↑` / `↓` | Switch to the previous/next release, staying in the menu |
| `[` / `]` | Previous/next kube context, staying in the menu (Context tab only) |
| `esc` | Back to the list |

**Confirmation / add / edit forms**

| Key | Action |
| --- | --- |
| `y` / `enter` | Confirm |
| `d` | Dry-run instead (upgrade/install confirmations only) |
| `n` / `esc` | Cancel |
| `tab` / `shift+tab` | Move between form fields |

## Development

```sh
go build ./...
go vet ./...
go build -o bin/helm-tui ./cmd/helm-tui
```

There's no test suite yet - changes are currently verified by driving the built binary in a pty.
