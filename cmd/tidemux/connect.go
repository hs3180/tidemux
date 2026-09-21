package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
)

// launch starts an installed client with a local gateway credential only.
// No upstream key is loaded or passed to the client. The gateway is explicitly
// started with serve so its lifetime is independent of any one client session.
func launch(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New("usage: tidemux <claude|kilo|kilo-ide|hermes> [--config path] [--executable path] -- [client arguments]")
	}
	name := args[0]
	if name != "claude" && name != "kilo" && name != "kilo-ide" && name != "hermes" {
		return errors.New("client must be claude, kilo, kilo-ide or hermes")
	}
	flags := flag.NewFlagSet("tidemux "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "gateway configuration; start tidemux serve with the same file")
	defaultExecutable := name
	if name == "kilo-ide" {
		defaultExecutable = "code"
	}
	extensionsDir := flags.String("extensions-dir", "", "Kilo IDE: optional VS Code extensions directory")
	executable := flags.String("executable", defaultExecutable, "installed client executable or absolute path")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if name != "kilo-ide" && *extensionsDir != "" {
		return errors.New("--extensions-dir is only supported for kilo-ide")
	}
	clientInput := flags.Args()
	if name == "kilo-ide" {
		var err error
		clientInput, err = ideWorkspaceArgs(clientInput, *extensionsDir)
		if err != nil {
			return err
		}
	}
	c, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := validateClientProtocol(name, c.Protocol); err != nil {
		return err
	}
	binary, err := exec.LookPath(*executable)
	if err != nil {
		return errors.New("client executable not found; install it or provide --executable")
	}
	absolute, err := filepath.Abs(*configPath)
	if err != nil {
		return errors.New("invalid config path")
	}
	hash := sha256.Sum256([]byte(absolute))
	state := filepath.Join(filepath.Dir(absolute), "client-state", name+"-"+hex.EncodeToString(hash[:6]))
	if name == "kilo-ide" {
		// VS Code places its Unix IPC socket under user-data-dir on macOS.
		// Keep this path short even when the gateway config is deeply nested.
		home, err := os.UserHomeDir()
		if err != nil {
			return errors.New("cannot locate IDE profile home")
		}
		state = filepath.Join(home, ".tidemux", "ide", hex.EncodeToString(hash[:6]))
		release, err := lockIDEProfile(state)
		if err != nil {
			return err
		}
		defer release()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	token, err := (gateway.MacOSKeychain{}).Lookup(ctx, c.AccessTokenKeychain)
	if err != nil {
		tty, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if ttyErr != nil {
			return errors.New("local gateway credential unavailable; run the client command in Terminal to unlock Keychain, or run provider configure if the credential is missing")
		}
		unlockErr := unlockKeychainIfNeeded(ctx, tty)
		tty.Close()
		if unlockErr != nil {
			return unlockErr
		}
		token, err = (gateway.MacOSKeychain{}).Lookup(ctx, c.AccessTokenKeychain)
		if err != nil {
			return errors.New("local gateway credential unavailable after Keychain check; run provider configure for this profile")
		}
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("invalid local gateway credential")
	}
	url := "http://" + c.ListenAddr
	if err = checkClientEndpoint(ctx, url, token, c.Model); err != nil {
		return err
	}
	overrides, clientArgs, err := clientLaunch(name, url, c.Model, state, os.Getenv("KILO_CONFIG_CONTENT"), c.ModelCapabilities)
	if err != nil {
		return err
	}
	overrides["TIDEMUX_GATEWAY_TOKEN"] = token
	if name == "claude" {
		overrides["ANTHROPIC_API_KEY"] = token
		overrides["ANTHROPIC_AUTH_TOKEN"] = ""
		overrides["CLAUDE_CODE_OAUTH_TOKEN"] = ""
		overrides["CLAUDE_CODE_USE_BEDROCK"] = ""
		overrides["CLAUDE_CODE_USE_VERTEX"] = ""
		overrides["CLAUDE_CODE_USE_FOUNDRY"] = ""
	}
	cmd := exec.CommandContext(ctx, binary, append(clientArgs, clientInput...)...)
	cmd.Env = overrideEnvironment(os.Environ(), overrides)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	fmt.Fprintf(stderr, "Connecting %s to TideMux at %s (model %s).\n", name, url, c.Model)
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%s exited: %w", name, err)
	}
	return nil
}

