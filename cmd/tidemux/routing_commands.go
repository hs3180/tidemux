package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

func routingCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux routing <show|set> [options]")
	}
	action := args[0]
	flags := flag.NewFlagSet("routing "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	shared := flags.String("shared-model-strategy", "", "shared bare-model strategy: off, random or price_priority")
	billing := flags.Bool("billing-exhaustion-failover", false, "allow explicitly mapped insufficient-balance errors to try another provider")
	if err := flags.Parse(interspersedRoutingArgs(args[1:])); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		return errors.New("usage: tidemux routing <show|set> [--shared-model-strategy off|random|price_priority] [--billing-exhaustion-failover=true|false] [--config PATH]")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	settings := c.EffectiveRouting()
	switch action {
	case "show":
		if flagWasSet(flags, "shared-model-strategy", "billing-exhaustion-failover") {
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
		if !flagWasSet(flags, "shared-model-strategy", "billing-exhaustion-failover") {
			return errors.New("routing set requires --shared-model-strategy and/or --billing-exhaustion-failover")
		}
		if flagWasSet(flags, "shared-model-strategy") {
			switch *shared {
			case "off", "none":
				settings.SharedModelStrategy = ""
			case "random", "price_priority":
				settings.SharedModelStrategy = *shared
			default:
				return errors.New("--shared-model-strategy must be off, random or price_priority")
			}
		}
		if flagWasSet(flags, "billing-exhaustion-failover") {
			settings.BillingExhaustionFailover = *billing
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

func providerAutoChainCommand(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("provider auto-chain", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	modelsValue := flags.String("models", "", "ordered comma-separated model IDs for model:auto")
	clear := flags.Bool("clear", false, "remove the provider's auto model chain")
	if err := flags.Parse(interspersedAutoChainArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || strings.TrimSpace(*path) == "" {
		return errors.New("usage: tidemux provider auto-chain REF [--models ID[,ID...]] [--clear] [--config PATH]")
	}
	if *clear && flagWasSet(flags, "models") {
		return errors.New("use either --models or --clear")
	}
	ref := flags.Arg(0)
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	provider, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	if *clear {
		provider.AutoModelChain = nil
	} else if flagWasSet(flags, "models") {
		models, parseErr := parseProviderModelList(*modelsValue)
		if parseErr != nil {
			return fmt.Errorf("invalid auto model chain: %w", parseErr)
		}
		if len(models) == 0 {
			return errors.New("--models requires at least one model ID")
		}
		for _, model := range models {
			if strings.EqualFold(model, "auto") {
				return errors.New("auto model chain entries cannot be auto")
			}
		}
		provider.AutoModelChain = models
	}
	if !flagWasSet(flags, "models", "clear") {
		if len(provider.AutoModelChain) == 0 {
			fmt.Fprintf(stdout, "Provider %s has no auto model chain.\n", ref)
			return nil
		}
		fmt.Fprintf(stdout, "Provider %s auto model chain: %s\n", ref, strings.Join(provider.AutoModelChain, " -> "))
		return nil
	}
	c.Providers[ref] = provider
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	if len(provider.AutoModelChain) == 0 {
		fmt.Fprintf(stdout, "Cleared provider %s auto model chain.\n", ref)
	} else {
		fmt.Fprintf(stdout, "Updated provider %s auto model chain: %s\n", ref, strings.Join(provider.AutoModelChain, " -> "))
	}
	return nil
}

func interspersedRoutingArgs(args []string) []string {
	return interspersedFlagsAndPositionals(args, map[string]bool{"--config": true, "-config": true, "--shared-model-strategy": true, "-shared-model-strategy": true, "--billing-exhaustion-failover": true, "-billing-exhaustion-failover": true})
}

func interspersedAutoChainArgs(args []string) []string {
	return interspersedFlagsAndPositionals(args, map[string]bool{"--config": true, "-config": true, "--models": true, "-models": true, "--clear": false, "-clear": false})
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
