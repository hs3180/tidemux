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

func routingCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux routing <show|set> [options]")
	}
	action := args[0]
	flags := flag.NewFlagSet("routing "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	shared := flags.String("shared-model-strategy", "", "shared bare-model strategy: off or random")
	billing := flags.String("billing-exhaustion-failover", "", "enable or disable cross-provider failover on insufficient balance")
	if err := flags.Parse(interspersedRoutingArgs(args[1:])); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		return errors.New("usage: tidemux routing <show|set> [--shared-model-strategy off|random] [--billing-exhaustion-failover true|false] [--config PATH]")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	settings := c.EffectiveRouting()
	switch action {
	case "show":
		if flagWasSet(flags, "shared-model-strategy") || flagWasSet(flags, "billing-exhaustion-failover") {
			return errors.New("routing show accepts only --config")
		}
		strategy := settings.SharedModelStrategy
		if strategy == "" {
			strategy = "off"
		}
		fmt.Fprintf(stdout, "Shared model strategy: %s\n", strategy)
		fmt.Fprintf(stdout, "Billing exhaustion failover: %t\n", settings.BillingExhaustionFailover)
		return nil
	case "set":
		sharedSet := flagWasSet(flags, "shared-model-strategy")
		billingSet := flagWasSet(flags, "billing-exhaustion-failover")
		if !sharedSet && !billingSet {
			return errors.New("routing set requires --shared-model-strategy and/or --billing-exhaustion-failover")
		}
		if sharedSet {
			switch *shared {
			case "off", "none":
				settings.SharedModelStrategy = ""
			case "random":
				settings.SharedModelStrategy = *shared
			default:
				return errors.New("--shared-model-strategy must be off or random")
			}
		}
		if billingSet {
			switch strings.ToLower(*billing) {
			case "true":
				settings.BillingExhaustionFailover = true
			case "false":
				settings.BillingExhaustionFailover = false
			default:
				return errors.New("--billing-exhaustion-failover must be true or false")
			}
		}
		if settings == (gateway.RoutingConfig{}) {
			c.Routing = nil
		} else {
			c.Routing = &settings
		}
		if err := writeCommandConfig(abs, c, before); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Updated routing settings.")
		return nil
	default:
		return errors.New("usage: tidemux routing <show|set> [options]")
	}
}

func interspersedRoutingArgs(args []string) []string {
	return interspersedFlagsAndPositionals(args, map[string]bool{
		"--config": true, "-config": true,
		"--shared-model-strategy": true, "-shared-model-strategy": true,
		"--billing-exhaustion-failover": true, "-billing-exhaustion-failover": true,
	})
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
