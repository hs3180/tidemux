package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

func providerBudgetResetCommand(args []string, stdout, stderr *os.File) error {
	usage := "usage: tidemux provider budget reset REF --window 5h|7d [--config PATH]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	ref := args[0]
	flags := flag.NewFlagSet("provider budget reset", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	windowValue := flags.String("window", "", "budget window to reset: 5h or 7d")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *configPath == "" || ref != strings.TrimSpace(ref) || !flagWasSet(flags, "window") {
		return errors.New(usage)
	}
	window := ledger.BudgetWindow(*windowValue)
	if window != ledger.BudgetWindowFiveHour && window != ledger.BudgetWindowSevenDay {
		return errors.New("window must be 5h or 7d")
	}
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if _, ok := config.Providers[ref]; !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	if gatewayListening(config.ListenAddr) {
		return errors.New("gateway is still listening; stop it before resetting a budget")
	}
	if _, err := os.Stat(config.LedgerPath); err != nil {
		return errors.New("budget ledger does not exist; start the gateway once to initialize it")
	}
	store, err := ledger.Open(config.LedgerPath)
	if err != nil {
		return errors.New("cannot open budget ledger")
	}
	defer store.Close()
	if err := store.ResetProviderBudget(context.Background(), ref, window, time.Now()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Reset provider %s budget window %s. Restart the gateway before sending new requests.\n", ref, window)
	return err
}

func gatewayListening(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" || port == "0" {
		return false
	}
	ip := net.ParseIP(host)
	if host == "localhost" {
		host = "127.0.0.1"
	} else if ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	} else if ip != nil && !ip.IsLoopback() {
		return false
	} else if ip == nil {
		return false
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
