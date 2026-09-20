package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

func budgetCommand(args []string, stdin *os.File, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("budget", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "configuration path")
	fiveHour := flags.Float64("budget-5h", 0, "rolling five-hour limit; zero disables it")
	weekly := flags.Float64("budget-weekly", 0, "rolling seven-day limit; zero disables it")
	currency := flags.String("budget-currency", "", "budget currency")
	mode := flags.String("budget-mode", "", "budget mode: alert, soft or hard")
	threshold := flags.Float64("budget-alert-threshold", 0, "alert threshold from 0 to 1")
	pricingFile := flags.String("pricing-file", "", "JSON pricing fragment to merge")
	disable := flags.Bool("disable", false, "disable budget enforcement")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected budget argument")
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read config")
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid config JSON")
	}
	var current ledger.BudgetPolicy
	if value, ok := raw["budget"]; ok {
		encoded, _ := json.Marshal(value)
		if err := json.Unmarshal(encoded, &current); err != nil {
			return errors.New("invalid budget configuration")
		}
	}

	interactive := !*disable && !flagWasSet(flags, "budget-5h", "budget-weekly", "budget-currency", "budget-mode", "budget-alert-threshold", "pricing-file")
	pricingPath := *pricingFile
	if interactive {
		current, pricingPath, err = promptBudget(stdin, stdout, current)
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
	if current != (ledger.BudgetPolicy{}) {
		if current.Currency == "" {
			current.Currency = "USD"
		}
		if current.AlertThreshold == 0 {
			current.AlertThreshold = 0.8
		}
		if current.Mode == "" {
			current.Mode = "hard"
		}
		raw["budget"] = current
	} else {
		delete(raw, "budget")
	}
	if strings.TrimSpace(pricingPath) != "" {
		prices, err := loadPricingFile(pricingPath)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(prices)
		var incoming map[string]any
		json.Unmarshal(encoded, &incoming)
		if existing, ok := raw["prices"].(map[string]any); ok {
			for model, price := range incoming {
				existing[model] = price
			}
		} else {
			raw["prices"] = incoming
		}
	}
	updated, err := json.Marshal(raw)
	if err != nil {
		return errors.New("cannot encode config")
	}
	validated, err := os.CreateTemp(filepath.Dir(path), ".tidemux-budget-*")
	if err != nil {
		return errors.New("cannot prepare config")
	}
	tmp := validated.Name()
	defer os.Remove(tmp)
	if err := validated.Chmod(0600); err != nil {
		validated.Close()
		return errors.New("cannot protect config")
	}
	if _, err := validated.Write(append(prettyJSON(updated), '\n')); err != nil {
		validated.Close()
		return errors.New("cannot write config")
	}
	if err := validated.Sync(); err != nil {
		validated.Close()
		return errors.New("cannot sync config")
	}
	if err := validated.Close(); err != nil {
		return errors.New("cannot close config")
	}
	if _, err := gateway.LoadConfig(tmp); err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.backup-budget-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(backup, data, 0600); err != nil {
		return errors.New("cannot create config backup")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.New("cannot install config")
	}
	fmt.Fprintf(stdout, "Budget configuration updated: %s\n", path)
	return nil
}

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

func promptBudget(in *os.File, out *os.File, current ledger.BudgetPolicy) (ledger.BudgetPolicy, string, error) {
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
		return current, "", err
	}
	current.FiveHourLimit, err = parseBudgetValue(value)
	if err != nil {
		return current, "", err
	}
	if value, err = read("weekly limit", formatBudget(current.WeeklyLimit)); err != nil {
		return current, "", err
	}
	current.WeeklyLimit, err = parseBudgetValue(value)
	if err != nil {
		return current, "", err
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
		return current, "", err
	}
	if current.Mode, err = read("mode", current.Mode); err != nil {
		return current, "", err
	}
	if value, err = read("alert threshold", strconv.FormatFloat(current.AlertThreshold, 'g', -1, 64)); err != nil {
		return current, "", err
	}
	current.AlertThreshold, err = strconv.ParseFloat(value, 64)
	if err != nil {
		return current, "", errors.New("invalid alert threshold")
	}
	pricing, err := read("pricing file (blank to keep current)", "")
	if err != nil {
		return current, "", err
	}
	return current, pricing, nil
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

func prettyJSON(data []byte) []byte {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return data
	}
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return encoded
}
