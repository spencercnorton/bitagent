// Package ui runs the Python operator console / public library (ui/) as a
// child process supervised by the core. Off by default so the estate crawler
// stack stays headless; the public quickstart sets UI_ENABLED=true.
package ui

import "time"

// Config is the "ui" configfx section; env keys UI_ENABLED, UI_LISTEN_ADDRESS,
// UI_DIR, UI_PYTHON, UI_RESTART_BACKOFF. The UI's own settings (OPERATOR_HOSTS,
// REQUIRE_AUTH, ...) are read by the child straight from the environment.
type Config struct {
	Enabled bool `yaml:"enabled"`
	// ListenAddress is host:port for uvicorn inside the container.
	ListenAddress string `yaml:"listen_address"`
	// Dir is where ui/app.py lives (the working directory of the child).
	Dir    string `yaml:"dir"`
	Python string `yaml:"python"`
	// RestartBackoff is the first wait after the child exits; it doubles
	// per consecutive crash up to maxBackoff.
	RestartBackoff time.Duration `yaml:"restart_backoff"`
}

func NewDefaultConfig() Config {
	return Config{
		Enabled:        false,
		ListenAddress:  "0.0.0.0:8080",
		Dir:            "/app/ui",
		Python:         "python3",
		RestartBackoff: 5 * time.Second,
	}
}
