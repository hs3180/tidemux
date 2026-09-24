package main

import (
	"os"
	"path/filepath"
)

const defaultListenAddr = "127.0.0.1:4000"

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tidemux.json"
	}
	return filepath.Join(home, "Library", "Application Support", "TideMux", "config.json")
}
