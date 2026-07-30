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
	height := flag.Int("height", 0, "fix the UI's height in terminal rows instead of following the terminal size")
	flag.Parse()

	m, err := tui.NewModel(*height)
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