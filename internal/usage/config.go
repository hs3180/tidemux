// Package usage exports committed ledger data for the dedicated ccusage source.
// Export files are optional views, never audit or budget authority.
package usage

import (
	"errors"
	"path/filepath"
	"strings"
)

type Config struct {
	Enabled     bool   `json:"enabled"`
	Directory   string `json:"directory,omitempty"`
	PollSeconds int    `json:"poll_seconds,omitempty"`
	MaxBytes    int64  `json:"max_bytes,omitempty"`
	MaxFiles    int    `json:"max_files,omitempty"`
}

func (c Config) Validate() error {
	if c.Directory != "" && (!filepath.IsAbs(c.Directory) || strings.TrimSpace(c.Directory) != c.Directory) {
		return errors.New("usage_log.directory must be an absolute path")
	}
	if c.PollSeconds < 0 || c.PollSeconds > 86400 {
		return errors.New("usage_log.poll_seconds must be 1..86400 or omitted")
	}
	if c.MaxBytes != 0 && (c.MaxBytes < 4096 || c.MaxBytes > 1<<30) {
		return errors.New("usage_log.max_bytes must be 4096..1073741824 or omitted")
	}
	if c.MaxFiles != 0 && (c.MaxFiles < 2 || c.MaxFiles > 64) {
		return errors.New("usage_log.max_files must be 2..64 or omitted")
	}
	return nil
}

func (c Config) Effective(ledgerPath string) Config {
	if c.Directory == "" {
		c.Directory = filepath.Join(filepath.Dir(ledgerPath), "usage")
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 1
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 16 << 20
	}
	if c.MaxFiles == 0 {
		c.MaxFiles = 8
	}
	return c
}
