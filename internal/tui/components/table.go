package components

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/deukyun/helm-tui/internal/tui/styles"
)

type ColumnDefinition struct {
	Title      string
	Width      int
	FlexFactor int
}

func SetTable(t *table.Model, cols []ColumnDefinition, targetWidth int) tea.Cmd {
	var columns = make([]table.Column, len(cols))
	targetWidth = targetWidth - 2 // remove the border Width
	remainingWidthAfterFixed := targetWidth
	totalFlex := 0
	for _, col := range cols {
		if col.FlexFactor != 0 {
			totalFlex += col.FlexFactor
		}
	}
	for i, col := range cols {
		if col.Width != 0 {
			columns[i] = table.Column{
				Title: col.Title,
				Width: col.Width,
			}
			remainingWidthAfterFixed = remainingWidthAfterFixed - col.Width
		}
	}
	for i, col := range cols {
		if col.FlexFactor != 0 {
			columns[i] = table.Column{
				Title: col.Title,
				Width: int(remainingWidthAfterFixed * col.FlexFactor / totalFlex),
			}
		}
	}
	// fill last column with the remaning Width due to integer division
	lastCol := columns[len(columns)-1]
	totalColWidth := 0
	for _, col := range columns {
		totalColWidth += col.Width
	}
	lastCol.Width = lastCol.Width + targetWidth - totalColWidth
	columns[len(columns)-1] = lastCol
	t.SetColumns(columns)
	t.SetWidth(targetWidth)
	return nil
}

func GenerateTable() table.Model {
	t := table.New()
	s := table.DefaultStyles()
	k := table.DefaultKeyMap()
	k.HalfPageUp.Unbind()
	k.PageDown.Unbind()
	k.HalfPageDown.Unbind()
	k.HalfPageDown.Unbind()
	k.GotoBottom.Unbind()
	k.GotoTop.Unbind()
	s.Header = s.Header.
		Padding(0, 0).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)

	t.SetStyles(s)
	t.KeyMap = k
	return t
}

// borderColor is the single color used for every border segment drawn by
// the table (top, header divider, and the row box drawn by the caller), so
// the whole thing reads as one continuous box rather than mismatched pieces.
var borderColor = lipgloss.Color("240")

func RenderTable(t table.Model, height int, width int, title string) string {
	t.SetHeight(height)
	t.SetWidth(width)

	view := t.View()

	topBorder := styles.GenerateTopBorderWithTitle(title, width, styles.Border, lipgloss.NewStyle().Foreground(borderColor))

	lines := strings.Split(view, "\n")
	var headerView string
	if len(lines) > 0 {
		headerView = lines[0]
	}

	// Use tee joints instead of rounded corners on the bottom so the header
	// box flows straight into the row box drawn below it (by the caller)
	// instead of looking like two separately closed boxes.
	dividerBorder := styles.Border
	dividerBorder.BottomLeft = styles.Border.MiddleLeft
	dividerBorder.BottomRight = styles.Border.MiddleRight

	headerBoxStyle := lipgloss.NewStyle().
		Border(dividerBorder, false, true, true).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(width)

	return lipgloss.JoinVertical(lipgloss.Left, topBorder, headerBoxStyle.Render(headerView))
}