package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/hs3180/tidemux/internal/gateway"
	"golang.org/x/term"
)

// interactiveProviderSetup holds the provider profile and its in-memory key
// until the Keychain/config transaction commits.
type interactiveProviderSetup struct {
	Name     string
	Provider gateway.Provider
	APIKey   []byte
}

func providerNameFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "provider"
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || net.ParseIP(host) != nil {
		return "local-provider"
	}
	labels := strings.Split(host, ".")
	for len(labels) > 1 && (labels[0] == "api" || labels[0] == "www") {
		labels = labels[1:]
	}
	candidate := labels[0]
	var slug strings.Builder
	lastHyphen := false
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && slug.Len() > 0 {
			slug.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		return "provider"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func promptProviderProtocol(in, out *os.File) (string, error) {
	for {
		fmt.Fprint(out, "Provider protocol could not be inferred. Enter openai or anthropic: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return "", errors.New("could not read provider protocol")
		}
		protocol := strings.ToLower(strings.TrimSpace(value))
		if protocol == "openai" || protocol == "anthropic" {
			return protocol, nil
		}
		fmt.Fprintln(out, "Provider protocol must be openai or anthropic.")
	}
}

func promptNewProvider(in, out *os.File, anthropicVersion string, httpClient *http.Client, forcedProtocol, preferredName string) (interactiveProviderSetup, error) {
	var baseURL string
	for {
		fmt.Fprint(out, "Provider API Base URL: ")
		value, err := readTerminalLine(in)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API Base URL")
		}
		baseURL = strings.TrimSpace(value)
		if baseURL == "" {
			fmt.Fprintln(out, "Provider API Base URL is required.")
			continue
		}
		if err := gateway.ValidateProviderBaseURL(baseURL); err != nil {
			fmt.Fprintf(out, "Invalid provider API Base URL: %v\n", err)
			continue
		}
		break
	}
	name := providerNameFromBaseURL(baseURL)
	if preferredName != "" {
		name = preferredName
	}
	fmt.Fprintf(out, "Provider reference: %s\n", name)
	var apiKey []byte
	for {
		fmt.Fprint(out, "Provider API key (hidden): ")
		var err error
		apiKey, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return interactiveProviderSetup{}, errors.New("could not read provider API key")
		}
		if strings.TrimSpace(string(apiKey)) != "" && !strings.ContainsAny(string(apiKey), "\r\n\x00") {
			break
		}
		for i := range apiKey {
			apiKey[i] = 0
		}
		fmt.Fprintln(out, "A valid provider API key is required.")
	}

	setup, err := inspectAndCompleteProvider(in, baseURL, string(apiKey), forcedProtocol, anthropicVersion, httpClient)
	if err != nil {
		zeroBytes(apiKey)
		return interactiveProviderSetup{}, err
	}
	setup.Name = name
	setup.Provider.UpstreamID = name
	setup.APIKey = apiKey
	return setup, nil
}
