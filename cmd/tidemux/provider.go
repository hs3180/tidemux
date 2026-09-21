package main

import (
	"fmt"
	"os"
)

const providerUsage = `usage: tidemux provider <command> [options]

commands:
  configure   create or replace a provider configuration
  models      list models supported by the configured provider

examples:
  tidemux provider configure --preset deepseek-flash
  tidemux provider models
  tidemux provider models --json
`

func providerCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, _ = fmt.Fprint(stdout, providerUsage)
		return nil
	}
	switch args[0] {
	case "configure":
		return configure(args[1:], stdout, stderr)
	case "models":
		return modelsCommand(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown provider command %q\n%s", args[0], providerUsage)
	}
}
