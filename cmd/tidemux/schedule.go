package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

type reportScheduleSync func(string, gateway.ReportSchedule) (string, error)

func promptReportSchedule(in, out *os.File) (gateway.ReportSchedule, error) {
	fmt.Fprint(out, "Daily report notification time (24-hour HH:MM, blank to disable) [disabled]: ")
	value, err := readTerminalLine(in)
	if err != nil {
		return gateway.ReportSchedule{}, errors.New("could not read notification time")
	}
	if strings.TrimSpace(value) == "" {
		return gateway.ReportSchedule{}, nil
	}
	normalized, err := gateway.NormalizeReportScheduleTime(value)
	if err != nil {
		return gateway.ReportSchedule{}, err
	}
	return gateway.ReportSchedule{Time: normalized, Channel: "macos"}, nil
}

// readTerminalLine avoids bufio read-ahead before a setup prompt switches to the
// password reader on the same controlling terminal.
func readTerminalLine(in *os.File) (string, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := in.Read(one[:])
		if n > 0 {
			switch one[0] {
			case '\n':
				return string(line), nil
			case '\r':
			default:
				line = append(line, one[0])
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
	}
}

func scheduleCommand(args []string, stdout, stderr *os.File) error {
	return scheduleCommandWithSync(args, stdout, stderr, syncReportSchedule)
}

func scheduleCommandWithSync(args []string, stdout, stderr *os.File, sync reportScheduleSync) error {
	flags := flag.NewFlagSet("report schedule", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	timeValue := flags.String("time", "", "daily notification time in local time (HH:MM)")
	disable := flags.Bool("disable", false, "disable scheduled notifications")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*configPath) == "" {
		return errors.New(usage)
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	current, err := gateway.LoadConfig(path)
	if err != nil {
		return err
	}
	if *disable {
		if flagWasSet(flags, "time") {
			return errors.New("--disable cannot be combined with --time")
		}
		if err := replaceReportScheduleConfig(path, gateway.ReportSchedule{}); err != nil {
			return err
		}
		if _, err := sync(path, gateway.ReportSchedule{}); err != nil {
			return fmt.Errorf("schedule disabled in configuration, but launchd cleanup failed: %w", err)
		}
		fmt.Fprintf(stdout, "Daily report notifications disabled: %s\n", path)
		return nil
	}
	if strings.TrimSpace(*timeValue) == "" {
		return errors.New("report schedule requires --time HH:MM or --disable")
	}
	normalized, err := gateway.NormalizeReportScheduleTime(*timeValue)
	if err != nil {
		return err
	}
	schedule := gateway.ReportSchedule{Time: normalized, Channel: "macos"}
	candidate := current
	candidate.ReportSchedule = schedule
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := replaceReportScheduleConfig(path, schedule); err != nil {
		return err
	}
	plistPath, err := sync(path, schedule)
	if err != nil {
		return fmt.Errorf("schedule saved in configuration, but launchd setup failed: %w", err)
	}
	fmt.Fprintf(stdout, "Daily report notification scheduled at %s (macOS Notification Center, local time)\n", schedule.Time)
	if plistPath != "" {
		fmt.Fprintf(stdout, "LaunchAgent: %s\n", plistPath)
	}
	return nil
}

func replaceReportScheduleConfig(path string, schedule gateway.ReportSchedule) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read config")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid config JSON")
	}
	if schedule == (gateway.ReportSchedule{}) {
		delete(raw, "report_schedule")
	} else {
		encoded, err := json.Marshal(schedule)
		if err != nil {
			return errors.New("cannot encode report schedule")
		}
		raw["report_schedule"] = encoded
	}
	updated, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return errors.New("cannot encode config")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tidemux-schedule-*")
	if err != nil {
		return errors.New("cannot prepare config")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot protect config")
	}
	if _, err := tmp.Write(append(updated, '\n')); err != nil {
		tmp.Close()
		return errors.New("cannot write config")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot sync config")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close config")
	}
	if _, err := gateway.LoadConfig(tmpName); err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.backup-schedule-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return errors.New("cannot create config backup")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("cannot install configuration")
	}
	return nil
}

func syncReportSchedule(configPath string, schedule gateway.ReportSchedule) (string, error) {
	if schedule == (gateway.ReportSchedule{}) {
		return "", removeReportSchedule(configPath)
	}
	return installReportSchedule(configPath, schedule)
}

func installReportSchedule(configPath string, schedule gateway.ReportSchedule) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("scheduled notifications require macOS launchd")
	}
	hour, minute, err := schedule.HourMinute()
	if err != nil {
		return "", err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return "", errors.New("invalid config path")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot find tidemux executable")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", errors.New("invalid tidemux executable path")
	}
	plistPath, err := reportSchedulePlistPath(configPath)
	if err != nil {
		return "", err
	}
	if err := unloadReportLaunchAgent(plistPath); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		return "", errors.New("cannot create LaunchAgents directory")
	}
	data := renderReportLaunchAgent(reportScheduleLabel(configPath), executable, configPath, hour, minute)
	if err := writePrivateFile(plistPath, data); err != nil {
		return "", err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if output, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		_ = output
		return "", fmt.Errorf("launchctl bootstrap failed: %w", err)
	}
	return plistPath, nil
}

func removeReportSchedule(configPath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	plistPath, err := reportSchedulePlistPath(configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return nil
	}
	if err := unloadReportLaunchAgent(plistPath); err != nil {
		return err
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return errors.New("cannot remove scheduled notification LaunchAgent")
	}
	return nil
}

func unloadReportLaunchAgent(plistPath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// bootout is intentionally best-effort: the job may not have been loaded
	// yet, while bootstrap must still be attempted with the new plist.
	_ = exec.Command("launchctl", "bootout", domain, plistPath).Run()
	return nil
}

func reportScheduleLabel(configPath string) string {
	absolute, _ := filepath.Abs(configPath)
	sum := sha256.Sum256([]byte(absolute))
	return "com.tidemux.daily-report." + hex.EncodeToString(sum[:6])
}

func reportSchedulePlistPath(configPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot find user home directory")
	}
	return filepath.Join(home, "Library", "LaunchAgents", reportScheduleLabel(configPath)+".plist"), nil
}

func renderReportLaunchAgent(label, executable, configPath string, hour, minute int) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key>
<array>
<string>%s</string>
<string>report</string>
<string>notify</string>
<string>--config</string>
<string>%s</string>
</array>
<key>StartCalendarInterval</key>
<dict>
<key>Hour</key><integer>%d</integer>
<key>Minute</key><integer>%d</integer>
</dict>
</dict>
</plist>
`, plistText(label), plistText(executable), plistText(configPath), hour, minute))
}

func plistText(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tidemux-launchagent-*")
	if err != nil {
		return errors.New("cannot prepare LaunchAgent")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot protect LaunchAgent")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write LaunchAgent")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("cannot sync LaunchAgent")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot close LaunchAgent")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("cannot install LaunchAgent")
	}
	return nil
}
