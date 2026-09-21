package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestPromptReportSchedule(t *testing.T) {
	for _, test := range []struct {
		name     string
		input    string
		wantTime string
		wantZero bool
	}{
		{name: "time", input: "08:35\n", wantTime: "08:35"},
		{name: "blank disables", input: "\n", wantZero: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				_, _ = io.WriteString(writer, test.input)
				_ = writer.Close()
			}()
			output := filepath.Join(t.TempDir(), "prompt.txt")
			out, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			schedule, promptErr := promptReportSchedule(reader, out)
			_ = out.Close()
			_ = reader.Close()
			if promptErr != nil {
				t.Fatal(promptErr)
			}
			if test.wantZero {
				if schedule != (gateway.ReportSchedule{}) {
					t.Fatalf("schedule=%+v, want disabled", schedule)
				}
			} else if schedule.Time != test.wantTime || schedule.Channel != "macos" {
				t.Fatalf("schedule=%+v", schedule)
			}
			prompt, err := os.ReadFile(output)
			if err != nil || !strings.Contains(string(prompt), "Daily report notification time") {
				t.Fatalf("prompt=%q err=%v", prompt, err)
			}
		})
	}
}

func TestScheduleCommandWritesAndDisablesSchedule(t *testing.T) {
	path := writeScheduleTestConfig(t)
	var calls []gateway.ReportSchedule
	sync := func(_ string, schedule gateway.ReportSchedule) (string, error) {
		calls = append(calls, schedule)
		return "/tmp/tidemux-test-report.plist", nil
	}
	stdout := filepath.Join(t.TempDir(), "stdout.txt")
	out, err := os.OpenFile(stdout, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduleCommandWithSync([]string{"--config", path, "--time", "09:05"}, out, os.Stderr, sync); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	configured, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if configured.ReportSchedule != (gateway.ReportSchedule{Time: "09:05", Channel: "macos"}) {
		t.Fatalf("schedule=%+v", configured.ReportSchedule)
	}
	if len(calls) != 1 || calls[0].Time != "09:05" {
		t.Fatalf("sync calls=%+v", calls)
	}
	if err := scheduleCommandWithSync([]string{"--config", path, "--disable"}, os.Stdout, os.Stderr, sync); err != nil {
		t.Fatal(err)
	}
	disabled, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.ReportSchedule != (gateway.ReportSchedule{}) {
		t.Fatalf("disabled schedule=%+v", disabled.ReportSchedule)
	}
	if len(calls) != 2 || calls[1] != (gateway.ReportSchedule{}) {
		t.Fatalf("disable sync calls=%+v", calls)
	}
	backups, err := filepath.Glob(path + ".backup-schedule-*")
	if err != nil || len(backups) != 2 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
}

func TestScheduleCommandRejectsInvalidTimeWithoutChangingConfig(t *testing.T) {
	path := writeScheduleTestConfig(t)
	if err := scheduleCommandWithSync([]string{"--config", path, "--time", "25:00"}, os.Stdout, os.Stderr, func(string, gateway.ReportSchedule) (string, error) {
		t.Fatal("sync called for invalid time")
		return "", nil
	}); err == nil {
		t.Fatal("invalid schedule time accepted")
	}
	c, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReportSchedule != (gateway.ReportSchedule{}) {
		t.Fatalf("config changed after invalid time: %+v", c.ReportSchedule)
	}
}

func TestScheduleCommandSupportsWebhookChannel(t *testing.T) {
	path := writeWebhookScheduleTestConfig(t)
	var calls []gateway.ReportSchedule
	sync := func(_ string, schedule gateway.ReportSchedule) (string, error) {
		calls = append(calls, schedule)
		return "/tmp/tidemux-test-report.plist", nil
	}
	if err := scheduleCommandWithSync([]string{"--config", path, "--time", "09:05", "--channel", "webhook"}, os.Stdout, os.Stderr, sync); err != nil {
		t.Fatal(err)
	}
	configured, err := gateway.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := gateway.ReportSchedule{Time: "09:05", Channel: "webhook"}
	if configured.ReportSchedule != want || len(calls) != 1 || calls[0] != want {
		t.Fatalf("schedule=%+v calls=%+v", configured.ReportSchedule, calls)
	}
}

func TestRenderReportLaunchAgentEscapesArguments(t *testing.T) {
	plist := string(renderReportLaunchAgent("label", "/tmp/tide&mux", "/tmp/config<one>.json", 9, 7))
	for _, want := range []string{"<key>Hour</key><integer>9</integer>", "<key>Minute</key><integer>7</integer>", "/tmp/tide&amp;mux", "/tmp/config&lt;one&gt;.json"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q: %s", want, plist)
		}
	}
}

func TestInstallReportScheduleWritesAndLoadsLaunchAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	home := t.TempDir()
	fakeBin := t.TempDir()
	launchctl := filepath.Join(fakeBin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configPath := filepath.Join(t.TempDir(), "config.json")
	plistPath, err := installReportSchedule(configPath, gateway.ReportSchedule{Time: "09:07", Channel: "macos"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"<key>Hour</key><integer>9</integer>", "<key>Minute</key><integer>7</integer>", "<string>report</string>", "<string>notify</string>", "<string>--config</string>"} {
		if !strings.Contains(content, want) {
			t.Fatalf("LaunchAgent missing %q: %s", want, content)
		}
	}
	info, err := os.Stat(plistPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("LaunchAgent permissions=%v err=%v", info, err)
	}
	if err := removeReportSchedule(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("LaunchAgent remains after removal: %v", err)
	}
}

func writeScheduleTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := gateway.Config{
		ListenAddr: "127.0.0.1:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "model", UpstreamID: "test",
		UpstreamKeychain:    gateway.KeychainReference{Service: "test.provider", Account: "default"},
		AccessTokenKeychain: gateway.KeychainReference{Service: "test.gateway", Account: "default"},
		MaxInFlight:         1, LedgerPath: filepath.Join(dir, "ledger.db"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWebhookScheduleTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := gateway.Config{
		ListenAddr: "127.0.0.1:8787", Protocol: "openai", BaseURL: "https://example.com/v1", Model: "model", UpstreamID: "test",
		UpstreamKeychain:    gateway.KeychainReference{Service: "test.provider", Account: "default"},
		AccessTokenKeychain: gateway.KeychainReference{Service: "test.gateway", Account: "default"},
		MaxInFlight:         1, LedgerPath: filepath.Join(dir, "ledger.db"),
		ReportWebhook: gateway.ReportWebhookConfig{Provider: "generic", Keychain: gateway.KeychainReference{Service: "webhook", Account: "default"}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
