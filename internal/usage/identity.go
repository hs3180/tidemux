package usage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Signer struct{ key []byte }

// OpenSigner keeps a profile-local private key beside the ledger. Export-file
// rotation cannot change grouping. It never persists the caller or session ID.
func OpenSigner(ledgerPath string) (*Signer, error) {
	path := ledgerPath + ".usage-key"
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("usage_identity_unavailable")
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, errors.New("usage_identity_unavailable")
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, errors.New("usage_identity_unavailable")
		}
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return nil, errors.New("usage_identity_unavailable")
		}
		err = directory.Sync()
		directory.Close()
		if err != nil {
			return nil, errors.New("usage_identity_unavailable")
		}
		return &Signer{key: key}, nil
	}
	if err != nil {
		return nil, errors.New("usage_identity_unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("usage_identity_unavailable")
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, errors.New("usage_identity_unavailable")
	}
	return &Signer{key: key}, nil
}

func (s *Signer) digest(parts ...string) string {
	h := hmac.New(sha256.New, s.key)
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Signer) SourceID() string { return s.digest("tidemux-usage-source-v1") }

func (s *Signer) SessionGroup(protocol, caller, session string) string {
	if s == nil || strings.TrimSpace(session) == "" {
		return ""
	}
	return s.digest("tidemux-usage-session-v1", protocol, caller, strings.TrimSpace(session))
}
