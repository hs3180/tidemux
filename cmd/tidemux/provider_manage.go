package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

func providerManage(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("provider manage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: tidemux provider manage [--config PATH]")
	}
	tty, err := openControlTTY("provider management requires an interactive terminal")
	if err != nil {
		return err
	}
	defer tty.Close()
	return runProviderManage(tty, stdout, stderr, *path)
}

func runProviderManage(tty, stdout, stderr *os.File, configPath string) error {
	for {
		config, err := managerConfig(configPath)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "\nProvider groups (API keys and Keychain details are hidden):")
		names := sortedProviderNames(config.Providers)
		if len(names) == 0 {
			fmt.Fprintln(stdout, "  No provider groups configured.")
		} else {
			for i, name := range names {
				provider := config.Providers[name]
				fmt.Fprintf(stdout, "  %d. %-20s %-9s %d keys  %s\n", i+1, name, provider.Protocol, configuredProviderKeyCount(provider), providerModelScopeLabel(provider.SupportedModels))
			}
		}
		fmt.Fprintln(stdout, "  A. Add provider    Q. Quit")
		fmt.Fprint(stdout, "Select a provider number, A or Q: ")
		choice, err := readTerminalLine(tty)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return errors.New("could not read provider selection")
		}
		choice = strings.TrimSpace(choice)
		switch strings.ToLower(choice) {
		case "q", "quit":
			return nil
		case "a", "add":
			runProviderAction(stdout, func() error {
				return providerAdd([]string{"--config", configPath}, stdout, stderr)
			})
			continue
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr != nil || index < 1 || index > len(names) {
			fmt.Fprintln(stdout, "Choose a listed provider number, A or Q.")
			continue
		}
		if err := runProviderGroupManage(tty, stdout, stderr, configPath, names[index-1]); err != nil {
			return err
		}
	}
}

func runProviderGroupManage(tty, stdout, stderr *os.File, configPath, ref string) error {
	for {
		config, err := managerConfig(configPath)
		if err != nil {
			return err
		}
		provider, ok := config.Providers[ref]
		if !ok {
			return nil
		}
		fmt.Fprintf(stdout, "\nProvider %s (%s, %d keys, %s)\n", ref, provider.Protocol, configuredProviderKeyCount(provider), providerModelScopeLabel(provider.SupportedModels))
		fmt.Fprintln(stdout, "  1. Show settings      2. Edit endpoint/protocol   3. Add key")
		fmt.Fprintln(stdout, "  4. List keys          5. Remove key               6. Edit model scope")
		fmt.Fprintln(stdout, "  7. Validate           8. Remove provider          0. Back")
		fmt.Fprint(stdout, "Choose an action: ")
		choice, err := readTerminalLine(tty)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return errors.New("could not read provider action")
		}
		switch strings.TrimSpace(choice) {
		case "0":
			return nil
		case "1":
			runProviderAction(stdout, func() error {
				return providerShow([]string{ref, "--config", configPath}, stdout, stderr)
			})
		case "2":
			runProviderAction(stdout, func() error {
				args, changed, err := promptProviderUpdateArgs(tty, stdout, ref, configPath, provider)
				if err != nil {
					return err
				}
				if !changed {
					fmt.Fprintln(stdout, "Provider settings unchanged.")
					return nil
				}
				return providerUpdate(args, stdout, stderr)
			})
		case "3":
			runProviderAction(stdout, func() error {
				return providerKeyAdd([]string{ref, "--config", configPath}, stdout, stderr)
			})
		case "4":
			runProviderAction(stdout, func() error {
				return providerKeyList([]string{ref, "--config", configPath}, stdout, stderr)
			})
		case "5":
			runProviderAction(stdout, func() error {
				if err := providerKeyList([]string{ref, "--config", configPath}, stdout, stderr); err != nil {
					return err
				}
				fmt.Fprint(stdout, "Key slot to remove (number; blank cancels): ")
				value, err := readTerminalLine(tty)
				if err != nil {
					return errors.New("could not read key slot")
				}
				value = strings.TrimSpace(value)
				if value == "" {
					return nil
				}
				return providerKeyRemove([]string{ref, value, "--config", configPath}, stdout, stderr)
			})
		case "6":
			runProviderAction(stdout, func() error {
				return providerModels([]string{ref, "--select", "--config", configPath}, stdout, stderr)
			})
		case "7":
			runProviderAction(stdout, func() error {
				return providerValidate([]string{ref, "--config", configPath}, stdout, stderr)
			})
		case "8":
			if err := providerRemove([]string{ref, "--config", configPath}, stdout, stderr); err != nil {
				fmt.Fprintf(stdout, "Provider was not removed: %v\n", err)
			} else {
				return nil
			}
		default:
			fmt.Fprintln(stdout, "Choose an action from 0 to 8.")
		}
	}
}

func promptProviderUpdateArgs(in, out *os.File, ref, configPath string, current gateway.Provider) ([]string, bool, error) {
	fmt.Fprintf(out, "Current endpoint: %s\nCurrent protocol: %s\nModel scope: %s\n", current.BaseURL, current.Protocol, providerModelScopeLabel(current.SupportedModels))
	fmt.Fprintln(out, "Changing the endpoint resets model scope to all models.")
	fmt.Fprint(out, "New endpoint (Enter keeps current): ")
	endpoint, err := readTerminalLine(in)
	if err != nil {
		return nil, false, errors.New("could not read provider endpoint")
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" {
		if err := gateway.ValidateProviderBaseURL(endpoint); err != nil {
			return nil, false, fmt.Errorf("invalid provider endpoint: %w", err)
		}
	}
	protocolPrompt := "Protocol override (openai or anthropic; Enter keeps current): "
	if endpoint != "" {
		protocolPrompt = "Protocol override (openai or anthropic; Enter auto-detects): "
	}
	fmt.Fprint(out, protocolPrompt)
	protocol, err := readTerminalLine(in)
	if err != nil {
		return nil, false, errors.New("could not read provider protocol")
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol != "" && protocol != "openai" && protocol != "anthropic" {
		return nil, false, errors.New("protocol override must be openai or anthropic")
	}
	changed := endpoint != "" || protocol != ""
	args := []string{ref}
	if endpoint != "" {
		args = append(args, "--endpoint", endpoint)
	}
	if protocol != "" {
		args = append(args, "--protocol", protocol)
	}
	args = append(args, "--config", configPath)
	return args, changed, nil
}

func runProviderAction(out *os.File, action func() error) {
	if err := action(); err != nil {
		fmt.Fprintf(out, "Action failed: %v\n", err)
	}
}

func managerConfig(path string) (gateway.Config, error) {
	config, _, _, err := loadCommandConfig(path)
	if err != nil {
		return gateway.Config{}, err
	}
	return migrateLegacyProvider(config, nil)
}

func providerModelScopeLabel(models []string) string {
	if len(models) == 0 {
		return "all models"
	}
	return fmt.Sprintf("%d allowed models", len(models))
}
