package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/deukyun/helm-tui/internal/tui"
)

func main() {
	m, err := tui.NewModel()
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