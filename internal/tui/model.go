package tui

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/deukyun/helm-tui/internal/config"
	"github.com/deukyun/helm-tui/internal/helm"
	"github.com/deukyun/helm-tui/internal/kube"
	"github.com/deukyun/helm-tui/internal/tui/components"
	"github.com/deukyun/helm-tui/internal/tui/styles"
)

var docStyle = lipgloss.NewStyle().Margin(1, 2)
var errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
var selectedStyle = lipgloss.NewStyle().Foreground(styles.HighlightColor).Bold(true)
var normalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
var cursorStyle = lipgloss.NewStyle().Background(styles.HighlightColor).Foreground(lipgloss.Color("252"))

// keyStyle highlights a key name; hintStyle is the muted gray used for the
// action it performs, so key hint lines read as "key  action" without
// needing quotes around the key.
var keyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
var hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

type keyHint struct {
	key   string
	label string
}

// hintLine renders key hints on a single line, separated by a muted " • ".
func hintLine(hints ...keyHint) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = keyStyle.Render(h.key) + hintStyle.Render(" "+h.label)
	}
	return strings.Join(parts, hintStyle.Render(" • "))
}

// hintLines renders key hints one per line, for vertical menus.
func hintLines(hints ...keyHint) string {
	lines := make([]string, len(hints))
	for i, h := range hints {
		lines[i] = keyStyle.Render(h.key) + hintStyle.Render("  "+h.label)
	}
	return strings.Join(lines, "\n")
}

type sessionState int

const (
	stateList sessionState = iota
	stateMenu
	stateAddProfile
	stateEditProfile
	stateDeleteProfile
	stateConfirmAction
	stateRollbackInput
	stateExecute
	stateInlineEdit
	stateReleaseList
)

// tabKind selects which top-level view is active: releases grouped by the
// current kube context, or releases grouped by name across every context.
type tabKind int

const (
	contextTab tabKind = iota
	releaseTab
)

const (
	// minContentWidth/minListHeight keep the table and list usable even if
	// the terminal is resized very small. maxContentWidth keeps it from
	// stretching into an unreadably wide table on a very wide terminal.
	minContentWidth = 60
	maxContentWidth = 120
	minListHeight   = 3
	maxListHeight   = 10
	// chromeHeight is everything drawn around the list rows (margin, the
	// context line, the table's own borders/header/divider, and breathing
	// room for the panel below the table). It's an estimate, not exact,
	// since the bottom panel's height varies by state.
	chromeHeight = 13
)

// helmResultMsg carries the result of a helm CLI call run in the
// background via a tea.Cmd (see startHelmCmd).
type helmResultMsg struct {
	output string
	err    error
}

func helmCmd(fn func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		out, err := fn()
		return helmResultMsg{output: out, err: err}
	}
}

type Model struct {
	state          sessionState
	config         *config.Config
	list           list.Model
	table          table.Model
	cursor         int
	selected       config.ReleaseProfile
	err            error

	currentContext    string
	availableContexts []string

	// Release tab: one flat list of every (release, context) pair across
	// every context, grouped visually by release.
	activeTab        tabKind
	releaseList      list.Model
	releaseTable     table.Model
	releaseTableCols []components.ColumnDefinition

	// Search, shown at the right end of the tab bar ('s' to start typing).
	// Filters both tabs' lists by release name / namespace substring match.
	searching   bool
	searchInput textinput.Model
	searchQuery string

	// Inline editing state
	isEditing    bool
	editingField int // quickEditChartVer or quickEditAppVersion
	editingInput textinput.Model

	// Form for adding profile
	inputs []textinput.Model
	focus  int

	// Confirmation and Input state
	confirmMsg          string
	confirmAction        func() tea.Cmd
	confirmDryRunAction  func() tea.Cmd // non-nil only for upgrade/install confirmations
	confirmCancelState   sessionState   // where "n"/esc returns to
	rollbackInput        textinput.Model

	// For command execution
	helmClient   *helm.Client
	output       string
	lastCommand  string // the "helm ..." line the last startHelmCmd ran, shown with the result
	loading      bool
	loadingLabel string
	spinner      spinner.Model

	// Window size, applied to the table/list on resize (see applyWindowSize).
	width        int
	height       int
	fixedHeight  int // if >0 (set via --height), overrides the terminal-reported height
	contentWidth int
	tableCols    []components.ColumnDefinition
}

type releaseItem struct {
	profile config.ReleaseProfile
}

func (i releaseItem) Title() string       { return i.profile.ReleaseName }
func (i releaseItem) Description() string { return i.profile.Namespace }
func (i releaseItem) FilterValue() string { return i.Title() }

