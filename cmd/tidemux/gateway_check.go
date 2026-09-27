package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
)

func gatewayCheck(args []string, stdout, stderr *os.File, lookup gateway.SecretLookup) error {
	flags := flag.NewFlagSet("gateway check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		return errors.New("usage: tidemux gateway check [--config PATH]")
	}
	config, _, _, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if lookup == nil {
		return errors.New("Keychain lookup required")
	}
	token, err := lookup.Lookup(context.Background(), config.AccessTokenKeychain)
	if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("gateway Keychain item unavailable; run `tidemux gateway configure --rotate-key` in Terminal if it is missing")
	}
	host, port, err := net.SplitHostPort(config.ListenAddr)
	if err != nil || port == "" || port == "0" {
		return errors.New("gateway check needs a running listener with a fixed port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("gateway check requires an IP listener address")
	}
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	} else if !ip.IsLoopback() {
		return errors.New("gateway check only contacts a loopback listener")
	}
	address := "http://" + net.JoinHostPort(host, port)
	if err := checkClientEndpoint(context.Background(), address, token); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "gateway: authenticated /v1/models at %s\n", address)
	return nil
}
