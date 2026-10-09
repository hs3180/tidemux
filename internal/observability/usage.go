package observability

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// UsageLog appends terminal request logs in Claude's project/session layout.
// It has no ledger reader, background worker or persisted export state.
type UsageLog struct {
	directory   string
	key         []byte
	mu          sync.Mutex
	lastWarning time.Time
}

func NewUsageLog(directory, callerCredential string) *UsageLog {
	return &UsageLog{directory: directory, key: []byte(callerCredential)}
}

// SessionID groups client sessions without logging their IDs or credentials.
// Reusing the gateway credential keeps identities stable across restarts.
func (l *UsageLog) SessionID(protocol, session string, requestScoped bool) string {
	if l == nil || session == "" || requestScoped {
		return ""
	}
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte("tidemux-log-session\x00" + protocol + "\x00" + session))
	return hex.EncodeToString(mac.Sum(nil))
}

type usageRecord struct {
	Timestamp string       `json:"timestamp"`
	RequestID string       `json:"requestId"`
	SessionID string       `json:"sessionId,omitempty"`
	Type      string       `json:"type"`
	Message   usageMessage `json:"message"`
}

type usageMessage struct {
	ID    string      `json:"id"`
	Model string      `json:"model"`
	Usage *usageCount `json:"usage,omitempty"`
}

type usageCount struct {
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWrite *int64 `json:"cache_creation_input_tokens,omitempty"`
}

func (e RequestSummary) usageRecord(requestID, model string) usageRecord {
	r := usageRecord{Timestamp: time.UnixMilli(e.TimestampMS).UTC().Format("2006-01-02T15:04:05.000Z"),
		RequestID: requestID, Type: "assistant", Message: usageMessage{ID: requestID, Model: model}}
	if group, err := hex.DecodeString(e.SessionID); err == nil && len(group) == sha256.Size {
		r.SessionID = e.SessionID
	}
	// Unknown input/output are omitted, so ccusage cannot count them as zero.
	if e.InputTokens == nil || e.OutputTokens == nil || *e.InputTokens < 0 || *e.OutputTokens < 0 {
		return r
	}
	// TideMux input includes cached tokens; Claude's input counts cache misses.
	input := *e.InputTokens
	for _, count := range []*int64{e.CacheReadTokens, e.CacheWriteTokens} {
		if count != nil {
			if *count < 0 || *count > input {
				return r
			}
			input -= *count
		}
	}
	r.Message.Usage = &usageCount{Input: input, Output: *e.OutputTokens, CacheRead: e.CacheReadTokens, CacheWrite: e.CacheWriteTokens}
	return r
}

func (l *UsageLog) append(record usageRecord, logger *slog.Logger) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.write(record); err != nil && (l.lastWarning.IsZero() || time.Since(l.lastWarning) >= time.Minute) {
		// Do not forward filesystem errors, which may contain private paths.
		LoggerOrDiscard(logger).Warn("usage log write failed", slog.Int("schema_version", SchemaVersion),
			slog.String("event", "usage_log_write_failure"))
		l.lastWarning = time.Now()
	}
}

func (l *UsageLog) write(record usageRecord) error {
	if record.RequestID == "" || record.Message.Model == "" {
		return nil
	}
	for _, dir := range []string{l.directory, filepath.Join(l.directory, "projects"), filepath.Join(l.directory, "projects", "tidemux")} {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("usage log directory is not private")
		}
	}
	session := record.SessionID
	if session == "" {
		session = "ungrouped"
	}
	path := filepath.Join(l.directory, "projects", "tidemux", session+".jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("usage log file is not private")
	}
	if info.Size() > 0 {
		var tail [1]byte
		if _, err := f.ReadAt(tail[:], info.Size()-1); err != nil {
			return err
		}
		if tail[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
