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

	// Inline editing state
	isEditing    bool
	editingField int // 0: ReleaseName, 1: Namespace, 2: Chart, 3: Version, 4: RemoteValues
	editingInput textinput.Model

	// Form for adding profile
	inputs []textinput.Model
	focus  int

	// Confirmation and Input state
	confirmMsg    string
	confirmAction func() tea.Cmd
	rollbackInput textinput.Model

	// For command execution
	helmClient   *helm.Client
	output       string
	loading      bool
	loadingLabel string
	spinner      spinner.Model

	// Window size, applied to the table/list on resize (see applyWindowSize).
	width        int
	height       int
	contentWidth int
	tableCols    []components.ColumnDefinition
}

type releaseItem struct {
	profile config.ReleaseProfile
}

func (i releaseItem) Title() string       { return i.profile.ReleaseName }
func (i releaseItem) Description() string { return i.profile.Namespace }
func (i releaseItem) FilterValue() string { return i.Title() }

func extractAppVersion(url string) string {
	// Matches versions like 4.6.150.2 or 1.0.1.1-AIE-948-7-SNAPSHOT
	re := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+(?:-[a-zA-Z0-9\.\-]+)?)`)
	match := re.FindString(url)
	if match == "" {
		return "N/A"
	}
	return match
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

	rowStyle := normalStyle
	if selected {
		rowStyle = selectedStyle
	}

	// Inline Editing logic
	fields := []string{
		i.profile.ReleaseName,
		i.profile.Namespace,
		i.profile.Chart,
		i.profile.Version,
		appVer,
	}

	for idx := range fields {
		if selected && d.model.isEditing && d.model.editingField == idx {
			fields[idx] = fmt.Sprintf("[%s]", d.model.editingInput.View())
		}
	}

	cursor := "  "
	if selected {
		cursor = "● " // "● "
	}

	// Use the same column widths the header uses (see components.SetTable)
	// so row cells line up under the header cells. The cursor prefix eats
	// into the first column's width, same as the header's first column.
	// Fields are truncated to their column width (same as the table
	// header's own cells) so a long value can't push later columns out of
	// alignment or wrap the row onto an extra line, especially in a
	// narrow terminal.
	cols := d.model.table.Columns()
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
		// Reserve a 1-char gap so a fully-truncated value doesn't run
		// straight into the next column; the last column doesn't need it.
		truncWidth := width
		if idx < len(fields)-1 {
			truncWidth = max(width-1, 0)
		}
		fmt.Fprintf(&b, "%-*s", width, runewidth.Truncate(field, truncWidth, "…"))
	}

	fmt.Fprintf(w, "%s", rowStyle.Render(b.String()))
}

func NewModel() (*Model, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}

	ctx, err := kube.GetCurrentContext()
	if err != nil {
		ctx = "unknown"
	}

	availableContexts, err := kube.ListContexts()
	if err != nil || len(availableContexts) == 0 {
		// kubectl isn't usable (not installed, no kubeconfig, etc.) - fall
		// back to just the one context so '['/']' become a no-op instead
		// of erroring.
		availableContexts = []string{ctx}
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
		width:  120,
		height: 30,
	}

	// Initialize Table
	m.table = components.GenerateTable()
	m.tableCols = []components.ColumnDefinition{
		{Title: "RELEASE", Width: 20},
		{Title: "NAMESPACE", Width: 15},
		{Title: "CHART", FlexFactor: 3},
		{Title: "CHART VER", Width: 12},
		{Title: "APP VERSION", FlexFactor: 2},
	}

	l := list.New(m.sortedListItems(ctx), releaseDelegate{model: m}, 0, 0)
	l.SetShowTitle(false) // the releases table header already shows this
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	m.list = l

	m.applyWindowSize()

	return m, nil
}

// sortedListItems returns the release items for ctx sorted by LastSelected
// (desc) then ReleaseName (asc).
func (m *Model) sortedListItems(ctx string) []list.Item {
	releases := m.config.GetReleasesForContext(ctx)
	sort.Slice(releases, func(i, j int) bool {
		if releases[i].LastSelected != releases[j].LastSelected {
			return releases[i].LastSelected > releases[j].LastSelected
		}
		return releases[i].ReleaseName < releases[j].ReleaseName
	})

	items := make([]list.Item, 0, len(releases))
	for _, r := range releases {
		items = append(items, releaseItem{profile: r})
	}
	return items
}

// switchContext moves the current kubernetes context by delta (+1/-1)
// within availableContexts, applies it via kubectl so it also takes effect
// outside the TUI, and reloads the release list for the new context.
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
	newCtx := m.availableContexts[idx]

	if err := kube.UseContext(newCtx); err != nil {
		m.err = err
		return
	}

	m.err = nil
	m.currentContext = newCtx
	m.list.SetItems(m.sortedListItems(newCtx))
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

	listHeight := m.height - chromeHeight
	if listHeight < minListHeight {
		listHeight = minListHeight
	}
	if listHeight > maxListHeight {
		listHeight = maxListHeight
	}
	m.list.SetSize(w, listHeight)
}

// startHelmCmd switches to the execute view, shows a spinner with the given
// label, and runs fn on a background goroutine (via tea.Cmd) so the UI never
// blocks on a helm CLI call. The result comes back as a helmResultMsg.
func (m *Model) startHelmCmd(label string, fn func() (string, error)) tea.Cmd {
	m.state = stateExecute
	m.loading = true
	m.loadingLabel = label
	return tea.Batch(helmCmd(fn), m.spinner.Tick)
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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
		// Handle global keys first
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
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
					case 0: p.ReleaseName = m.editingInput.Value()
					case 1: p.Namespace = m.editingInput.Value()
					case 2: p.Chart = m.editingInput.Value()
					case 3: p.Version = m.editingInput.Value()
					case 4:
						// Replace the version part in the RemoteValues URL
						currentVal := p.RemoteValues
						newVer := m.editingInput.Value()
						re := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+(?:-[a-zA-Z0-9\.\-]+)?)`)
						p.RemoteValues = re.ReplaceAllString(currentVal, newVer)
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
						var items []list.Item
						updatedReleases := m.config.GetReleasesForContext(m.currentContext)
						for _, r := range updatedReleases {
							items = append(items, releaseItem{profile: r})
						}
						m.list.SetItems(items)
					}
					m.isEditing = false
					return m, nil
				}
				if msg.String() == "tab" {
					m.editingField = (m.editingField + 1) % 5
					selectedItem := m.list.SelectedItem().(releaseItem)
					p := selectedItem.profile

					var val string
					switch m.editingField {
					case 0: val = p.ReleaseName
					case 1: val = p.Namespace
					case 2: val = p.Chart
					case 3: val = p.Version
					case 4: val = extractAppVersion(p.RemoteValues)
					}
					m.editingInput.SetValue(val)
					return m, nil
				}
				if msg.String() == "shift+tab" {
					m.editingField = (m.editingField - 1 + 5) % 5
					selectedItem := m.list.SelectedItem().(releaseItem)
					p := selectedItem.profile

					var val string
					switch m.editingField {
					case 0: val = p.ReleaseName
					case 1: val = p.Namespace
					case 2: val = p.Chart
					case 3: val = p.Version
					case 4: val = extractAppVersion(p.RemoteValues)
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
				m.editingField = 4 // Default to App Version (Remote Values)

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

				profile := m.selected
				return m, m.startHelmCmd(fmt.Sprintf("Upgrading %s...", profile.ReleaseName), func() (string, error) {
					return m.helmClient.Upgrade(profile)
				})
			}
			return m, listCmd
		} else if m.state == stateMenu {
			if msg.String() == "esc" {
				m.state = stateList
				return m, nil
			}
			if msg.String() == "e" {
				m.setupEditProfileForm()
				m.state = stateEditProfile
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "x" {
				m.confirmMsg = fmt.Sprintf("Are you sure you want to delete profile %s?", m.selected.ReleaseName)
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
					// Update list items
					var items []list.Item
					updatedReleases := m.config.GetReleasesForContext(m.currentContext)
					for _, r := range updatedReleases {
						items = append(items, releaseItem{profile: r})
					}
					m.list.SetItems(items)
					m.state = stateList
					return nil
				}
				m.state = stateConfirmAction
				return m, nil
			}

			profile := m.selected

			switch msg.String() {
			case "h":
				return m, m.startHelmCmd(fmt.Sprintf("Fetching history for %s...", profile.ReleaseName), func() (string, error) {
					return m.helmClient.History(profile)
				})
			case "u":
				return m, m.startHelmCmd(fmt.Sprintf("Upgrading %s...", profile.ReleaseName), func() (string, error) {
					return m.helmClient.Upgrade(profile)
				})
			case "i":
				return m, m.startHelmCmd(fmt.Sprintf("Installing %s...", profile.ReleaseName), func() (string, error) {
					return m.helmClient.Install(profile)
				})
			case "r":
				m.rollbackInput = textinput.New()
				m.rollbackInput.Placeholder = "Revision (Enter for previous)"
				m.rollbackInput.Focus()
				m.state = stateRollbackInput
				return m, nil
			case "d":
				m.confirmMsg = fmt.Sprintf("Are you sure you want to delete release %s?", profile.ReleaseName)
				m.confirmAction = func() tea.Cmd {
					return m.startHelmCmd(fmt.Sprintf("Deleting %s...", profile.ReleaseName), func() (string, error) {
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
				m.state = stateMenu
				return m, nil
			}
			if msg.String() == "y" || msg.String() == "enter" {
				cmd := m.confirmAction()
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
				return m, m.startHelmCmd(fmt.Sprintf("Rolling back %s...", profile.ReleaseName), func() (string, error) {
					return m.helmClient.Rollback(profile, rev)
				})
			}
			var cmd tea.Cmd
			m.rollbackInput, cmd = m.rollbackInput.Update(msg)
			return m, cmd
		} else if m.state == stateInlineEdit {
			if msg.String() == "esc" {
				m.state = stateList
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
					var items []list.Item
					updatedReleases := m.config.GetReleasesForContext(m.currentContext)
					for _, r := range updatedReleases {
						items = append(items, releaseItem{profile: r})
					}
					m.list.SetItems(items)
					m.state = stateList
				}
				return m, nil
			}
			if msg.String() == "tab" {
				m.focus = (m.focus + 1) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "shift+tab" { // Simplified representation
				m.focus = (m.focus - 1 + len(m.inputs)) % len(m.inputs)
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
						var items []list.Item
						updatedReleases := m.config.GetReleasesForContext(m.currentContext)
						for _, r := range updatedReleases {
							items = append(items, releaseItem{profile: r})
						}
						m.list.SetItems(items)
						m.selected = p
						m.state = stateMenu
					}
					return m, nil
				}
				m.focus++
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "tab" {
				m.focus = (m.focus + 1) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			}
		} else if m.state == stateAddProfile {
			if msg.String() == "esc" {
				m.state = stateList
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
					m.config.AddRelease(m.currentContext, p)
					if err := m.config.Save(); err != nil {
						m.err = err
					} else {
						var items []list.Item
						releases := m.config.GetReleasesForContext(m.currentContext)
						for _, r := range releases {
							items = append(items, releaseItem{profile: r})
						}
						m.list.SetItems(items)
						m.state = stateList
					}
					return m, nil
				}
				m.focus++
				return m, m.inputs[m.focus].Focus()
			}
			if msg.String() == "tab" {
				m.focus = (m.focus + 1) % len(m.inputs)
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

func (m *Model) setupAddProfileForm() {
	fields := []string{"Namespace", "Release Name", "Chart", "Version", "Remote Values URL"}
	m.inputs = make([]textinput.Model, len(fields))

	for i := 0; i < len(fields); i++ {
		t := textinput.New()
		t.Placeholder = fields[i]
		m.inputs[i] = t
	}
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

// renderReleasesTable renders the releases table (its title already shows
// the current context). It is shown in every state so the table stays
// visible while the user navigates the release menu, executes commands, or
// fills out forms.
func (m *Model) renderReleasesTable() string {
	s := components.RenderTable(m.table, 15, m.contentWidth, fmt.Sprintf(" Releases [%s]", m.currentContext)) + "\n"

	listView := m.list.View()
	borderStyle := lipgloss.NewStyle().
		Border(styles.Border, false, true, true).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(m.contentWidth)

	s += borderStyle.Render(listView)
	return s
}

func (m *Model) View() string {
	s := m.renderReleasesTable() + "\n\n"

	switch m.state {
	case stateList:
		if m.err != nil {
			s += errorStyle.Render(fmt.Sprintf("  Error: %v", m.err)) + "\n\n"
		}
		releases := m.config.GetReleasesForContext(m.currentContext)
		if len(releases) == 0 {
			s += "  " + hintLine(keyHint{"a", "add a new release profile"})
		} else if m.isEditing {
			s += "  " + hintStyle.Render("Editing... ") + hintLine(
				keyHint{"tab", "switch field"},
				keyHint{"enter", "save"},
				keyHint{"esc", "cancel"},
			)
		} else {
			s += "  " + hintLine(
				keyHint{"a", "add profile"},
				keyHint{"e", "edit profile"},
				keyHint{"u", "upgrade"},
				keyHint{"enter", "select release"},
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
		s += "\n\n" + hintLine(keyHint{"esc", "back"})
	case stateConfirmAction:
		s += fmt.Sprintf("Confirmation\n\n%s\n\n", m.confirmMsg)
		s += hintLine(keyHint{"y", "yes"}, keyHint{"n", "no"}, keyHint{"esc", "cancel"})
	case stateRollbackInput:
		s += fmt.Sprintf("Rollback %s\n\n%s\n\n", m.selected.ReleaseName, m.rollbackInput.View())
		s += hintLine(keyHint{"enter", "execute"}, keyHint{"esc", "cancel"})
	case stateAddProfile:
		s += fmt.Sprintf("Add New Release Profile [%s]\n\n", m.currentContext)
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
			s += fmt.Sprintf("Execution Result for %s:\n\n%s\n\n", m.selected.ReleaseName, m.output)
			s += hintLine(keyHint{"esc", "return to menu"})
		}
	default:
		s += fmt.Sprintf("Unknown state: %d", m.state)
	}
	return docStyle.Render(s)
}