// appVersionPattern matches a semantic version optionally followed by a
// pre-release/build suffix (e.g. "1.0.1.1-AIE-948-7-SNAPSHOT"), as it
// appears embedded in a RemoteValues URL.
var appVersionPattern = regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+(?:-[a-zA-Z0-9\.\-]+)?)`)

// releaseWordPattern and snapshotWordPattern match the "release"/"snapshot"
// repository-channel word in an Artifactory-style URL path segment, e.g.
// ".../lml-generic-release-local/...".
var releaseWordPattern = regexp.MustCompile(`\brelease\b`)
var snapshotWordPattern = regexp.MustCompile(`\bsnapshot\b`)

func extractAppVersion(url string) string {
	match := appVersionPattern.FindString(url)
	if match == "" {
		return "N/A"
	}
	return match
}

// applyAppVersion substitutes newVer into url's embedded version. If newVer
// looks like a snapshot build (contains "SNAPSHOT"), it also repoints the
// Artifactory repo-channel segment from release to snapshot (e.g.
// "lml-generic-release-local" -> "lml-generic-snapshot-local"), and vice
// versa for a plain release version - so quick-editing just the App
// Version is enough to also switch which Artifactory repo it's pulled from.
func applyAppVersion(url, newVer string) string {
	result := appVersionPattern.ReplaceAllString(url, newVer)
	if strings.Contains(strings.ToUpper(newVer), "SNAPSHOT") {
		return releaseWordPattern.ReplaceAllString(result, "snapshot")
	}
	return snapshotWordPattern.ReplaceAllString(result, "release")
}

// renderRow lays out fields under cols the same way the table header does
// (see components.SetTable), so list rows line up with the header cells
// above them. The cursor prefix eats into the first column's width, and
// each field but the last is truncated to width-1 so a fully-truncated
// value doesn't run straight into the next column.
func renderRow(cols []table.Column, selected bool, fields []string) string {
	cursor := "  "
	if selected {
		cursor = "● "
	}

	var b strings.Builder
	b.WriteString(cursor)
	for idx, field := range fields {
		width := 0
		if idx < len(cols) {
			width = cols[idx].Width
		}
		if idx == 0 {
			width = max(width-2, 0)
		}
		truncWidth := width
		if idx < len(fields)-1 {
			truncWidth = max(width-1, 0)
		}
		fmt.Fprintf(&b, "%-*s", width, runewidth.Truncate(field, truncWidth, "…"))
	}

	rowStyle := normalStyle
	if selected {
		rowStyle = selectedStyle
	}
	return rowStyle.Render(b.String())
}

type releaseDelegate struct {
	model *Model
}

func (d releaseDelegate) Height() int { return 1 }
func (d releaseDelegate) Spacing() int { return 0 }
func (d releaseDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return nil
}
func (d releaseDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i := item.(releaseItem)
	appVer := extractAppVersion(i.profile.RemoteValues)

	selected := index == m.Index()

	// Inline Editing logic. Only Chart Version and App Version are
	// quick-editable (see quickEditChartVer/quickEditAppVersion) - Release
	// Name and Namespace are shown but not part of the edit cycle.
	fields := []string{
		i.profile.ReleaseName,
		i.profile.Namespace,
		i.profile.Version,
		appVer,
	}

	if selected && d.model.isEditing {
		col := d.model.editingField + 2 // fields[2]=chart ver, fields[3]=app version
		if col >= 0 && col < len(fields) {
			fields[col] = fmt.Sprintf("[%s]", d.model.editingInput.View())
		}
	}

	fmt.Fprintf(w, "%s", renderRow(d.model.table.Columns(), selected, fields))
}

// releaseRowItem is a row in the Release tab's flat list: one (release,
// context) pair - i.e. one profile, wherever in Config.Contexts it lives.
type releaseRowItem struct {
	context string
	profile config.ReleaseProfile
}

func (i releaseRowItem) Title() string       { return i.profile.ReleaseName }
func (i releaseRowItem) Description() string { return i.context }
func (i releaseRowItem) FilterValue() string { return i.profile.ReleaseName + " " + i.context }

// quickEditChartVer/quickEditAppVersion index the only two fields quick
// edit ever touches, on both tabs: Release Name/Namespace/Chart are only
// editable via the full "Edit Profile" form, since bumping versions is by
// far the most common quick edit and the only one worth a fast path.
const (
	quickEditChartVer int = iota
	quickEditAppVersion
)

type releaseRowDelegate struct {
	model *Model
}

func (d releaseRowDelegate) Height() int  { return 1 }
func (d releaseRowDelegate) Spacing() int { return 0 }
func (d releaseRowDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return nil
}
func (d releaseRowDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i := item.(releaseRowItem)
	selected := index == m.Index()

	// Blank the RELEASE cell on every row but the first for a given
	// release, so consecutive rows for the same release read as one
	// merged group instead of repeating the name.
	releaseName := i.profile.ReleaseName
	if index > 0 {
		if prev, ok := m.Items()[index-1].(releaseRowItem); ok && prev.profile.ReleaseName == releaseName {
			releaseName = ""
		}
	}

	fields := []string{releaseName, i.context, i.profile.Namespace, i.profile.Version, extractAppVersion(i.profile.RemoteValues)}

	if selected && d.model.isEditing {
		col := d.model.editingField + 3 // fields[3]=chart ver, fields[4]=app version
		if col >= 0 && col < len(fields) {
			fields[col] = fmt.Sprintf("[%s]", d.model.editingInput.View())
		}
	}

	fmt.Fprintf(w, "%s", renderRow(d.model.releaseTable.Columns(), selected, fields))
}

// sortedContextNames returns the context names present in cfg, sorted.
func sortedContextNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NewModel builds the initial Model. fixedHeight, if >0 (from the --height
// flag), pins the layout height regardless of what the terminal reports via
// tea.WindowSizeMsg - useful when running inside something that doesn't
// report a usable size, or to constrain the app to less than the full
// terminal. Pass 0 to always follow the terminal's reported size.
func NewModel(fixedHeight int) (*Model, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}

	// configContexts are the context names already present in the loaded
	// YAML, sorted. When kubectl can't tell us the real current context
	// (not installed, no kubeconfig, etc.), falling back to the first of
	// these - instead of a synthetic "unknown" - lets a multi-context YAML
	// be browsed/tested without a real cluster.
	configContexts := sortedContextNames(cfg)

	ctx, err := kube.GetCurrentContext()
	if err != nil {
		if len(configContexts) > 0 {
			ctx = configContexts[0]
		} else {
			ctx = "unknown"
		}
	}

	availableContexts, err := kube.ListContexts()
	if err != nil || len(availableContexts) == 0 {
		if len(configContexts) > 0 {
			availableContexts = configContexts
		} else {
			availableContexts = []string{ctx}
		}
	}

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(styles.HighlightColor)

	// Create model first so we can pass it to delegate
	m := &Model{
		state:             stateList,
		config:            cfg,
		currentContext:    ctx,
		availableContexts: availableContexts,
		helmClient:        helm.NewClient(),
		spinner:           sp,
		// Sane defaults until the first tea.WindowSizeMsg arrives.
		width:       120,
		height:      30,
		fixedHeight: fixedHeight,
	}
	if fixedHeight > 0 {
		m.height = fixedHeight
	}

	// Initialize Table
	m.table = components.GenerateTable()
	m.tableCols = []components.ColumnDefinition{
		{Title: "RELEASE", Width: 20},
		{Title: "NAMESPACE", Width: 15},
		{Title: "CHART VER", Width: 12},
		{Title: "APP VERSION", FlexFactor: 1},
	}

	l := list.New(m.sortedListItems(ctx), releaseDelegate{model: m}, 0, 0)
	l.SetShowTitle(false) // the releases table header already shows this
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	l.InfiniteScrolling = true // down past the last item wraps to the first, and vice versa
	// bubbles' default list keymap binds both "q" and "esc" to its own
	// internal quit - which returns tea.Quit straight out of Update() and
	// would silently exit the whole app on a bare esc/q press while
	// browsing. The app manages its own quit key (ctrl+c) and esc
	// behavior, so disable list's.
	l.DisableQuitKeybindings()
	m.list = l

	// Release tab: flat (release, context) list
	m.releaseTableCols = []components.ColumnDefinition{
		{Title: "RELEASE", Width: 22},
		{Title: "CONTEXT", Width: 17},
		{Title: "NAMESPACE", Width: 13},
		{Title: "CHART VER", Width: 12},
		{Title: "APP VERSION", FlexFactor: 1},
	}
	m.releaseTable = components.GenerateTable()

	rl := list.New(m.releaseRowListItems(), releaseRowDelegate{model: m}, 0, 0)
	rl.SetShowTitle(false)
	rl.SetShowStatusBar(false)
	rl.SetFilteringEnabled(false)
	rl.SetShowHelp(false)
	rl.InfiniteScrolling = true
	rl.DisableQuitKeybindings()
	m.releaseList = rl

	m.applyWindowSize()

	return m, nil
}

// matchesSearch reports whether p's release name or namespace contains
// query (case-insensitive). An empty query matches everything.
func matchesSearch(query string, p config.ReleaseProfile) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(p.ReleaseName), q) || strings.Contains(strings.ToLower(p.Namespace), q)
}

// sortedListItems returns the release items for ctx matching the active
// search query (see matchesSearch), sorted by LastSelected (desc) then
// ReleaseName (asc).
func (m *Model) sortedListItems(ctx string) []list.Item {
	releases := m.config.GetReleasesForContext(ctx)
	filtered := make([]config.ReleaseProfile, 0, len(releases))
	for _, r := range releases {
		if matchesSearch(m.searchQuery, r) {
			filtered = append(filtered, r)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].LastSelected != filtered[j].LastSelected {
			return filtered[i].LastSelected > filtered[j].LastSelected
		}
		return filtered[i].ReleaseName < filtered[j].ReleaseName
	})

	items := make([]list.Item, 0, len(filtered))
	for _, r := range filtered {
		items = append(items, releaseItem{profile: r})
	}
	return items
}

// setContext applies name via kubectl so it also takes effect outside the
// TUI, and reloads the context tab's release list for it. The local switch
// always happens, even if kubectl fails or isn't installed (e.g. testing a
// multi-context YAML with no cluster available) - the error is still
// recorded so it's visible, it just doesn't block browsing.
func (m *Model) setContext(name string) error {
	err := kube.UseContext(name)
	m.err = err
	m.currentContext = name
	m.list.SetItems(m.sortedListItems(name))
	return err
}

// switchContext moves the current kubernetes context by delta (+1/-1)
// within availableContexts and applies it via setContext.
func (m *Model) switchContext(delta int) {
	n := len(m.availableContexts)
	if n <= 1 {
		return
	}

	idx := 0
	for i, c := range m.availableContexts {
		if c == m.currentContext {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%n + n) % n
	m.setContext(m.availableContexts[idx])
}

// releaseRowListItems returns every (context, profile) pair across every
// context matching the active search query (see matchesSearch), sorted by
// ReleaseName then context, for the Release tab's flat list.
func (m *Model) releaseRowListItems() []list.Item {
	type row struct {
		ctx     string
		profile config.ReleaseProfile
	}
	var rows []row
	for ctx, profiles := range m.config.Contexts {
		for _, p := range profiles {
			if !matchesSearch(m.searchQuery, p) {
				continue
			}
			rows = append(rows, row{ctx: ctx, profile: p})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].profile.ReleaseName != rows[j].profile.ReleaseName {
			return rows[i].profile.ReleaseName < rows[j].profile.ReleaseName
		}
		return rows[i].ctx < rows[j].ctx
	})

	items := make([]list.Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, releaseRowItem{context: r.ctx, profile: r.profile})
	}
	return items
}

// refreshLists recomputes every list that could be affected by a config
// mutation (add/edit/delete profile), so the Context and Release tabs stay
// in sync regardless of which tab the mutation happened in.
func (m *Model) refreshLists() {
	m.list.SetItems(m.sortedListItems(m.currentContext))
	m.releaseList.SetItems(m.releaseRowListItems())
}

// isTyping reports whether a text input currently owns the keyboard (quick
// edit, search, or one of the profile forms), so global single-key
// shortcuts like "q" and "s" should be treated as ordinary characters
// instead.
func (m *Model) isTyping() bool {
	if m.isEditing || m.searching {
		return true
	}
	switch m.state {
	case stateAddProfile, stateEditProfile, stateRollbackInput, stateInlineEdit:
		return true
	}
	return false
}

// canSwitchTab reports whether the tab/shift+tab key should toggle the
// active tab right now, rather than being handled by the current state
// (form field navigation, inline-edit field navigation, etc.).
func (m *Model) canSwitchTab() bool {
	if m.isTyping() {
		return false
	}
	switch m.state {
	case stateList, stateReleaseList, stateMenu:
		return true
	}
	return false
}

// switchTab toggles the active tab and moves to that tab's top-level
// browsing state.
func (m *Model) switchTab() {
	if m.activeTab == contextTab {
		m.activeTab = releaseTab
		m.state = stateReleaseList
	} else {
		m.activeTab = contextTab
		m.state = stateList
	}
	m.refreshLists()
}

// backState is the state to return to when backing out of the shared
// stateMenu/stateExecute/etc. flow, depending on which tab it was entered
// from.
func (m *Model) backState() sessionState {
	if m.activeTab == releaseTab {
		return stateReleaseList
	}
	return stateList
}

// fieldNavDelta returns +1/-1 if msg should move a form/quick-edit's focus
// to the next/previous field - tab or down for next, shift+tab or up for
// previous - or 0 if msg isn't a field-navigation key. Forms are single-line
// text fields, so up/down have no other meaning to conflict with.
func fieldNavDelta(msg tea.KeyMsg) int {
	switch msg.String() {
	case "tab", "down":
		return 1
	case "shift+tab", "up":
		return -1
	}
	return 0
}

// navigateSelection moves the active tab's underlying list cursor per msg
// (an up/down key) and updates m.selected (and, on the Release tab, the
// active context) to match - so the action menu can stay open on stateMenu
// while the user arrows through releases.
func (m *Model) navigateSelection(msg tea.KeyMsg) tea.Cmd {
	if m.activeTab == releaseTab {
		var cmd tea.Cmd
		m.releaseList, cmd = m.releaseList.Update(msg)
		if item, ok := m.releaseList.SelectedItem().(releaseRowItem); ok {
			m.setContext(item.context)
			m.selected = item.profile
		}
		return cmd
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if item, ok := m.list.SelectedItem().(releaseItem); ok {
		m.selected = item.profile
	}
	return cmd
}

// switchMenuContext moves the active kube context by delta (see
// switchContext) while staying in stateMenu if possible: it looks for a
// profile with the same ReleaseName as the currently selected one in the
// new context and keeps that selected (syncing the Context tab's list
// cursor to match), so "[" / "]" work from the selected state the same way
// they do while browsing. If the new context has no release by that name,
// there's nothing sensible left to show in the menu, so it backs out to
// the tab's top-level list instead. Only called for the Context tab - on
// the Release tab, up/down already moves between a release's contexts
// (adjacent rows in the flat list), so a separate context switch would be
// redundant and could land on an arbitrary, unrelated context.
func (m *Model) switchMenuContext(delta int) {
	name := m.selected.ReleaseName
	m.switchContext(delta)

	found := false
	for _, p := range m.config.Contexts[m.currentContext] {
		if p.ReleaseName == name {
			m.selected = p
			found = true
			break
		}
	}
	if !found {
		m.state = m.backState()
		return
	}
	if m.activeTab == contextTab {
		for i, it := range m.list.Items() {
			if ri, ok := it.(releaseItem); ok && ri.profile.ReleaseName == name {
				m.list.Select(i)
				break
			}
		}
	}
}

// applyWindowSize recomputes the table's column widths and the list's
// height from the model's current width/height, clamped to a usable
// range. Called on startup and on every tea.WindowSizeMsg.
func (m *Model) applyWindowSize() {
	// docStyle's outer margin (2 cols each side) plus the table's own
	// left/right border (1 col each side) sit outside the content width.
	w := m.width - 6
	if w < minContentWidth {
		w = minContentWidth
	}
	if w > maxContentWidth {
		w = maxContentWidth
	}
	m.contentWidth = w

	if len(m.tableCols) > 0 {
		components.SetTable(&m.table, m.tableCols, w)
	}
	if len(m.releaseTableCols) > 0 {
		components.SetTable(&m.releaseTable, m.releaseTableCols, w)
	}

	listHeight := m.height - chromeHeight - lipgloss.Height(m.renderTabBar())
	if listHeight < minListHeight {
		listHeight = minListHeight
	}
	if listHeight > maxListHeight {
		listHeight = maxListHeight
	}
	m.list.SetSize(w, listHeight)
	m.releaseList.SetSize(w, listHeight)
}

// startHelmCmd switches to the execute view, shows a spinner with the given
// label, and runs fn on a background goroutine (via tea.Cmd) so the UI never
// blocks on a helm CLI call. The result comes back as a helmResultMsg.
// startHelmCmd switches to the execute view and runs fn in the background.
// command is the exact "helm ..." line fn runs (see helm.MultilineCommandString),
// shown alongside the result once it completes.
func (m *Model) startHelmCmd(label, command string, fn func() (string, error)) tea.Cmd {
	m.state = stateExecute
	m.loading = true
	m.loadingLabel = label
	m.lastCommand = command
	return tea.Batch(helmCmd(fn), m.spinner.Tick)
}

// confirmUpgrade and confirmInstall put the model into stateConfirmAction
// showing the exact helm command that would run, so upgrade/install always
// require an explicit yes before touching the cluster. cancelState is
// where "n"/esc returns to. "d" runs the same command with --dry-run
// instead, without leaving the confirmation's safety net.
func (m *Model) confirmUpgrade(profile config.ReleaseProfile, cancelState sessionState) {
	cmdStr := helm.MultilineCommandString(helm.UpgradeArgs(profile, false))
	dryCmdStr := helm.MultilineCommandString(helm.UpgradeArgs(profile, true))
	m.confirmMsg = fmt.Sprintf("Run this command?\n\n%s", cmdStr)
	m.confirmCancelState = cancelState
	m.confirmAction = func() tea.Cmd {
		return m.startHelmCmd(fmt.Sprintf("Upgrading %s...", profile.ReleaseName), cmdStr, func() (string, error) {
			return m.helmClient.Upgrade(profile, false)
		})
	}
	m.confirmDryRunAction = func() tea.Cmd {
		return m.startHelmCmd(fmt.Sprintf("Dry-run upgrading %s...", profile.ReleaseName), dryCmdStr, func() (string, error) {
			return m.helmClient.Upgrade(profile, true)
		})
	}
	m.state = stateConfirmAction
}

func (m *Model) confirmInstall(profile config.ReleaseProfile, cancelState sessionState) {
	cmdStr := helm.MultilineCommandString(helm.InstallArgs(profile, false))
	dryCmdStr := helm.MultilineCommandString(helm.InstallArgs(profile, true))
	m.confirmMsg = fmt.Sprintf("Run this command?\n\n%s", cmdStr)
	m.confirmCancelState = cancelState
	m.confirmAction = func() tea.Cmd {
		return m.startHelmCmd(fmt.Sprintf("Installing %s...", profile.ReleaseName), cmdStr, func() (string, error) {
			return m.helmClient.Install(profile, false)
		})
	}
	m.confirmDryRunAction = func() tea.Cmd {
		return m.startHelmCmd(fmt.Sprintf("Dry-run installing %s...", profile.ReleaseName), dryCmdStr, func() (string, error) {
			return m.helmClient.Install(profile, true)
		})
	}
	m.state = stateConfirmAction
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		if m.fixedHeight <= 0 {
			m.height = msg.Height
		}
		m.applyWindowSize()
		return m, nil
	case helmResultMsg:
		m.loading = false
		if msg.err != nil {
			m.output = fmt.Sprintf("Error: %v\n\n%s", msg.err, msg.output)
		} else {
			m.output = msg.output
		}
		m.state = stateExecute
		return m, nil
	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var spinCmd tea.Cmd
		m.spinner, spinCmd = m.spinner.Update(msg)
		return m, spinCmd
	case tea.KeyMsg:
		// Handle global keys first. Plain "q" only quits outside of any
		// text input - otherwise typing a release/namespace name (or
		// search query) containing "q" would quit the app.
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if !m.isTyping() {
				return m, tea.Quit
			}
		}

		if (msg.String() == "tab" || msg.String() == "shift+tab") && m.canSwitchTab() {
			m.switchTab()
			return m, nil
		}

		if msg.String() == "s" && !m.searching && !m.isTyping() && (m.state == stateList || m.state == stateReleaseList) {
			m.searching = true
			m.searchInput = textinput.New()
			m.searchInput.Placeholder = "search release/namespace"
			m.searchInput.SetValue(m.searchQuery)
			m.searchInput.CursorEnd()
			m.searchInput.Focus()
			return m, textinput.Blink
		}

		if m.searching {
			if msg.String() == "up" || msg.String() == "down" {
				// Let the arrow keys move the selection in the filtered
				// list without leaving the search box, so results can be
				// browsed while still typing/refining the query.
				var cmd tea.Cmd
				if m.state == stateReleaseList {
					m.releaseList, cmd = m.releaseList.Update(msg)
				} else {
					m.list, cmd = m.list.Update(msg)
				}
				return m, cmd
			}
			if msg.String() == "esc" {
				m.searching = false
				m.searchQuery = ""
				m.refreshLists()
				return m, nil
			}
			if msg.String() == "enter" {
				m.searching = false
				return m, nil
			}
			var cmd tea.Cmd
			m.searchInput, cmd = m.searchInput.Update(msg)
			m.searchQuery = m.searchInput.Value()
			m.refreshLists()
			return m, cmd
		}

		if m.state == stateReleaseList {
			// Handle quick-edit (Chart Version / App Version) first, same
			// shape as stateList's inline editing below.
			if m.isEditing {
				if msg.String() == "esc" {
					m.isEditing = false
					return m, nil
				}
				if msg.String() == "enter" {
					item, ok := m.releaseList.SelectedItem().(releaseRowItem)
					if !ok {
						m.isEditing = false
						return m, nil
					}
					p := item.profile

					switch m.editingField {
					case quickEditChartVer:
						p.Version = m.editingInput.Value()
					case quickEditAppVersion:
						p.RemoteValues = applyAppVersion(p.RemoteValues, m.editingInput.Value())
					}

					releases := m.config.Contexts[item.context]
					for i, r := range releases {
						if r.ReleaseName == item.profile.ReleaseName {
							m.config.Contexts[item.context][i] = p
							break
						}
					}
					if err := m.config.Save(); err != nil {
						m.err = err
					} else {
						m.refreshLists()
					}
					m.isEditing = false
					return m, nil
				}
				if fieldNavDelta(msg) != 0 {
					m.editingField = (m.editingField + 1) % 2
					if item, ok := m.releaseList.SelectedItem().(releaseRowItem); ok {
						var val string
						switch m.editingField {
						case quickEditChartVer:
							val = item.profile.Version
						case quickEditAppVersion:
							val = extractAppVersion(item.profile.RemoteValues)
						}
						m.editingInput.SetValue(val)
					}
					return m, nil
				}
				var editCmd tea.Cmd
				m.editingInput, editCmd = m.editingInput.Update(msg)
				return m, editCmd
			}

			var listCmd tea.Cmd
			m.releaseList, listCmd = m.releaseList.Update(msg)

			if msg.String() == "esc" && m.searchQuery != "" {
				m.searchQuery = ""
				m.refreshLists()
				return m, nil
			}
			if msg.String() == "enter" {
				item, ok := m.releaseList.SelectedItem().(releaseRowItem)
				if !ok {
					return m, listCmd
				}
				m.setContext(item.context)

				for i := range m.config.Contexts[m.currentContext] {
					if m.config.Contexts[m.currentContext][i].ReleaseName == item.profile.ReleaseName {
						m.config.Contexts[m.currentContext][i].LastSelected = time.Now().Unix()
						break
					}
				}
				if err := m.config.Save(); err != nil {
					m.err = err
				}

				m.state = stateMenu
				m.selected = item.profile
				return m, nil
			}
			if msg.String() == "e" {
				item, ok := m.releaseList.SelectedItem().(releaseRowItem)
				if !ok {
					return m, listCmd
				}
				m.isEditing = true
				m.editingField = quickEditAppVersion // same default as the Context tab's quick edit
				m.editingInput = textinput.New()
				m.editingInput.SetValue(extractAppVersion(item.profile.RemoteValues))
				m.editingInput.Focus()
				return m, nil
			}
			if msg.String() == "u" {
				item, ok := m.releaseList.SelectedItem().(releaseRowItem)
				if !ok {
					return m, listCmd
				}
				m.setContext(item.context)
				m.selected = item.profile
				m.confirmUpgrade(m.selected, stateReleaseList)
				return m, nil
			}
			if msg.String() == "a" {
				m.setupAddProfileForm()
				m.state = stateAddProfile
				return m, m.inputs[m.focus].Focus()
			}
			return m, listCmd
		}

		if m.state == stateList {
			// Handle inline editing first
			if m.isEditing {
				if msg.String() == "esc" {
					m.isEditing = false
					return m, nil
				}
				if msg.String() == "enter" {
					// Save the value to the profile
					selectedItem := m.list.SelectedItem().(releaseItem)
					p := selectedItem.profile

					switch m.editingField {
					case quickEditChartVer:
						p.Version = m.editingInput.Value()
					case quickEditAppVersion:
						p.RemoteValues = applyAppVersion(p.RemoteValues, m.editingInput.Value())
					}

					releases := m.config.Contexts[m.currentContext]
					for i, r := range releases {
						if r.ReleaseName == selectedItem.profile.ReleaseName {
							m.config.Contexts[m.currentContext][i] = p
							break
						}
					}
					if err := m.config.Save(); err != nil {
						m.err = err
					} else {
						m.refreshLists()
					}
					m.isEditing = false
					return m, nil
				}
				if fieldNavDelta(msg) != 0 {
					m.editingField = (m.editingField + 1) % 2
					selectedItem := m.list.SelectedItem().(releaseItem)
					p := selectedItem.profile

					var val string
					switch m.editingField {
					case quickEditChartVer:
						val = p.Version
					case quickEditAppVersion:
						val = extractAppVersion(p.RemoteValues)
					}
					m.editingInput.SetValue(val)
					return m, nil
				}

				// Pass character input to textinput
				var cmd tea.Cmd
				m.editingInput, cmd = m.editingInput.Update(msg)
				return m, cmd
			}

			// List state updates (like pagination) must be called
			var listCmd tea.Cmd
			m.list, listCmd = m.list.Update(msg)

			if msg.String() == "esc" && m.searchQuery != "" {
				m.searchQuery = ""
				m.refreshLists()
				return m, nil
			}
			if msg.String() == "enter" {
				releases := m.config.GetReleasesForContext(m.currentContext)
				if len(releases) == 0 {
					return m, nil
				}

				selectedItem := m.list.SelectedItem().(releaseItem)

				for i := range m.config.Contexts[m.currentContext] {
					if m.config.Contexts[m.currentContext][i].ReleaseName == selectedItem.profile.ReleaseName {
						m.config.Contexts[m.currentContext][i].LastSelected = time.Now().Unix()
						break
					}
				}
				if err := m.config.Save(); err != nil {
					m.err = err
				}

				m.state = stateMenu
				m.selected = selectedItem.profile
				return m, nil
			}
			if msg.String() == "[" {
				m.switchContext(-1)
				return m, nil
			}
			if msg.String() == "]" {
				m.switchContext(1)
				return m, nil
			}
			if msg.String() == "a" {
				m.setupAddProfileForm()
				m.state = stateAddProfile
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "e" {
				selectedItem := m.list.SelectedItem().(releaseItem)
				m.isEditing = true
				m.editingField = quickEditAppVersion

				m.editingInput = textinput.New()
				m.editingInput.SetValue(extractAppVersion(selectedItem.profile.RemoteValues))
				m.editingInput.Focus() // Ensure the input is focused

				return m, nil
			}
			if msg.String() == "u" {
				releases := m.config.GetReleasesForContext(m.currentContext)
				if len(releases) == 0 {
					return m, nil
				}
				selectedItem := m.list.SelectedItem().(releaseItem)
				m.selected = selectedItem.profile
				m.confirmUpgrade(m.selected, stateList)
				return m, nil
			}
			return m, listCmd
		} else if m.state == stateMenu {
			if msg.String() == "up" || msg.String() == "down" {
				cmd := m.navigateSelection(msg)
				return m, cmd
			}
			if msg.String() == "[" && m.activeTab == contextTab {
				m.switchMenuContext(-1)
				return m, nil
			}
			if msg.String() == "]" && m.activeTab == contextTab {
				m.switchMenuContext(1)
				return m, nil
			}
			if msg.String() == "esc" {
				m.state = m.backState()
				return m, nil
			}
			if msg.String() == "e" {
				m.setupEditProfileForm()
				m.state = stateEditProfile
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "x" {
				m.confirmMsg = fmt.Sprintf("Are you sure you want to delete profile %s?", m.selected.ReleaseName)
				m.confirmCancelState = stateMenu
				m.confirmDryRunAction = nil
				m.confirmAction = func() tea.Cmd {
					// Delete profile logic
					releases := m.config.Contexts[m.currentContext]
					for i, r := range releases {
						if r.ReleaseName == m.selected.ReleaseName {
							m.config.Contexts[m.currentContext] = append(releases[:i], releases[i+1:]...)
							break
						}
					}
					if err := m.config.Save(); err != nil {
						m.err = err
					}
					m.refreshLists()
					m.state = m.backState()
					return nil
				}
				m.state = stateConfirmAction
				return m, nil
			}

			profile := m.selected

			switch msg.String() {
			case "h":
				cmdStr := helm.MultilineCommandString(helm.HistoryArgs(profile))
				return m, m.startHelmCmd(fmt.Sprintf("Fetching history for %s...", profile.ReleaseName), cmdStr, func() (string, error) {
					return m.helmClient.History(profile)
				})
			case "u":
				m.confirmUpgrade(profile, stateMenu)
				return m, nil
			case "i":
				m.confirmInstall(profile, stateMenu)
				return m, nil
			case "r":
				m.rollbackInput = textinput.New()
				m.rollbackInput.Placeholder = "Revision (Enter for previous)"
				m.rollbackInput.Focus()
				m.state = stateRollbackInput
				return m, nil
			case "d":
				deleteCmdStr := helm.MultilineCommandString(helm.DeleteArgs(profile))
				m.confirmMsg = fmt.Sprintf("Are you sure you want to delete release %s?\n\n%s", profile.ReleaseName, deleteCmdStr)
				m.confirmCancelState = stateMenu
				m.confirmDryRunAction = nil
				m.confirmAction = func() tea.Cmd {
					return m.startHelmCmd(fmt.Sprintf("Deleting %s...", profile.ReleaseName), deleteCmdStr, func() (string, error) {
						return m.helmClient.Delete(profile)
					})
				}
				m.state = stateConfirmAction
				return m, nil
			default:
				return m, nil
			}
		} else if m.state == stateExecute {
			if m.loading {
				return m, nil
			}
			if msg.String() == "esc" {
				m.state = stateMenu
				return m, nil
			}
		} else if m.state == stateConfirmAction {
			if msg.String() == "esc" || msg.String() == "n" {
				m.state = m.confirmCancelState
				return m, nil
			}
			if msg.String() == "y" || msg.String() == "enter" {
				cmd := m.confirmAction()
				return m, cmd
			}
			if msg.String() == "d" && m.confirmDryRunAction != nil {
				cmd := m.confirmDryRunAction()
				return m, cmd
			}
		} else if m.state == stateRollbackInput {
			if msg.String() == "esc" {
				m.state = stateMenu
				return m, nil
			}
			if msg.String() == "enter" {
				rev := 0
				if m.rollbackInput.Value() != "" {
					fmt.Sscanf(m.rollbackInput.Value(), "%d", &rev)
				}
				profile := m.selected
				cmdStr := helm.MultilineCommandString(helm.RollbackArgs(profile, rev))
				return m, m.startHelmCmd(fmt.Sprintf("Rolling back %s...", profile.ReleaseName), cmdStr, func() (string, error) {
					return m.helmClient.Rollback(profile, rev)
				})
			}
			var cmd tea.Cmd
			m.rollbackInput, cmd = m.rollbackInput.Update(msg)
			return m, cmd
		} else if m.state == stateInlineEdit {
			if msg.String() == "esc" {
				m.state = m.backState()
				return m, nil
			}
			if msg.String() == "enter" {
				p := m.selected
				p.Version = m.inputs[0].Value()
				p.RemoteValues = m.inputs[1].Value()

				releases := m.config.Contexts[m.currentContext]
				for i, r := range releases {
					if r.ReleaseName == m.selected.ReleaseName {
						m.config.Contexts[m.currentContext][i] = p
						break
					}
				}

				if err := m.config.Save(); err != nil {
					m.err = err
				} else {
					m.refreshLists()
					m.state = m.backState()
				}
				return m, nil
			}
			if delta := fieldNavDelta(msg); delta != 0 {
				m.focus = (m.focus + delta + len(m.inputs)) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			}
		} else if m.state == stateEditProfile {
			if msg.String() == "esc" {
				m.state = stateMenu
				return m, nil
			}
			if msg.String() == "enter" {
				if m.focus == len(m.inputs)-1 {
					p := config.ReleaseProfile{
						Namespace:    m.inputs[0].Value(),
						ReleaseName:  m.inputs[1].Value(),
						Chart:        m.inputs[2].Value(),
						Version:      m.inputs[3].Value(),
						RemoteValues: m.inputs[4].Value(),
					}

					releases := m.config.Contexts[m.currentContext]
					for i, r := range releases {
						if r.ReleaseName == m.selected.ReleaseName {
							m.config.Contexts[m.currentContext][i] = p
							break
						}
					}

					if err := m.config.Save(); err != nil {
						m.err = err
					} else {
						m.refreshLists()
						m.selected = p
						m.state = stateMenu
					}
					return m, nil
				}
				m.focus++
				return m, m.inputs[m.focus].Focus()
			}
			if delta := fieldNavDelta(msg); delta != 0 {
				m.focus = (m.focus + delta + len(m.inputs)) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			}
		} else if m.state == stateAddProfile {
			if msg.String() == "esc" {
				m.state = m.backState()
				return m, nil
			}
			if msg.String() == "enter" {
				if m.focus == len(m.inputs)-1 {
					p := config.ReleaseProfile{
						Namespace:    m.inputs[0].Value(),
						ReleaseName:  m.inputs[1].Value(),
						Chart:        m.inputs[2].Value(),
						Version:      m.inputs[3].Value(),
						RemoteValues: m.inputs[4].Value(),
					}
					targetContext := m.inputs[addProfileContextField].Value()
					m.config.AddRelease(targetContext, p)
					if err := m.config.Save(); err != nil {
						m.err = err
					} else {
						m.refreshLists()
						m.state = m.backState()
					}
					return m, nil
				}
				m.focus++
				return m, m.inputs[m.focus].Focus()
			}
			if delta := fieldNavDelta(msg); delta != 0 {
				m.focus = (m.focus + delta + len(m.inputs)) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			}
		}
	}

	if (m.state == stateAddProfile || m.state == stateEditProfile || m.state == stateInlineEdit) && len(m.inputs) > 0 && m.focus < len(m.inputs) {
		m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
		return m, cmd
	}

	return m, nil
}

// addProfileContextField is the index of the Context field in the add
// profile form. It's last so the other fields (shared with edit profile)
// keep the same indices, and it's editable - unlike edit profile, which
// updates an existing entry in place, add profile has no "current context"
// to assume from the Release tab's flat, all-contexts list - so it's
// prefilled with m.currentContext but can be changed to any context.
const addProfileContextField = 5

func (m *Model) setupAddProfileForm() {
	fields := []string{"Namespace", "Release Name", "Chart", "Version", "Remote Values URL", "Context"}
	m.inputs = make([]textinput.Model, len(fields))

	for i := 0; i < len(fields); i++ {
		t := textinput.New()
		t.Placeholder = fields[i]
		m.inputs[i] = t
	}
	m.inputs[addProfileContextField].SetValue(m.currentContext)
	m.focus = 0
}

func (m *Model) setupEditProfileForm() {
	fields := []string{"Namespace", "Release Name", "Chart", "Version", "Remote Values URL"}
	m.inputs = make([]textinput.Model, len(fields))

	values := []string{
		m.selected.Namespace,
		m.selected.ReleaseName,
		m.selected.Chart,
		m.selected.Version,
		m.selected.RemoteValues,
	}

	for i := 0; i < len(fields); i++ {
		t := textinput.New()
		t.Placeholder = fields[i]
		t.SetValue(values[i])
		m.inputs[i] = t
	}
	m.focus = 0
}

// renderPanel renders a table header followed by a bordered list body
// beneath it, the shared look used for every top-level browsing list
// (Context tab's releases, Release tab's names and per-release contexts).
func (m *Model) renderPanel(t table.Model, l list.Model, title string) string {
	s := components.RenderTable(t, 15, m.contentWidth, title) + "\n"

	borderStyle := lipgloss.NewStyle().
		Border(styles.Border, false, true, true).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(m.contentWidth)

	return s + borderStyle.Render(l.View())
}

// renderReleasesTable renders the releases table (its title already shows
// the current context). It is shown in every state so the table stays
// visible while the user navigates the release menu, executes commands, or
// fills out forms.
func (m *Model) renderReleasesTable() string {
	return m.renderPanel(m.table, m.list, fmt.Sprintf(" Releases [%s]", m.currentContext))
}

// renderReleaseTable renders the Release tab's flat list: every (release,
// context) pair across every context, grouped visually by release.
func (m *Model) renderReleaseTable() string {
	return m.renderPanel(m.releaseTable, m.releaseList, " Releases (all contexts) ")
}

// renderActiveTable renders whichever tab's table is active, so selecting
// a release from the Release tab (which enters the shared stateMenu/etc.
// flow below) keeps showing the Release tab's table instead of switching
// to the Context tab's.
func (m *Model) renderActiveTable() string {
	if m.activeTab == releaseTab {
		return m.renderReleaseTable()
	}
	return m.renderReleasesTable()
}

// renderTabBar renders the Context/Release tab bar shown at the top of
// every view.
func (m *Model) renderTabBar() string {
	labels := []string{"Context", "Release"}
	rendered := make([]string, len(labels))
	for i, label := range labels {
		style := styles.InactiveTabStyle
		if tabKind(i) == m.activeTab {
			style = styles.ActiveTabStyle
		}
		rendered[i] = style.Render(label)
	}
	left := lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
	right := m.renderSearchBox()

	gap := m.contentWidth - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Repeat(" ", gap), right)
}

// renderSearchBox renders the search indicator shown at the right end of
// the tab bar: a hint while idle, the live input while typing, or the
// active query (with a reminder that esc clears it) once confirmed.
func (m *Model) renderSearchBox() string {
	switch {
	case m.searching:
		return hintStyle.Render("search: ") + m.searchInput.View()
	case m.searchQuery != "":
		return hintStyle.Render("search: ") + keyStyle.Render(m.searchQuery) + hintStyle.Render("  (esc clear)")
	default:
		return hintLine(keyHint{"s", "search"})
	}
}

func (m *Model) View() string {
	doc := m.renderTabBar() + "\n\n"

	switch m.state {
	case stateReleaseList:
		doc += m.renderReleaseTable() + "\n\n"
		if m.err != nil {
			doc += errorStyle.Render(fmt.Sprintf("  Error: %v", m.err)) + "\n\n"
		}
		if len(m.releaseList.Items()) == 0 {
			doc += "  " + hintLine(keyHint{"a", "add a new release profile"}, keyHint{"tab", "switch tab"})
		} else if m.isEditing {
			doc += "  " + hintStyle.Render("Editing... ") + hintLine(
				keyHint{"tab", "switch field"},
				keyHint{"enter", "save"},
				keyHint{"esc", "cancel"},
			)
		} else {
			doc += "  " + hintLine(
				keyHint{"a", "add profile"},
				keyHint{"enter", "select"},
				keyHint{"e", "edit"},
				keyHint{"u", "upgrade"},
				keyHint{"tab", "switch tab"},
				keyHint{"ctrl+c", "quit"},
			)
		}
		return docStyle.Render(doc)
	}

	s := doc + m.renderActiveTable() + "\n\n"

	switch m.state {
	case stateList:
		if m.err != nil {
			s += errorStyle.Render(fmt.Sprintf("  Error: %v", m.err)) + "\n\n"
		}
		releases := m.config.GetReleasesForContext(m.currentContext)
		if len(releases) == 0 {
			s += "  " + hintLine(keyHint{"a", "add a new release profile"}, keyHint{"tab", "switch tab"})
		} else if m.isEditing {
			s += "  " + hintStyle.Render("Editing... ") + hintLine(
				keyHint{"tab", "switch field"},
				keyHint{"enter", "save"},
				keyHint{"esc", "cancel"},
			)
		} else {
			s += "  " + hintLine(
				keyHint{"a", "add profile"},
				keyHint{"e", "edit"},
				keyHint{"u", "upgrade"},
				keyHint{"enter", "select"},
				keyHint{"[ ]", "switch context"},
				keyHint{"tab", "switch tab"},
				keyHint{"ctrl+c", "quit"},
			)
		}
	case stateInlineEdit:
		s += fmt.Sprintf("Quick Edit: %s\n\n", m.selected.ReleaseName)
		if len(m.inputs) == 0 {
			s += "Error: No inputs initialized"
		} else {
			for i, input := range m.inputs {
				label := "Chart Version"
				if i == 1 {
					label = "App Version (Remote Values URL)"
				}
				cursor := "  "
				if i == m.focus {
					cursor = "> "
				}
				s += fmt.Sprintf("%s %s: %s\n", cursor, label, input.View())
			}
		}
		s += "\n" + hintLine(keyHint{"tab", "switch field"}, keyHint{"enter", "save"}, keyHint{"esc", "cancel"})
	case stateMenu:
		s += fmt.Sprintf("Selected: %s\n\n", m.selected.ReleaseName)
		s += hintLines(
			keyHint{"h", "History"},
			keyHint{"u", "Upgrade"},
			keyHint{"i", "Install"},
			keyHint{"r", "Rollback"},
			keyHint{"d", "Delete"},
			keyHint{"e", "Edit Profile"},
			keyHint{"x", "Delete Profile"},
		)
		menuHints := []keyHint{{"↑↓", "switch release"}}
		if m.activeTab == contextTab {
			menuHints = append(menuHints, keyHint{"[ ]", "switch context"})
		}
		menuHints = append(menuHints, keyHint{"tab", "switch tab"}, keyHint{"esc", "back"})
		s += "\n\n" + hintLine(menuHints...)
	case stateConfirmAction:
		s += fmt.Sprintf("Confirmation\n\n%s\n\n", m.confirmMsg)
		if m.confirmDryRunAction != nil {
			s += hintLine(keyHint{"y", "yes"}, keyHint{"d", "dry run"}, keyHint{"n", "no"}, keyHint{"esc", "cancel"})
		} else {
			s += hintLine(keyHint{"y", "yes"}, keyHint{"n", "no"}, keyHint{"esc", "cancel"})
		}
	case stateRollbackInput:
		s += fmt.Sprintf("Rollback %s\n\n%s\n\n", m.selected.ReleaseName, m.rollbackInput.View())
		s += hintLine(keyHint{"enter", "execute"}, keyHint{"esc", "cancel"})
	case stateAddProfile:
		s += "Add New Release Profile\n\n"
		for i, input := range m.inputs {
			cursor := "  "
			if i == m.focus {
				cursor = "> "
			}
			s += fmt.Sprintf("%s %s\n", cursor, input.View())
		}
		s += "\n" + hintLine(keyHint{"tab", "next field"}, keyHint{"enter", "save"}, keyHint{"esc", "cancel"})
	case stateEditProfile:
		s += fmt.Sprintf("Edit Release Profile [%s]\n\n", m.selected.ReleaseName)
		for i, input := range m.inputs {
			cursor := "  "
			if i == m.focus {
				cursor = "> "
			}
			s += fmt.Sprintf("%s %s\n", cursor, input.View())
		}
		s += "\n" + hintLine(keyHint{"tab", "next field"}, keyHint{"enter", "save"}, keyHint{"esc", "cancel"})
	case stateExecute:
		if m.loading {
			s += fmt.Sprintf("%s %s", m.spinner.View(), m.loadingLabel)
		} else {
			s += fmt.Sprintf("Execution Result for %s:\n\n", m.selected.ReleaseName)
			if m.lastCommand != "" {
				s += m.lastCommand + "\n\n"
			}
			s += m.output + "\n\n"
			s += hintLine(keyHint{"esc", "return to menu"})
		}
	default:
		s += fmt.Sprintf("Unknown state: %d", m.state)
	}
	return docStyle.Render(s)
}