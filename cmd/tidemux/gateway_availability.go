package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

func gatewayAvailability(args []string, stdout, stderr *os.File, lookup gateway.SecretLookup) error {
	flags := flag.NewFlagSet("gateway availability", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	asJSON := flags.Bool("json", false, "output availability as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		return errors.New("usage: tidemux gateway availability [--json] [--config PATH]")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil || port == "" || port == "0" {
		return errors.New("availability needs a running gateway with a fixed port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() && !ip.IsUnspecified() {
		return errors.New("availability only contacts an IP loopback listener")
	}
	if ip.IsUnspecified() {
		host = "::1"
		if ip.To4() != nil {
			host = "127.0.0.1"
		}
	}
	if lookup == nil {
		return errors.New("gateway Keychain lookup required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := lookup.Lookup(ctx, c.AccessTokenKeychain)
	if err != nil || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
		return errors.New("gateway Keychain item unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/tidemux/availability-status", nil)
	if err != nil {
		return errors.New("could not query gateway availability")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("could not query gateway availability")
	}
	defer response.Body.Close()
	var report gateway.AvailabilityReport
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&report) != nil || report.RecoveryMode != "request_driven" {
		return errors.New("gateway availability response is unavailable or invalid")
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(report)
	}
	fmt.Fprintf(stdout, "Recovery: %s; idle TTL: %ds; observed target limit: %d\n", report.RecoveryMode, report.IdleTTLSeconds, report.StateLimit)
	for _, provider := range report.Providers {
		printAvailability(stdout, provider.Ref, provider.Generation, provider.AvailabilityStatus)
		for _, model := range provider.Models {
			printAvailability(stdout, provider.Ref+"/"+model.Model, provider.Generation, model.AvailabilityStatus)
		}
	}
	return nil
}

func printAvailability(out io.Writer, target string, generation uint64, status gateway.AvailabilityStatus) {
	fmt.Fprintf(out, "%s generation=%d state=%s", target, generation, status.State)
	if status.Reason != "" {
		fmt.Fprintf(out, " reason=%s", status.Reason)
	}
	if !status.RetryAt.IsZero() {
		fmt.Fprintf(out, " retry-at=%s probe-due=%t", status.RetryAt.Format(time.RFC3339), status.ProbeDue)
	}
	fmt.Fprintln(out)
}
