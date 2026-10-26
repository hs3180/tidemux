// Package contentlog provides an opt-in, bounded local output for external
// collectors. It never opens a network connection or removes content files.
package contentlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Both base64 data and a worst-case JSON-escaped search text fit in 64 KiB.
const ChunkBytes = 8 << 10
const MaxEventBytes = 64 << 10
const maxDirectoryEntries = 4096

type Config struct {
	Enabled        bool   `json:"enabled,omitempty"`
	Directory      string `json:"directory,omitempty"`
	FileBytes      int64  `json:"file_bytes,omitempty"`
	DirectoryBytes int64  `json:"directory_bytes,omitempty"`
	QueueEvents    int    `json:"queue_events,omitempty"`
}

func (c Config) Effective() Config {
	if c.FileBytes == 0 {
		c.FileBytes = 8 << 20
	}
	if c.DirectoryBytes == 0 {
		c.DirectoryBytes = 256 << 20
	}
	if c.QueueEvents == 0 {
		c.QueueEvents = 256
	}
	return c
}

func (c Config) Validate() error {
	if c == (Config{}) {
		return nil
	}
	c = c.Effective()
	if !filepath.IsAbs(c.Directory) || c.FileBytes < MaxEventBytes || c.DirectoryBytes < c.FileBytes || c.QueueEvents < 1 || c.QueueEvents > 4096 {
		return errors.New("content_output requires an absolute directory, file_bytes >= 65536, directory_bytes >= file_bytes and queue_events in 1..4096")
	}
	return nil
}

// Log has one disk writer. Producers never wait for it; a full queue drops
// events and increments content-free counters. Finish cannot promise delivery.
type Log struct {
	config   Config
	logger   *slog.Logger
	queue    chan packet
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	lock     *os.File
	file     *os.File
	size     int64
	used     int64
	dropped  atomic.Uint64
	failures atomic.Uint64
	written  atomic.Uint64
	warning  atomic.Int64
}

type packet struct {
	data     []byte
	gap      *atomic.Bool
	terminal *Event
}