func validateClientProtocol(name, protocol string) error {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "auto", "openai", "anthropic":
		return nil
	}
	return errors.New("configuration has an invalid provider protocol; use automatic detection or a legacy openai/anthropic value")
}

func checkClientEndpoint(ctx context.Context, url, token, model string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/v1/models", nil)
	if err != nil {
		return errors.New("invalid gateway address")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Do(req)
	if err != nil {
		return errors.New("gateway is not reachable; run tidemux serve --config with the same profile first")
	}
	defer r.Body.Close()
	if r.StatusCode == 401 {
		return errors.New("gateway rejected this profile's local credential; check the running serve profile")
	}
	if r.StatusCode != 200 {
		return errors.New("gateway model discovery unavailable; check the running TideMux build and profile")
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil {
		return errors.New("invalid gateway model list")
	}
	for _, m := range body.Data {
		if m.ID == model {
			return nil
		}
	}
	return errors.New("configured model is not listed by the running gateway")
}
func overrideEnvironment(env []string, overrides map[string]string) []string {
	result := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; !ok {
			result = append(result, entry)
		}
	}
	for k, v := range overrides {
		if v != "" {
			result = append(result, k+"="+v)
		}
	}
	return result
}
func clientLaunch(name, url, model, state, kiloConfig string, declared ...gateway.ModelCapabilities) (map[string]string, []string, error) {
	env := map[string]string{}
	capabilities := gateway.ModelCapabilities{}
	if len(declared) > 0 {
		capabilities = declared[0]
	}
	if err := capabilities.Validate(); err != nil {
		return nil, nil, err
	}
	switch name {
	case "claude":
		env["CLAUDE_CONFIG_DIR"] = state
		env["ANTHROPIC_BASE_URL"] = url
		env["ANTHROPIC_MODEL"] = model
		env["CLAUDE_CODE_SIMPLE"] = "1"
		return env, nil, nil
	case "kilo", "kilo-ide":
		for _, kind := range []string{"CONFIG", "DATA", "CACHE", "STATE"} {
			env["XDG_"+kind+"_HOME"] = filepath.Join(state, strings.ToLower(kind))
		}
		config := map[string]any{}
		if kiloConfig != "" {
			if json.Unmarshal([]byte(kiloConfig), &config) != nil || config == nil {
				return nil, nil, errors.New("existing KILO_CONFIG_CONTENT is invalid JSON")
			}
		}
		providers, ok := config["provider"].(map[string]any)
		if !ok {
			if config["provider"] != nil {
				return nil, nil, errors.New("existing Kilo provider configuration is invalid")
			}
			providers = map[string]any{}
		}
		modelData := map[string]any{"name": model}
		limits := map[string]int64{}
		if capabilities.ContextTokens > 0 {
			limits["context"] = capabilities.ContextTokens
		}
		if capabilities.MaxOutputTokens > 0 {
			limits["output"] = capabilities.MaxOutputTokens
		}
		if len(limits) > 0 {
			modelData["limit"] = limits
		}
		providers["tidemux-local"] = map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "TideMux", "options": map[string]any{"baseURL": url + "/v1", "apiKey": "{env:TIDEMUX_GATEWAY_TOKEN}"}, "models": map[string]any{model: modelData}}
		config["provider"] = providers
		config["model"] = "tidemux-local/" + model
		encoded, _ := json.Marshal(config)
		env["KILO_CONFIG_CONTENT"] = string(encoded)
		if name == "kilo-ide" {
			return env, []string{"--new-window", "--wait", "--user-data-dir", filepath.Join(state, "vscode")}, nil
		}
		return env, nil, nil
	case "hermes":
		// This dedicated profile is TideMux-managed; Hermes sessions are persistent.
		// JSON is valid YAML. No secret is written, only a process environment reference.
		if err := os.MkdirAll(state, 0o700); err != nil {
			return nil, nil, errors.New("cannot create Hermes profile")
		}
		path := filepath.Join(state, "config.yaml")
		marker := filepath.Join(state, ".tidemux-managed")
		if _, err := os.Lstat(path); err == nil {
			if _, err := os.Stat(marker); err != nil {
				return nil, nil, errors.New("refusing to overwrite unmanaged Hermes profile")
			}
		}
		config := map[string]any{"model": map[string]any{"default": model, "provider": "tidemux-local"}, "providers": map[string]any{"tidemux-local": map[string]any{"api": url + "/v1", "key_env": "TIDEMUX_GATEWAY_TOKEN", "transport": "chat_completions"}}}
		if capabilities.ContextTokens > 0 {
			config["model"].(map[string]any)["context_length"] = capabilities.ContextTokens
		}
		encoded, _ := json.MarshalIndent(config, "", "  ")
		f, err := os.CreateTemp(state, ".config-*")
		if err != nil {
			return nil, nil, errors.New("cannot create Hermes config")
		}
		temp := f.Name()
		defer os.Remove(temp)
		_, err = f.Write(encoded)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, nil, errors.New("cannot write Hermes config")
		}
		if err = os.Rename(temp, path); err != nil {
			return nil, nil, errors.New("cannot install Hermes config")
		}
		if err = os.WriteFile(marker, []byte("TideMux owns config.yaml; sessions remain persistent.\n"), 0o600); err != nil {
			return nil, nil, errors.New("cannot mark managed Hermes profile")
		}
		env["HERMES_HOME"] = state
		return env, []string{"chat", "--provider", "tidemux-local", "--model", model}, nil

	}
	return nil, nil, errors.New("unknown client")
}

