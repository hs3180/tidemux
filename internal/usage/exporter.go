package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

const batchSize = 128

var logName = regexp.MustCompile(`^usage-([0-9]{20})\.jsonl$`)
var groupName = regexp.MustCompile(`^[0-9a-f]{64}$`)

type logFile struct {
	path   string
	group  string
	number int64
}

func logDirectory(directory string) string { return filepath.Join(directory, "projects", "tidemux") }

type checkpoint struct {
	Version     int    `json:"version"`
	SourceID    string `json:"source_id"`
	AuditRowID  int64  `json:"audit_rowid"`
	AuditID     string `json:"audit_id"`
	StatementID int64  `json:"statement_id"`
	File        int64  `json:"file"`
}

type Status struct {
	Enabled     bool   `json:"enabled"`
	LastSuccess string `json:"last_success,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	AuditRowID  int64  `json:"audit_rowid"`
	StatementID int64  `json:"statement_id"`
}

type Exporter struct {
	config      Config
	sourceID    string
	reader      *ledger.Ledger
	afterAppend func() error // test-only crash boundary, before checkpoint.
}

func New(c Config, ledgerPath string, signer *Signer) (*Exporter, error) {
	if !c.Enabled {
		return nil, errors.New("usage_export_disabled")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, errors.New("usage_identity_unavailable")
	}
	reader, err := ledger.OpenReadOnly(ledgerPath)
	if err != nil {
		return nil, errors.New("usage_ledger_unavailable")
	}
	return &Exporter{config: c.Effective(ledgerPath), sourceID: signer.SourceID(), reader: reader}, nil
}

func (e *Exporter) Close() error { return e.reader.Close() }

// Sync exports a bounded committed page. Output is synced BEFORE checkpoint.
// A crash between the two can duplicate a snapshot. Stable message/request IDs
// let external consumers deduplicate token usage. The audit writer is uninvolved.
func (e *Exporter) Sync(ctx context.Context, backfill bool) (result Status, err error) {
	result.Enabled = true
	if err = prepareDirectory(e.config.Directory); err != nil {
		return result, err
	}
	lock, err := privateOpen(filepath.Join(e.config.Directory, ".writer.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return result, errors.New("usage_output_unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return result, errors.New("usage_export_in_use")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Only the lock owner removes orphaned private checkpoint temporary files.
	entries, _ := os.ReadDir(e.config.Directory)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".usage-temp-") {
			if os.Remove(filepath.Join(e.config.Directory, entry.Name())) != nil {
				return result, errors.New("usage_checkpoint_failed")
			}
		}
	}
	state, err := e.readCheckpoint()
	if err != nil {
		return result, err
	}
	if backfill {
		state.AuditRowID = 0
		state.AuditID = ""
		state.StatementID = 0
	}
	// VACUUM can renumber implicit rowids; verify the anchor and replay safely.
	if state.AuditID != "" {
		anchor, readErr := e.reader.UsageAuditByID(ctx, state.AuditID)
		if readErr != nil {
			return result, errors.New("usage_ledger_read_failed")
		}
		if len(anchor) != 1 || anchor[0].RowID != state.AuditRowID {
			state.AuditRowID = 0
			state.AuditID = ""
			state.StatementID = 0
		}
	}
	logs, err := listLogs(e.config.Directory)
	if err != nil {
		return result, err
	}
	var writer *os.File
	var writerGroup string
	if len(logs) > 0 {
		writerGroup = logs[len(logs)-1].group
		writer, err = e.openLog(&state, writerGroup, false)
		if err != nil {
			return result, err
		}
	}
	defer func() {
		if writer != nil {
			writer.Close()
		}
	}()
	appendRow := func(row ledger.UsageAudit) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		record := makeRecord(e.sourceID, row)
		data, encodeErr := json.Marshal(record)
		if encodeErr != nil || len(data) > 65536 {
			return errors.New("usage_record_invalid")
		}
		group := record.SessionID
		if group == "" {
			group = "ungrouped"
		}
		if writer != nil && writerGroup != group {
			if writer.Sync() != nil || writer.Close() != nil {
				return errors.New("usage_output_failed")
			}
			writer = nil
		}
		if writer == nil {
			writer, err = e.openLog(&state, group, false)
			if err != nil {
				return err
			}
			writerGroup = group
		}
		info, statErr := writer.Stat()
		if statErr != nil {
			return errors.New("usage_output_failed")
		}
		if info.Size() > 0 && info.Size()+int64(len(data)+1) > e.config.MaxBytes {
			if writer.Sync() != nil {
				return errors.New("usage_output_failed")
			}
			if writer.Close() != nil {
				return errors.New("usage_output_failed")
			}
			writer = nil
			writer, err = e.openLog(&state, group, true)
			if err != nil {
				return err
			}
		}
		data = append(data, '\n')
		if n, writeErr := writer.Write(data); writeErr != nil || n != len(data) {
			return errors.New("usage_output_failed")
		}
		return nil
	}
	audits, err := e.reader.UsageAudits(ctx, state.AuditRowID, batchSize)
	if err != nil {
		return result, errors.New("usage_ledger_read_failed")
	}
	for _, row := range audits {
		if err = appendRow(row); err != nil {
			return result, err
		}
		state.AuditRowID = row.RowID
		state.AuditID = row.Audit.ID
	}
	statements, err := e.reader.UsageStatements(ctx, state.StatementID, batchSize)
	if err != nil {
		return result, errors.New("usage_ledger_read_failed")
	}
	seen := map[string]bool{}
	for _, statement := range statements {
		if statement.RequestID != "" && !seen[statement.RequestID] {
			rows, readErr := e.reader.UsageAuditByID(ctx, statement.RequestID)
			if readErr != nil {
				return result, errors.New("usage_ledger_read_failed")
			}
			for _, row := range rows {
				// Future audit pages will include the latest statement snapshot.
				if row.RowID <= state.AuditRowID && row.StatementLines > 0 {
					if err = appendRow(row); err != nil {
						return result, err
					}
				}
			}
			seen[statement.RequestID] = true
		}
		state.StatementID = statement.ID
	}
	if writer != nil && writer.Sync() != nil {
		return result, errors.New("usage_output_failed")
	}
	if e.afterAppend != nil {
		if err = e.afterAppend(); err != nil {
			return result, err
		}
	}
	if err = atomicJSON(e.config.Directory, "checkpoint.json", state); err != nil {
		return result, errors.New("usage_checkpoint_failed")
	}
	if err = e.prune(); err != nil {
		return result, err
	}
	result.AuditRowID = state.AuditRowID
	result.StatementID = state.StatementID
	result.LastSuccess = time.Now().UTC().Format(time.RFC3339Nano)
	return result, nil
}

func prepareDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return errors.New("usage_output_unavailable")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("usage_output_not_private")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("usage_output_unavailable")
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "projects" && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !(name == "checkpoint.json" || name == ".writer.lock" || strings.HasPrefix(name, ".usage-temp-")) {
			return errors.New("usage_directory_not_dedicated")
		}
	}
	projects := filepath.Join(directory, "projects")
	if err := privateDirectory(projects); err != nil {
		return err
	}
	entries, err = os.ReadDir(projects)
	if err != nil {
		return errors.New("usage_output_unavailable")
	}
	for _, entry := range entries {
		if entry.Name() != "tidemux" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("usage_directory_not_dedicated")
		}
	}
	if err := privateDirectory(logDirectory(directory)); err != nil {
		return err
	}
	_, err = listLogs(directory)
	return err
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("usage_output_unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("usage_output_not_private")
	}
	return nil
}

// Files are globally numbered so rotation/retention remain bounded across
// sessions. The session directory stays stable when its files rotate.
func listLogs(directory string) ([]logFile, error) {
	groups, err := os.ReadDir(logDirectory(directory))
	if err != nil {
		return nil, errors.New("usage_output_failed")
	}
	var logs []logFile
	for _, group := range groups {
		if !group.IsDir() || group.Type()&os.ModeSymlink != 0 || !(groupName.MatchString(group.Name()) || group.Name() == "ungrouped") {
			return nil, errors.New("usage_directory_not_dedicated")
		}
		path := filepath.Join(logDirectory(directory), group.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("usage_output_not_private")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, errors.New("usage_output_failed")
		}
		for _, entry := range entries {
			parts := logName.FindStringSubmatch(entry.Name())
			if len(parts) == 0 || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil, errors.New("usage_directory_not_dedicated")
			}
			number, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || number < 1 {
				return nil, errors.New("usage_directory_not_dedicated")
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return nil, errors.New("usage_output_not_private")
			}
			logs = append(logs, logFile{path: filepath.Join(path, entry.Name()), group: group.Name(), number: number})
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].number < logs[j].number })
	for i := 1; i < len(logs); i++ {
		if logs[i-1].number == logs[i].number {
			return nil, errors.New("usage_directory_not_dedicated")
		}
	}
	return logs, nil
}

func privateOpen(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("not a private regular file")
	}
	return f, nil
}

func (e *Exporter) readCheckpoint() (checkpoint, error) {
	s := checkpoint{Version: 2, SourceID: e.sourceID, File: 1}
	f, err := privateOpen(filepath.Join(e.config.Directory, "checkpoint.json"), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, errors.New("usage_checkpoint_invalid")
	}
	defer f.Close()
	if json.NewDecoder(io.LimitReader(f, 4096)).Decode(&s) != nil || s.Version != 2 || s.SourceID != e.sourceID || s.File < 1 || s.AuditRowID < 0 || s.StatementID < 0 {
		return s, errors.New("usage_checkpoint_invalid")
	}
	return s, nil
}

func (e *Exporter) openLog(s *checkpoint, group string, rotate bool) (*os.File, error) {
	logs, err := listLogs(e.config.Directory)
	if err != nil {
		return nil, errors.New("usage_output_failed")
	}
	var path string
	for _, log := range logs {
		if log.number > s.File {
			s.File = log.number
		}
		if log.group == group {
			path = log.path
		}
	}
	if !groupName.MatchString(group) && group != "ungrouped" {
		return nil, errors.New("usage_record_invalid")
	}
	directory := filepath.Join(logDirectory(e.config.Directory), group)
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	if path == "" || rotate {
		if len(logs) > 0 {
			s.File++
		}
		path = filepath.Join(directory, fmt.Sprintf("usage-%020d.jsonl", s.File))
	}
	f, err := privateOpen(path, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil, errors.New("usage_output_failed")
	}
	// Recover only our private output's incomplete final line. Never touch a
	// client log. Committed lines are retained, including duplicate revisions.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, errors.New("usage_output_failed")
	}
	if info.Size() > 0 {
		n := min(info.Size(), 65537)
		buf := make([]byte, int(n))
		if _, err = f.ReadAt(buf, info.Size()-n); err != nil {
			f.Close()
			return nil, errors.New("usage_output_failed")
		}
		if buf[len(buf)-1] != '\n' {
			last := bytes.LastIndexByte(buf, '\n')
			if last < 0 && n < info.Size() {
				f.Close()
				return nil, errors.New("usage_output_invalid_tail")
			}
			if err = f.Truncate(info.Size() - n + int64(last+1)); err != nil {
				f.Close()
				return nil, errors.New("usage_output_failed")
			}
		}
	}
	// Prune even after failed checkpoint persistence, keeping retries bounded.
	if err = e.prune(); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (e *Exporter) prune() error {
	logs, err := listLogs(e.config.Directory)
	if err != nil {
		return errors.New("usage_retention_failed")
	}
	for len(logs) > e.config.MaxFiles {
		path := logs[0].path
		if os.Remove(path) != nil {
			return errors.New("usage_retention_failed")
		}
		// Remove only an empty managed session directory; active groups remain.
		_ = os.Remove(filepath.Dir(path))
		logs = logs[1:]
	}
	return nil
}

func atomicJSON(directory, name string, value any) error {
	f, err := os.CreateTemp(directory, ".usage-temp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(value)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(directory, name)); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func WriteStatus(ledgerPath string, s Status) error {
	return atomicJSON(filepath.Dir(ledgerPath), filepath.Base(ledgerPath)+".usage-status.json", s)
}

func ReadStatus(ledgerPath string) (Status, error) {
	var s Status
	f, err := privateOpen(ledgerPath+".usage-status.json", os.O_RDONLY)
	if err != nil {
		return s, errors.New("usage_status_unavailable")
	}
	defer f.Close()
	if json.NewDecoder(io.LimitReader(f, 4096)).Decode(&s) != nil {
		return s, errors.New("usage_status_unavailable")
	}
	return s, nil
}

// FailureCode is safe for public diagnostics. Never forward raw OS/SQL errors.
func FailureCode(err error) string {
	if err == nil {
		return ""
	}
	code := err.Error()
	switch code {
	case "usage_identity_unavailable", "usage_ledger_unavailable", "usage_output_unavailable", "usage_output_not_private", "usage_directory_not_dedicated", "usage_export_in_use", "usage_checkpoint_invalid", "usage_ledger_read_failed", "usage_record_invalid", "usage_output_failed", "usage_checkpoint_failed", "usage_retention_failed", "usage_output_invalid_tail":
		return code
	default:
		return "usage_export_failed"
	}
}
