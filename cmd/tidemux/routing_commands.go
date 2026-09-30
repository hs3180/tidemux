package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

func autoChainCommand(args []string, stdout, stderr *os.File) error {
	action := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("auto-chain "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(interspersedFlagsAndPositionals(args, map[string]bool{"--config": true, "-config": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(*configPath) == "" {
		return errors.New("configuration path must not be empty")
	}
	config, path, before, err := loadCommandConfig(*configPath)
	if err != nil {
		return err
	}
	switch action {
	case "show":
		if flags.NArg() != 0 {
			return errors.New("usage: tidemux auto-chain [show] [--config PATH]")
		}
		if len(config.AutoChain) == 0 {
			fmt.Fprintln(stdout, "No auto chain configured.")
			return nil
		}
		for index, entry := range config.AutoChain {
			fmt.Fprintf(stdout, "%d\t%s/%s\n", index+1, entry.Provider, entry.Model)
		}
		return nil
	case "set":
		if flags.NArg() == 0 || flags.NArg()%2 != 0 {
			return errors.New("usage: tidemux auto-chain set [--config PATH] PROVIDER MODEL [PROVIDER MODEL ...]")
		}
		chain := make([]gateway.AutoChainEntry, 0, flags.NArg()/2)
		for index := 0; index < flags.NArg(); index += 2 {
			chain = append(chain, gateway.AutoChainEntry{Provider: flags.Arg(index), Model: flags.Arg(index + 1)})
		}
		config.AutoChain = chain
		if err := writeCommandConfig(path, config, before); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Updated auto chain with %d provider/model entries.\n", len(chain))
		return nil
	case "clear":
		if flags.NArg() != 0 {
			return errors.New("usage: tidemux auto-chain clear [--config PATH]")
		}
		config.AutoChain = nil
		if err := writeCommandConfig(path, config, before); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Cleared auto chain.")
		return nil
	default:
		return errors.New("usage: tidemux auto-chain <show|set|clear> [--config PATH]")
	}
}

func interspersedFlagsAndPositionals(args []string, valueFlags map[string]bool) []string {
	var options, positionals []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		options = append(options, arg)
		name := arg
		if equal := strings.IndexByte(name, '='); equal >= 0 {
			name = name[:equal]
		}
		if expectsValue, ok := valueFlags[name]; ok && expectsValue && !strings.Contains(arg, "=") && index+1 < len(args) {
			index++
			options = append(options, args[index])
		}
	}
	return append(options, positionals...)
}
