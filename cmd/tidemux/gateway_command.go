package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

func gatewayCommand(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 || args[0] != "configure" {
		return errors.New("usage: tidemux gateway configure [options]")
	}
	return gatewayConfigure(args[1:], stdout, stderr)
}

func gatewayConfigure(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("gateway configure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", defaultConfigPath(), "configuration path")
	listen := flags.String("listen", "", "listener mode: loopback or 0.0.0.0")
	maxInFlight := flags.Int("max-in-flight", 0, "maximum simultaneous upstream requests")
	maxSessions := flags.Int("max-active-sessions", -1, "maximum active logical sessions; zero disables the limit")
	idleSeconds := flags.Int("active-session-idle-timeout-seconds", -1, "idle time before releasing a retained session; zero uses five minutes")
	rotateKey := flags.Bool("rotate-key", false, "set a new gateway key using hidden input; Enter generates one")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected gateway configure argument")
	}
	if *listen != "" && *listen != "loopback" && *listen != "0.0.0.0" {
		return errors.New("--listen must be loopback or 0.0.0.0")
	}
	c, abs, before, err := loadCommandConfig(*path)
	if err != nil {
		return err
	}
	if c.BaseURL != "" {
		c, err = migrateLegacyProvider(c, gateway.MacOSKeychain{})
		if err != nil {
			return err
		}
	}
	interactive := !flagWasSet(flags, "listen", "max-in-flight", "max-active-sessions", "active-session-idle-timeout-seconds", "rotate-key")
	var tty *os.File
	if interactive || (*listen == "0.0.0.0" && isLoopbackListenAddr(c.ListenAddr)) || *rotateKey || credentialRefEmpty(c.AccessTokenKeychain) {
		if runtime.GOOS != "darwin" {
			return errors.New("gateway credentials require macOS Keychain")
		}
		tty, err = openControlTTY("gateway setup requires an interactive terminal")
		if err != nil {
			return err
		}
		defer tty.Close()
		if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
			return err
		}
	}
	if interactive {
		mode := "loopback"
		if !isLoopbackListenAddr(c.ListenAddr) {
			mode = "0.0.0.0"
		}
		mode, err = readPromptDefault(tty, "Listener mode (loopback or 0.0.0.0)", mode)
		if err != nil {
			return err
		}
		if mode != "loopback" && mode != "0.0.0.0" {
			return errors.New("listener mode must be loopback or 0.0.0.0")
		}
		*listen = mode
		if *maxInFlight, err = readPromptInt(tty, "Maximum simultaneous upstream requests", c.MaxInFlight); err != nil {
			return err
		}
		if *maxSessions, err = readPromptInt(tty, "Maximum active sessions (0 disables)", c.MaxActiveSessions); err != nil {
			return err
		}
		if *idleSeconds, err = readPromptInt(tty, "Inactive session release seconds (0 means 300)", c.ActiveSessionIdleTimeoutSeconds); err != nil {
			return err
		}
	}
	if *listen != "" {
		_, port, splitErr := net.SplitHostPort(c.ListenAddr)
		if splitErr != nil {
			return errors.New("current listener address is invalid")
		}
		host := "127.0.0.1"
		if *listen == "0.0.0.0" {
			host = "0.0.0.0"
		}
		c.ListenAddr = net.JoinHostPort(host, port)
	}
	if interactive || flagWasSet(flags, "max-in-flight") {
		c.MaxInFlight = *maxInFlight
	}
	if interactive || flagWasSet(flags, "max-active-sessions") {
		c.MaxActiveSessions = *maxSessions
	}
	if interactive || flagWasSet(flags, "active-session-idle-timeout-seconds") {
		c.ActiveSessionIdleTimeoutSeconds = *idleSeconds
	}

	wasLoopback := isLoopbackListenAddr(mustCurrentListenAddr(before, c.ListenAddr))
	needsNewKey := credentialRefEmpty(c.AccessTokenKeychain) || (*listen == "0.0.0.0" && wasLoopback) || *rotateKey
	var generated []byte
	if needsNewKey {
		if tty == nil {
			return errors.New("switching to external access requires a terminal; enter a gateway API key or press Enter to generate one")
		}
		fmt.Fprint(tty, "Gateway API key (hidden; press Enter to generate randomly): ")
		key, readErr := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if readErr != nil {
			return errors.New("could not read gateway API key")
		}
		if len(key) == 0 {
			generated, err = randomCredential()
			if err != nil {
				return err
			}
			key = generated
		}
		defer zeroBytes(key)
		if !validSecret(key) {
			return errors.New("gateway API key is empty or invalid")
		}
		for _, provider := range c.Providers {
			refs, refsErr := provider.KeychainReferences()
			if refsErr != nil {
				return errors.New("cannot verify distinct gateway and provider credentials")
			}
			for _, ref := range refs {
				providerKey, lookupErr := (gateway.MacOSKeychain{}).Lookup(context.Background(), ref)
				if lookupErr != nil {
					return errors.New("cannot verify distinct gateway and provider credentials")
				}
				if string(key) == providerKey {
					return errors.New("gateway API key must differ from every provider API key")
				}
			}
		}
		ref, err := newKeychainReference("com.tidemux.gateway")
		if err != nil {
			return err
		}
		store := gateway.MacOSKeychain{}
		if err := store.StoreNew(context.Background(), ref, string(key)); err != nil {
			return errors.New("cannot save gateway API key in Keychain")
		}
		stored, lookupErr := store.Lookup(context.Background(), ref)
		if lookupErr != nil || stored != string(key) {
			_ = store.Delete(context.Background(), ref)
			return errors.New("gateway API key Keychain read-back verification failed")
		}
		c.AccessTokenKeychain = ref
		if err := writeCommandConfig(abs, c, before); err != nil {
			_ = store.Delete(context.Background(), ref)
			return err
		}
		if len(generated) > 0 {
			fmt.Fprintf(stdout, "Generated gateway API key (copy it now): %s\n", string(generated))
		}
		fmt.Fprintln(stdout, "Gateway API key stored in Keychain.")
	} else {
		if !isLoopbackListenAddr(c.ListenAddr) {
			if _, err := (gateway.MacOSKeychain{}).Lookup(context.Background(), c.AccessTokenKeychain); err != nil {
				return errors.New("external access requires an available gateway API key; rerun with --rotate-key")
			}
		}
		if err := writeCommandConfig(abs, c, before); err != nil {
			return err
		}
	}
	mode := "loopback"
	if !isLoopbackListenAddr(c.ListenAddr) {
		mode = "0.0.0.0"
	}
	fmt.Fprintf(stdout, "Gateway settings updated: listen=%s, max-in-flight=%d, max-active-sessions=%d, idle-timeout=%ds\n", mode, c.MaxInFlight, c.MaxActiveSessions, effectiveIdleTimeout(c.ActiveSessionIdleTimeoutSeconds))
	return nil
}

func readPromptDefault(tty *os.File, label, current string) (string, error) {
	fmt.Fprintf(tty, "%s [%s]: ", label, current)
	value, err := readTerminalLine(tty)
	if err != nil {
		return "", errors.New("could not read gateway setting")
	}
	if strings.TrimSpace(value) == "" {
		return current, nil
	}
	return strings.TrimSpace(value), nil
}

func readPromptInt(tty *os.File, label string, current int) (int, error) {
	value, err := readPromptDefault(tty, label, strconv.Itoa(current))
	if err != nil {
		return 0, err
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", label)
	}
	return number, nil
}

func mustCurrentListenAddr(before []byte, fallback string) string {
	if len(before) == 0 {
		return fallback
	}
	var raw struct {
		ListenAddr string `json:"listen_addr"`
	}
	if json.Unmarshal(before, &raw) != nil || raw.ListenAddr == "" {
		return fallback
	}
	return raw.ListenAddr
}

func effectiveIdleTimeout(seconds int) int {
	if seconds == 0 {
		return 300
	}
	return seconds
}
