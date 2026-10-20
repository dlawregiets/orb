// Command orb browses OpenRouter models and curates the models available
// through the OpenRouter provider in an OpenCode configuration.
package main

import (
	"flag"
	"log"
	"os"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	tea "github.com/charmbracelet/bubbletea"

	"orb/internal/opencode"
	"orb/internal/tui"
)

func main() {
	configPath := flag.String("config", "", "OpenCode config file to edit (default: global opencode.json)")
	flag.Parse()

	path, err := opencode.ResolvePath(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := opencode.Load(path)
	if err != nil {
		log.Fatal(err)
	}

	apiKey := os.Getenv("OPENROUTER_API_KEY")

	var client *openrouter.OpenRouter
	if apiKey != "" {
		client = openrouter.New(openrouter.WithSecurity(apiKey))
	} else {
		client = openrouter.New()
	}

	p := tea.NewProgram(tui.New(client, cfg, apiKey != ""), tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