func Open(c Config, logger *slog.Logger) (*Log, error) {
	if c == (Config{}) {
		return nil, nil
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c = c.Effective()
	// The directory must be dedicated, private and not a symlink. Do not
	// repair permissions or recursively create arbitrary parent directories.
	if err := os.Mkdir(c.Directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.New("cannot prepare content output directory")
	}
	info, err := os.Lstat(c.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("content output directory must be private")
	}
	lock, err := os.OpenFile(filepath.Join(c.Directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("cannot lock content output directory")
	}
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, errors.New("content output directory lock unavailable")
	}
	l := &Log{config: c, logger: logger, queue: make(chan packet, c.QueueEvents), done: make(chan struct{}), lock: lock}
	if err := l.scan(true); err != nil {
		lock.Close()
		return nil, errors.New("cannot inspect content output files")
	}
	go l.run()
	return l, nil
}

func (l *Log) Status() map[string]uint64 {
	if l == nil {
		return map[string]uint64{}
	}
	return map[string]uint64{"written_events": l.written.Load(), "dropped_events": l.dropped.Load(), "write_failures": l.failures.Load(), "queued_events": uint64(len(l.queue))}
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()
	<-l.done
	return nil
}

func (l *Log) warn() {
	now := time.Now().Unix()
	previous := l.warning.Load()
	if now-previous >= 60 && l.warning.CompareAndSwap(previous, now) && l.logger != nil {
		l.logger.Warn("content output has gaps", slog.Int("schema_version", 2), slog.String("event", "content_output_gap"),
			slog.Uint64("dropped_events", l.dropped.Load()), slog.Uint64("write_failures", l.failures.Load()))
	}
}

func (l *Log) enqueue(e Event, gap *atomic.Bool) bool {
	data, err := json.Marshal(e)
	if err != nil || len(data)+1 > MaxEventBytes {
		l.dropped.Add(1)
		return false
	}
	data = append(data, '\n')
	p := packet{data: data, gap: gap}
	if e.Complete != nil {
		p.terminal = &e
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.closed {
		select {
		case l.queue <- p:
			return true
		default:
		}
	}
	l.dropped.Add(1)
	return false
}

func (l *Log) run() {
	defer close(l.done)
	defer l.lock.Close()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		var p packet
		select {
		case <-ticker.C:
			if err := l.seal(); err != nil {
				l.failures.Add(1)
				l.warn()
			}
			if l.dropped.Load() > 0 {
				l.warn()
			}
			continue
		case next, ok := <-l.queue:
			if !ok {
				if err := l.seal(); err != nil {
					l.failures.Add(1)
					l.warn()
				}
				return
			}
			p = next
		}
		if p.terminal != nil && p.gap.Load() {
			*p.terminal.Complete = false
			p.data, _ = json.Marshal(p.terminal)
			p.data = append(p.data, '\n')
		}
		if err := l.write(p.data); err != nil {
			p.gap.Store(true)
			l.failures.Add(1)
			l.warn()
		} else {
			l.written.Add(1)
		}
		if l.dropped.Load() > 0 {
			l.warn()
		}
	}
}

func contentFile(name string) bool {
	return strings.HasPrefix(name, "content-") && (strings.HasSuffix(name, ".active") || strings.HasSuffix(name, ".jsonl"))
}

// Leftover active files are handed off as interrupted, never resumed. The
// terminal event and hashes, rather than a filename, determine completeness.
func (l *Log) scan(recoverActive bool) error {
	directory, err := os.Open(l.config.Directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	var used int64
	var interrupted []string
	count := 0
	for {
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		count += len(entries)
		if count >= maxDirectoryEntries {
			return errors.New("content output file limit reached")
		}
		for _, entry := range entries {
			if !contentFile(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("unsafe content file")
			}
			used += info.Size()
			if recoverActive && strings.HasSuffix(entry.Name(), ".active") {
				interrupted = append(interrupted, filepath.Join(l.config.Directory, entry.Name()))
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	for _, path := range interrupted {
		target := strings.TrimSuffix(path, ".active") + ".interrupted.jsonl"
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("interrupted content target exists")
		}
		if err := os.Rename(path, target); err != nil {
			return err
		}
	}
	l.used = used
	return nil
}

func (l *Log) seal() error {
	if l.file == nil {
		return nil
	}
	f := l.file
	l.file, l.size = nil, 0
	// A sync/close failure leaves .active, so file-copy tools do not mistake
	// it for a successful handoff. It can be recovered on the next startup.
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("content file close failed")
	}
	return os.Rename(f.Name(), strings.TrimSuffix(f.Name(), ".active")+".jsonl")
}

func (l *Log) write(data []byte) error {
	if l.file != nil && l.size+int64(len(data)) > l.config.FileBytes {
		if err := l.seal(); err != nil {
			return err
		}
	}
	if l.file == nil || l.used+int64(len(data)) > l.config.DirectoryBytes {
		if err := l.scan(false); err != nil {
			return err
		}
	}
	if l.used+int64(len(data)) > l.config.DirectoryBytes {
		return errors.New("content output capacity reached")
	}
	if l.file == nil {
		f, err := os.CreateTemp(l.config.Directory, "content-"+time.Now().UTC().Format("20060102T150405.000Z")+"-*.active")
		if err != nil {
			return err
		}
		l.file = f
	}
	n, err := l.file.Write(data)
	l.size += int64(n)
	l.used += int64(n)
	if err != nil || n != len(data) {
		// Never append another event behind a partial JSON line.
		l.file.Close()
		l.file, l.size = nil, 0
		return io.ErrShortWrite
	}
	return nil
}

type Metadata struct {
	RequestID        string `json:"requestId"`
	SessionID        string `json:"sessionId,omitempty"`
	Protocol         string `json:"protocol"`
	ProviderProtocol string `json:"provider_protocol"`
	Provider         string `json:"provider_ref"`
	Model            string `json:"model"`
}

type Event struct {
	Metadata
	SchemaVersion int    `json:"schema_version"`
	EventID       string `json:"event_id"`
	Timestamp     string `json:"timestamp"`
	Event         string `json:"event"`
	View          string `json:"view"`
	Seq           uint64 `json:"seq"`
	Direction     string `json:"direction,omitempty"`
	Offset        int64  `json:"offset,omitempty"`
	Data          []byte `json:"data,omitempty"` // Canonical, base64-encoded bytes.
	Text          string `json:"text,omitempty"` // Search convenience; never parsed into ES fields.
	Outcome       string `json:"outcome,omitempty"`
	Complete      *bool  `json:"complete,omitempty"`
	InputBytes    int64  `json:"input_bytes,omitempty"`
	OutputBytes   int64  `json:"output_bytes,omitempty"`
	InputSHA256   string `json:"input_sha256,omitempty"`
	OutputSHA256  string `json:"output_sha256,omitempty"`
}

// Capture is owned by one HTTP handler. Hashes cover all observed bytes, even
// those whose events were dropped. Consumers must check offsets/counts/hashes
// and a successful terminal event; enqueue success is not a persistence receipt.
type Capture struct {
	log                     *Log
	metadata                Metadata
	seq                     uint64
	input, output           hash.Hash
	inputBytes, outputBytes int64
	gap                     atomic.Bool
}

func (l *Log) Start(metadata Metadata, input []byte) *Capture {
	if l == nil {
		return nil
	}
	c := &Capture{log: l, metadata: metadata, input: sha256.New(), output: sha256.New()}
	c.chunks("input", input)
	return c
}

func (c *Capture) emit(e Event) {
	e.Metadata, e.SchemaVersion, e.View = c.metadata, 1, "client"
	e.Seq = c.seq
	e.EventID = c.metadata.RequestID + ":" + strconv.FormatUint(c.seq, 10)
	e.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	c.seq++
	if !c.log.enqueue(e, &c.gap) {
		c.gap.Store(true)
	}
}

func (c *Capture) chunks(direction string, data []byte) {
	count, digest := &c.inputBytes, c.input
	if direction == "output" {
		count, digest = &c.outputBytes, c.output
	}
	for len(data) > 0 {
		n := min(len(data), ChunkBytes)
		part := data[:n]
		digest.Write(part)
		c.emit(Event{Event: "content_chunk", Direction: direction, Offset: *count, Data: part, Text: string(part)})
		*count += int64(n)
		data = data[n:]
	}
}

func (c *Capture) Write(data []byte) {
	if c != nil {
		c.chunks("output", data)
	}
}

func (c *Capture) Finish(outcome string, complete bool) {
	if c == nil {
		return
	}
	complete = complete && !c.gap.Load() && outcome == "success"
	c.emit(Event{Event: "content_end", Outcome: outcome, Complete: &complete,
		InputBytes: c.inputBytes, OutputBytes: c.outputBytes,
		InputSHA256: hex.EncodeToString(c.input.Sum(nil)), OutputSHA256: hex.EncodeToString(c.output.Sum(nil))})
}
