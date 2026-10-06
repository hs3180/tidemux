package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	usagelog "github.com/hs3180/tidemux/internal/usage"
)

func usageCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux usage configure|status|export [--config PATH]")
	}
	command := args[0]
	if command != "configure" && command != "status" && command != "export" {
		return errors.New("unknown usage command")
	}
	flags := flag.NewFlagSet("usage "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "gateway configuration")
	var enable, disable, asJSON, backfill bool
	var config usagelog.Config
	if command == "configure" {
		flags.BoolVar(&enable, "enable", false, "explicitly enable usage logs and pseudonymous session grouping")
		flags.BoolVar(&disable, "disable", false, "remove optional usage_log configuration; preserve files and history")
		flags.StringVar(&config.Directory, "directory", "", "dedicated private absolute output directory")
		flags.IntVar(&config.PollSeconds, "poll-seconds", 0, "export interval; 0 uses one second")
		flags.Int64Var(&config.MaxBytes, "max-bytes", 0, "rotation size; 0 uses 16 MiB")
		flags.IntVar(&config.MaxFiles, "max-files", 0, "retained output files; 0 uses eight")
	} else if command == "status" {
		flags.BoolVar(&asJSON, "json", false, "structured exporter status")
	} else {
		flags.BoolVar(&backfill, "backfill", false, "replay existing committed history; consumer deduplicates")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected usage command argument")
	}
	c, abs, original, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	if command == "configure" {
		if enable == disable {
			return errors.New("choose exactly one of --enable or --disable")
		}
		if disable {
			if config != (usagelog.Config{}) {
				return errors.New("output settings require --enable")
			}
			c.UsageLog = nil
		} else {
			config.Enabled = true
			if err := config.Validate(); err != nil {
				return err
			}
			c.UsageLog = &config
		}
		if err := writeCommandConfig(abs, c, original); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "usage: configuration saved; restart the gateway to apply. Existing ledger and logs are preserved.")
		return nil
	}
	if c.UsageLog == nil || !c.UsageLog.Enabled {
		if command == "export" {
			return errors.New("usage export requires explicit usage configure --enable")
		}
		if asJSON {
			return json.NewEncoder(stdout).Encode(usagelog.Status{})
		}
		fmt.Fprintln(stdout, "usage: disabled (default)")
		return nil
	}
	if command == "status" {
		s, err := usagelog.ReadStatus(c.LedgerPath)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(stdout).Encode(s)
		}
		fmt.Fprintf(stdout, "usage: enabled; last success=%s; error=%s; audit cursor=%d; statement cursor=%d\n", s.LastSuccess, s.ErrorCode, s.AuditRowID, s.StatementID)
		return nil
	}
	signer, err := usagelog.OpenSigner(c.LedgerPath)
	if err != nil {
		return err
	}
	exporter, err := usagelog.New(*c.UsageLog, c.LedgerPath, signer)
	if err != nil {
		return err
	}
	defer exporter.Close()
	var previous usagelog.Status
	for {
		s, err := exporter.Sync(context.Background(), backfill)
		if err != nil {
			return err
		}
		backfill = false
		if s.AuditRowID == previous.AuditRowID && s.StatementID == previous.StatementID {
			return json.NewEncoder(stdout).Encode(s)
		}
		previous = s
	}
}
