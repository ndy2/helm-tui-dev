# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`helm-tui` is a terminal UI, built with [Bubble Tea](https://github.com/charmbracelet/bubbletea), for managing Helm releases from saved profiles across multiple Kubernetes contexts. It's installed as a `helm` plugin (`helm tui`). Release profiles (namespace, release name, chart, version, values URL) are saved once and then upgraded/installed/rolled back with a couple of keystrokes, with the exact `helm ...` command always shown and confirmed before it runs.

## Commands

```sh
go build ./...                          # build everything
go vet ./...                            # vet
go build -o bin/helm-tui ./cmd/helm-tui # build the actual plugin binary
gofmt -l .                              # check formatting (CI/dev convention; no formatting diffs should exist)
```

There is no test suite. Changes are verified by driving the built binary in a pty (e.g. manually, or via a pty-driving script) — there's no other way to exercise the Bubble Tea event loop.

### Local development against a real `helm` install

To iterate without reinstalling the plugin each time, symlink the plugin directory into Helm's plugin dir instead of copying it:

```sh
ln -s "$(pwd)" "$(helm env HELM_PLUGINS)/helm-tui"
```

After that, `go build -o bin/helm-tui ./cmd/helm-tui` is all that's needed between changes — `helm tui` picks up the new binary immediately. The plugin entry point is `plugin.sh` (dispatches the `tui` subcommand to `bin/helm-tui`) declared by `plugin.yaml`.

### Config file

Release profiles live in `~/.helm-tui.yaml` (`internal/config`), keyed by kube context name. The app works fully offline against this file when `kubectl`/a live cluster isn't available — useful for testing multi-context config without a cluster (see `NewModel`'s fallback logic in `internal/tui/model.go`).

## Architecture

### Package layout

- `cmd/helm-tui` — flag parsing (`--height`) and the Bubble Tea program bootstrap.
- `internal/config` — `Config`/`ReleaseProfile` types and YAML load/save for `~/.helm-tui.yaml`.
- `internal/kube` — thin `kubectl` wrapper (`GetCurrentContext`, `ListContexts`, `UseContext`).
- `internal/helm` — runs the actual `helm` CLI and models its arguments.
- `internal/tui` — the Bubble Tea `Model`: state machine, all key handling, rendering. This is almost the entire app (`model.go` is ~1700 lines, deliberately not yet split further).
- `internal/tui/components` — the flex-width table component shared by both tabs.
- `internal/tui/styles` — lipgloss style/border definitions.

### `internal/helm`: why it runs through a pty

`Client.execute` runs `helm` attached to a pseudo-terminal (`github.com/creack/pty`), not plain pipes. This is required because some kubeconfig exec-credential plugins (notably the Rancher CLI's login flow) read interactive prompts from the controlling tty directly rather than the process's stdin — a plain pipe just hangs. `authwatch.go`'s `authPromptWatcher` watches the pty output stream for the Rancher/Keycloak `Select auth provider:` prompt, answers it automatically (always picks the Keycloak entry), and opens the printed SSO login URL in the user's browser so the user only has to complete login there.

For every action (`History`, `Upgrade`, `Install`, `Rollback`, `Delete`), the CLI args are built by a standalone `*Args` function (`UpgradeArgs`, `InstallArgs`, ...) separate from the `Client` method that runs them. This lets the TUI show the exact command line (via `CommandString`/`MultilineCommandString`) in a confirmation prompt *before* running it, and again alongside the result — the args builder is the single source of truth for both.

### `internal/tui`: state machine shape

`Model` is one big `sessionState` state machine (`stateList`, `stateMenu`, `stateAddProfile`, `stateEditProfile`, `stateDeleteProfile`, `stateConfirmAction`, `stateRollbackInput`, `stateExecute`, `stateInlineEdit`, `stateReleaseList`). `Update` dispatches almost entirely on `m.state` inside the `tea.KeyMsg` case; each state's block owns its own key handling before falling through.

Two tabs share most of that state machine:
- **Context tab** (`stateList`) — releases in the currently active kube context, sourced from `m.list`/`m.table`. `[`/`]` cycles the active kube context (via `kube.UseContext`).
- **Release tab** (`stateReleaseList`) — a flat list of every `(release, context)` pair across *all* contexts (`m.releaseList`/`m.releaseTable`), grouped visually by release (consecutive rows for the same release blank the RELEASE cell — see `releaseRowDelegate.Render`).

Once a release is selected from either tab, the flow (`stateMenu`, `stateConfirmAction`, `stateExecute`, `stateRollbackInput`, `stateEditProfile`) is shared code — it doesn't know or care which tab it was entered from. Two mechanisms make that possible:
- `m.activeTab` + `backState()` — "back"/`esc` returns to whichever tab's top-level list state (`stateList` or `stateReleaseList`) is active.
- `m.confirmCancelState` — set explicitly whenever entering `stateConfirmAction`, since a confirmation can be cancelled back to more than just the top-level list (e.g. back to `stateMenu`).

Other structural points worth knowing before editing `model.go`:
- **Quick edit vs full edit**: `e` inline-edits just Chart Version / App Version (`isEditing` + `editingField`, one of `quickEditChartVer`/`quickEditAppVersion`) directly in the list row. `E` (from the selected-release menu) opens the full `stateEditProfile` form for every field. `a` opens `stateAddProfile`, the same form shape plus a Context field (since Add has no "current context" to assume when invoked from the Release tab).
- **App Version snapshot/release auto-switch**: `applyAppVersion` substitutes the embedded version in `RemoteValues` and, if the new version string contains `SNAPSHOT`, also repoints the Artifactory-style repo-channel path segment between `release` and `snapshot` (`releaseWordPattern`/`snapshotWordPattern`).
- **`isTyping()`/`canSwitchTab()`**: gate global single-key shortcuts (`q`, `s`, `tab`) so they type as ordinary characters while a text input (search, quick edit, any form) has focus, instead of quitting/switching tabs mid-input.
- **Search** (`s`): live-filters both tabs' underlying data (`matchesSearch`, applied in `sortedListItems`/`releaseRowListItems`) by release name/namespace substring; the query persists across tab switches until cleared with `esc`.
- **Window sizing** (`applyWindowSize`): the table/list are re-sized on every `tea.WindowSizeMsg`, clamped between `minContentWidth`/`maxContentWidth` and `minListHeight`/`maxListHeight`. `--height` (persisted to `config.UIHeight`) overrides the terminal-driven height with a fixed row count instead, clamped to `[minFixedListHeight, maxFixedListHeight]`.
- **Long-running helm calls**: `startHelmCmd` switches to `stateExecute`, shows a spinner, and runs the `helm` call in a `tea.Cmd` goroutine, returning a `helmResultMsg` — the UI never blocks on a CLI call.

### `internal/tui/components`: table layout

`SetTable` computes column widths from a mix of fixed-`Width` and `FlexFactor` columns (flex columns split the width remaining after fixed columns, weighted by factor; the last column absorbs any leftover from integer division). `RenderTable` draws the table header as a bordered box whose bottom border uses tee joints (not corners) so the caller's list body, drawn directly below via a matching bordered style, reads as one continuous box rather than two stacked ones — see `Model.renderPanel` in `internal/tui/model.go` for how the two are joined.
