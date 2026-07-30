package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/deukyun/helm-tui/internal/tui"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: helm tui [flags]\n\n"+
			"A terminal UI for managing Helm releases from saved profiles across kube contexts.\n\n"+
			"Flags:\n")
		flag.PrintDefaults()
	}
	height := flag.Int("height", 0, "number of release rows to show (1 row = 1 release), clamped to 10-50, instead of following the terminal size; saved as the default for future runs")
	width := flag.Int("width", 0, "table/list content width in columns, clamped to 60-120, instead of following the terminal size; saved as the default for future runs")
	flag.Parse()

	m, err := tui.NewModel(*height, *width)
	if err != nil {
		fmt.Printf("Error initializing model: %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas! There was an error running the program: %v\n", err)
		os.Exit(1)
	}
}