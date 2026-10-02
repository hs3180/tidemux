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
	const usage = "usage: tidemux auto-chain <show|set|clear> [--entries REF/MODEL[,REF/MODEL...]] [--config PATH]"
	action := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("auto-chain "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	entries := flags.String("entries", "", "ordered provider/model pairs for the single instance chain; safe failures affect new auto sessions only")
	if err := flags.Parse(interspersedFlagsAndPositionals(args, map[string]bool{"--config": true, "-config": true, "--entries": true, "-entries": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if (action != "set" && flags.NArg() != 0) || strings.TrimSpace(*path) == "" {
		return errors.New(usage)
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	switch action {
	case "show":
		if flagWasSet(flags, "entries") {
			return errors.New("auto-chain show accepts only --config")
		}
		if len(c.AutoChain) == 0 {
			fmt.Fprintln(stdout, "No instance auto chain configured.")
			return nil
		}
		for index, entry := range c.AutoChain {
			fmt.Fprintf(stdout, "%d\t%s/%s\n", index+1, entry.Provider, entry.Model)
		}
		return nil
	case "set":
		var chain []gateway.AutoChainEntry
		if flagWasSet(flags, "entries") {
			if flags.NArg() != 0 || strings.TrimSpace(*entries) == "" {
				return errors.New("auto-chain set requires either --entries or PROVIDER MODEL pairs")
			}
			for _, value := range strings.Split(*entries, ",") {
				provider, model, found := strings.Cut(strings.TrimSpace(value), "/")
				if !found || provider == "" || model == "" {
					return errors.New("each auto-chain entry must use REF/MODEL")
				}
				chain = append(chain, gateway.AutoChainEntry{Provider: provider, Model: model})
			}
		} else {
			if flags.NArg() == 0 || flags.NArg()%2 != 0 {
				return errors.New("usage: tidemux auto-chain set [--config PATH] PROVIDER MODEL [PROVIDER MODEL ...]")
			}
			for index := 0; index < flags.NArg(); index += 2 {
				chain = append(chain, gateway.AutoChainEntry{Provider: flags.Arg(index), Model: flags.Arg(index + 1)})
			}
		}
		c.AutoChain = chain
	case "clear":
		if flagWasSet(flags, "entries") {
			return errors.New("auto-chain clear accepts only --config")
		}
		c.AutoChain = nil
	default:
		return errors.New(usage)
	}
	if err := writeCommandConfig(abs, c, before); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Updated instance auto chain. Restart a running gateway to apply it.")
	return nil
}