// IDE arguments are a workspace, not arbitrary Code switches: allowing a second
// user-data-dir or reuse-window could hand the request to an unrelated process
// which did not inherit the credential and provider environment.
func ideWorkspaceArgs(args []string, extensionsDir string) ([]string, error) {
	if len(args) > 1 {
		return nil, errors.New("kilo-ide accepts one workspace directory after --")
	}
	workspace := "."
	if len(args) == 1 {
		workspace = args[0]
	}
	if strings.HasPrefix(workspace, "-") {
		return nil, errors.New("kilo-ide expects a workspace directory, not VS Code flags")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, errors.New("invalid IDE workspace")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return nil, errors.New("IDE workspace must be an existing directory")
	}
	result := []string{}
	if extensionsDir != "" {
		path, err := filepath.Abs(extensionsDir)
		if err != nil {
			return nil, errors.New("invalid extensions directory")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil, errors.New("extensions directory must exist; install Kilo Code there first")
		}
		result = append(result, "--extensions-dir", path)
	}
	return append(result, absolute), nil
}

func lockIDEProfile(state string) (func(), error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, errors.New("cannot create IDE profile")
	}
	f, err := os.OpenFile(filepath.Join(state, ".launch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("cannot lock IDE profile")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("this TideMux IDE profile is already being launched; use its existing window or close it first")
	}
	release := func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
	data, err := os.ReadFile(filepath.Join(state, "vscode", "code.lock"))
	if err != nil && !os.IsNotExist(err) {
		release()
		return nil, errors.New("cannot inspect the existing IDE process")
	}
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			release()
			return nil, errors.New("invalid VS Code process marker; close the dedicated IDE profile before retrying")
		}
		if err = syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			release()
			return nil, errors.New("the dedicated VS Code process is still running; quit that instance before reconnecting so updated credentials and model settings take effect")
		} else if err != syscall.ESRCH {
			release()
			return nil, errors.New("cannot verify the existing IDE process has exited")
		}
	}
	return release, nil
}
