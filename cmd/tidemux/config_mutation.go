package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/hs3180/tidemux/internal/gateway"
)

func loadCommandConfig(path string) (gateway.Config, string, []byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return gateway.Config{}, "", nil, errors.New("invalid config path")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return gateway.Config{ListenAddr: defaultListenAddr, MaxInFlight: 1, LedgerPath: filepath.Join(filepath.Dir(abs), "ledger.db")}, abs, nil, nil
		}
		return gateway.Config{}, abs, nil, errors.New("cannot read config")
	}
	c, err := gateway.LoadConfig(abs)
	if err != nil {
		return gateway.Config{}, abs, nil, err
	}
	return c, abs, data, nil
}

func writeCommandConfig(path string, config gateway.Config, expected []byte) error {
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return errors.New("cannot encode config")
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("cannot create configuration directory")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tidemux-config-*")
	if err != nil {
		return errors.New("cannot prepare config")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot protect config")
	}
	if _, err := tmp.Write(data); err != nil {
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
	current, err := os.ReadFile(path)
	if expected == nil {
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return errors.New("config appeared during update; no changes were made")
		}
	} else if err != nil || !bytes.Equal(current, expected) {
		return errors.New("config changed during update; no changes were made")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.New("could not atomically install config")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func credentialRefEmpty(ref gateway.KeychainReference) bool {
	return ref.Service == "" && ref.Account == ""
}

func newKeychainReference(service string) (gateway.KeychainReference, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return gateway.KeychainReference{}, errors.New("cannot generate credential reference")
	}
	return gateway.KeychainReference{Service: service, Account: hex.EncodeToString(nonce)}, nil
}

func randomCredential() ([]byte, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, errors.New("cannot generate gateway credential")
	}
	encoded := make([]byte, hex.EncodedLen(len(value)))
	hex.Encode(encoded, value)
	for i := range value {
		value[i] = 0
	}
	return encoded, nil
}
