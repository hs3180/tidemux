package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

func providerBudgetCommand(args []string, stdin *os.File, stdout, stderr *os.File) error {
	ref, rest := leadingEndpoint(args)
	flags := flag.NewFlagSet("provider budget", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	fiveHour := flags.Float64("budget-5h", 0, "rolling five-hour limit; zero disables this window")
	weekly := flags.Float64("budget-weekly", 0, "rolling seven-day limit; zero disables this window")
	currency := flags.String("budget-currency", "", "budget currency")
	mode := flags.String("budget-mode", "", "budget mode: alert, soft or hard")
	threshold := flags.Float64("budget-alert-threshold", 0, "alert threshold from 0 to 1")
	disable := flags.Bool("disable", false, "disable budget enforcement for this provider")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider budget argument")
	}
	if strings.TrimSpace(*configPath) == "" {
		return errors.New("configuration path cannot be empty")
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read config")
	}
	c, legacyBudget, legacyFieldsReplaced, err := gateway.LoadConfigForProviderBudgetMigration(path)
	if err != nil {
		return err
	}
	c, err = migrateLegacyProvider(c, nil)
	if err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		return errors.New("no provider is configured; run `tidemux provider add` first")
	}
	if ref == "" {
		if len(c.Providers) != 1 {
			return errors.New("specify a provider: `tidemux provider budget REF`")
		}
		ref = sortedProviderNames(c.Providers)[0]
	}
	provider, ok := c.Providers[ref]
	if !ok {
		return fmt.Errorf("provider %q not found", ref)
	}
	legacyMigration := legacyBudget != (ledger.BudgetPolicy{})
	var current ledger.BudgetPolicy
	if provider.Budget != nil {
		current = *provider.Budget
	}
	if legacyMigration {
		if current != (ledger.BudgetPolicy{}) && !*disable {
			return fmt.Errorf("global budget and provider %q already have separate policies; use --disable to discard the old global policy", ref)
		}
		if current == (ledger.BudgetPolicy{}) && !*disable {
			current = legacyBudget
		}
	}

	interactive := !*disable && !flagWasSet(flags, "budget-5h", "budget-weekly", "budget-currency", "budget-mode", "budget-alert-threshold")
	if interactive {
		current, err = promptBudget(stdin, stdout, current)
		if err != nil {
			return err
		}
	} else if *disable {
		current = ledger.BudgetPolicy{}
	} else {
		if flagWasSet(flags, "budget-5h") {
			current.FiveHourLimit = *fiveHour
		}
		if flagWasSet(flags, "budget-weekly") {
			current.WeeklyLimit = *weekly
		}
		if flagWasSet(flags, "budget-currency") {
			current.Currency = *currency
		}
		if flagWasSet(flags, "budget-mode") {
			current.Mode = *mode
		}
		if flagWasSet(flags, "budget-alert-threshold") {
			current.AlertThreshold = *threshold
		}
	}
	if current.FiveHourLimit == 0 && current.WeeklyLimit == 0 {
		current = ledger.BudgetPolicy{}
	} else {
		if current.Currency == "" {
			current.Currency = "USD"
		}
		if current.AlertThreshold == 0 {
			current.AlertThreshold = 0.8
		}
		if current.Mode == "" {
			current.Mode = "hard"
		}
	}
	if current == (ledger.BudgetPolicy{}) {
		provider.Budget = nil
	} else {
		provider.Budget = &current
	}
	c.Providers[ref] = provider
	if err := writeCommandConfig(path, c, before); err != nil {
		return err
	}
	if legacyMigration {
		if *disable {
			fmt.Fprintln(stdout, "Removed the former global budget policy.")
		} else {
			fmt.Fprintf(stdout, "Moved the former global budget policy to provider %s.\n", ref)
		}
	}
	if legacyFieldsReplaced {
		fmt.Fprintln(stdout, "Replaced the incompatible legacy budget fields with the current provider policy.")
	}
	fmt.Fprintf(stdout, "Budget configuration updated for provider %s.\n", ref)
	return nil
}

func promptBudget(in *os.File, out *os.File, current ledger.BudgetPolicy) (ledger.BudgetPolicy, error) {
	s := bufio.NewScanner(in)
	read := func(label, value string) (string, error) {
		fmt.Fprintf(out, "%s [%s]: ", label, value)
		if !s.Scan() {
			return "", errors.New("interactive input ended")
		}
		answer := strings.TrimSpace(s.Text())
		if answer == "" {
			return value, nil
		}
		return answer, nil
	}
	var err error
	var value string
	if value, err = read("5h limit", formatBudget(current.FiveHourLimit)); err != nil {
		return current, err
	}
	current.FiveHourLimit, err = parseBudgetValue(value)
	if err != nil {
		return current, err
	}
	if value, err = read("weekly limit", formatBudget(current.WeeklyLimit)); err != nil {
		return current, err
	}
	current.WeeklyLimit, err = parseBudgetValue(value)
	if err != nil {
		return current, err
	}
	if current.FiveHourLimit == 0 && current.WeeklyLimit == 0 {
		return ledger.BudgetPolicy{}, nil
	}
	if current.Currency == "" {
		current.Currency = "USD"
	}
	if current.Mode == "" {
		current.Mode = "hard"
	}
	if current.AlertThreshold == 0 {
		current.AlertThreshold = .8
	}
	if current.Currency, err = read("currency", current.Currency); err != nil {
		return current, err
	}
	if current.Mode, err = read("mode", current.Mode); err != nil {
		return current, err
	}
	if value, err = read("alert threshold", strconv.FormatFloat(current.AlertThreshold, 'g', -1, 64)); err != nil {
		return current, err
	}
	current.AlertThreshold, err = strconv.ParseFloat(value, 64)
	if err != nil {
		return current, errors.New("invalid alert threshold")
	}
	return current, nil
}

func parseBudgetValue(value string) (float64, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 {
		return 0, errors.New("budget limit must be a non-negative number")
	}
	return n, nil
}

func formatBudget(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }

func flagWasSet(flags *flag.FlagSet, names ...string) bool {
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, name := range names {
		if set[name] {
			return true
		}
	}
	return false
}
