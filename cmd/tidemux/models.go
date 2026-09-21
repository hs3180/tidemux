package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/gateway"
)

func modelsCommand(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("provider models", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	jsonOutput := flags.Bool("json", false, "output model IDs as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *configPath == "" {
		return errors.New(providerUsage)
	}
	c, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	resolved, err := c.ResolveCredentials(context.Background(), gateway.MacOSKeychain{})
	if err != nil {
		return err
	}
	resolved, err = gateway.ResolveProviderProtocol(resolved, nil)
	if err != nil {
		return err
	}
	models, err := adapter.DiscoverModels(context.Background(), resolved.Protocol, resolved.BaseURL, resolved.APIKey, resolved.APIVersion, nil)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(models)
	}
	for _, model := range models {
		if model.DisplayName == "" {
			fmt.Fprintln(stdout, model.ID)
		} else {
			fmt.Fprintf(stdout, "%s\t%s\n", model.ID, model.DisplayName)
		}
	}
	return nil
}
