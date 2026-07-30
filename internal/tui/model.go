package tui

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/deukyun/helm-tui/internal/config"
	"github.com/deukyun/helm-tui/internal/helm"
	"github.com/deukyun/helm-tui/internal/kube"
	"github.com/deukyun/helm-tui/internal/tui/components"
	"github.com/deukyun/helm-tui/internal/tui/styles"
)

var docStyle = lipgloss.NewStyle().Margin(1, 2)
var headerStyle = lipgloss.NewStyle().Foreground(styles.HighlightColor).Bold(true)
var selectedStyle = lipgloss.NewStyle().Foreground(styles.HighlightColor).Bold(true)
var normalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
var cursorStyle = lipgloss.NewStyle().Background(styles.HighlightColor).Foreground(lipgloss.Color("252"))

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

type Model struct {
	state          sessionState
	config         *config.Config
	list           list.Model
	table          table.Model
	cursor         int
	selected       config.ReleaseProfile
	err            error

	currentContext string

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
	helmClient *helm.Client
	output     string
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
		fmt.Fprintf(&b, "%-*s", width, field)
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

	// Create model first so we can pass it to delegate
	m := &Model{
		state:          stateList,
		config:         cfg,
		currentContext: ctx,
		helmClient:     helm.NewClient(),
	}

	// Initialize Table
	m.table = components.GenerateTable()
	cols := []components.ColumnDefinition{
		{Title: "RELEASE", Width: 20},
		{Title: "NAMESPACE", Width: 15},
		{Title: "CHART", FlexFactor: 3},
		{Title: "CHART VER", Width: 12},
		{Title: "APP VERSION", FlexFactor: 2},
	}
	components.SetTable(&m.table, cols, 120)

	var items []list.Item
	releases := cfg.GetReleasesForContext(ctx)

	// Sort: LastSelected (Desc) -> ReleaseName (Asc)
	sort.Slice(releases, func(i, j int) bool {
		if releases[i].LastSelected != releases[j].LastSelected {
			return releases[i].LastSelected > releases[j].LastSelected
		}
		return releases[i].ReleaseName < releases[j].ReleaseName
	})

	for _, r := range releases {
		items = append(items, releaseItem{profile: r})
	}

	// Set height to 10 to show up to 10 items at once
	l := list.New(items, releaseDelegate{model: m}, 0, 10)
	l.Title = fmt.Sprintf("Releases [%s]", ctx)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	m.list = l

	return m, nil
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
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

				var output string
				var err error
				output, err = m.helmClient.Upgrade(m.selected)
				if err != nil {
					m.output = fmt.Sprintf("Error: %v\n\n%s", err, output)
				} else {
					m.output = output
				}
				m.state = stateExecute
				return m, nil
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

			var output string
			var err error

			switch msg.String() {
			case "h":
				output, err = m.helmClient.History(m.selected)
			case "u":
				output, err = m.helmClient.Upgrade(m.selected)
			case "i":
				output, err = m.helmClient.Install(m.selected)
			case "r":
				m.rollbackInput = textinput.New()
				m.rollbackInput.Placeholder = "Revision (Enter for previous)"
				m.rollbackInput.Focus()
				m.state = stateRollbackInput
				return m, nil
			case "d":
				m.confirmMsg = fmt.Sprintf("Are you sure you want to delete release %s?", m.selected.ReleaseName)
				m.confirmAction = func() tea.Cmd {
					var out string
					var e error
					out, e = m.helmClient.Delete(m.selected)
					if e != nil {
						m.output = fmt.Sprintf("Error: %v\n\n%s", e, out)
						m.state = stateExecute
						return nil
					}
					m.output = out
					m.state = stateExecute
					return nil
				}
				m.state = stateConfirmAction
				return m, nil
			default:
				return m, nil
			}

			if err != nil {
				m.output = fmt.Sprintf("Error: %v\n\n%s", err, output)
			} else {
				m.output = output
			}
			m.state = stateExecute
			return m, nil
		} else if m.state == stateExecute {
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
				var output string
				var err error
				output, err = m.helmClient.Rollback(m.selected, rev)
				if err != nil {
					m.output = fmt.Sprintf("Error: %v\n\n%s", err, output)
				} else {
					m.output = output
				}
				m.state = stateExecute
				return m, nil
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

func (m *Model) View() string {
	var s string
	switch m.state {
	case stateList:
		s = headerStyle.Render(fmt.Sprintf("Current Context: %s", m.currentContext)) + "\n\n"

		// Use the new Table component for rendering the list
		s += components.RenderTable(m.table, 15, 120, fmt.Sprintf(" Releases [%s]", m.currentContext)) + "\n"

		listView := m.list.View()
		borderStyle := lipgloss.NewStyle().
			Border(styles.Border, false, true, true).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1).
			Width(120)

		s += borderStyle.Render(listView)

		releases := m.config.GetReleasesForContext(m.currentContext)
		if len(releases) == 0 {
			s += "\n\n  Press 'a' to add a new release profile"
		} else {
			if m.isEditing {
				s += "\n\n  Editing... 'tab' switch field • 'enter' save • 'esc' cancel"
			} else {
				s += "\n\n  'a' add profile • 'e' edit profile • 'u' upgrade • 'enter' select release"
			}
		}
	case stateInlineEdit:
		s = fmt.Sprintf("Quick Edit: %s\n\n", m.selected.ReleaseName)
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
		s += "\nTab: Switch Field | Enter: Save | Esc: Cancel"
	case stateMenu:
		s = fmt.Sprintf("Selected: %s\n\n(h) History\n(u) Upgrade\n(i) Install\n(r) Rollback\n(d) Delete\n(e) Edit Profile\n(x) Delete Profile\n\nEsc: Back", m.selected.ReleaseName)
	case stateConfirmAction:
		s = fmt.Sprintf("Confirmation\n\n%s\n\n(y) Yes | (n) No / Esc: Cancel", m.confirmMsg)
	case stateRollbackInput:
		s = fmt.Sprintf("Rollback %s\n\n%s\n\nEnter: Execute | Esc: Cancel", m.selected.ReleaseName, m.rollbackInput.View())
	case stateAddProfile:
		s = fmt.Sprintf("Add New Release Profile [%s]\n\n", m.currentContext)
		for i, input := range m.inputs {
			cursor := "  "
			if i == m.focus {
				cursor = "> "
			}
			s += fmt.Sprintf("%s %s\n", cursor, input.View())
		}
		s += "\nTab: Next Field | Enter: Save | Esc: Cancel"
	case stateEditProfile:
		s = fmt.Sprintf("Edit Release Profile [%s]\n\n", m.selected.ReleaseName)
		for i, input := range m.inputs {
			cursor := "  "
			if i == m.focus {
				cursor = "> "
			}
			s += fmt.Sprintf("%s %s\n", cursor, input.View())
		}
		s += "\nTab: Next Field | Enter: Save | Esc: Cancel"
	case stateExecute:
		s = fmt.Sprintf("Execution Result for %s:\n\n%s\n\nPress 'esc' to return to menu", m.selected.ReleaseName, m.output)
	default:
		s = fmt.Sprintf("Unknown state: %d", m.state)
	}
	return docStyle.Render(s)
}