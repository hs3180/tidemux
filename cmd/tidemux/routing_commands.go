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
	shared := flags.String("shared-model-strategy", "", "shared bare-model strategy: off, random (24h idle session affinity) or price_priority")
	billing := flags.Bool("billing-exhaustion-failover", false, "allow explicitly mapped insufficient-balance errors to try another provider of the same client protocol")
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

func interspersedRoutingArgs(args []string) []string {
	return interspersedFlagsAndPositionals(args, map[string]bool{"--config": true, "-config": true, "--shared-model-strategy": true, "-shared-model-strategy": true, "--billing-exhaustion-failover": true, "-billing-exhaustion-failover": true})
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
