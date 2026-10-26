package contentlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// This consumer knows only the on-disk contract. Replays are deduplicated by
// event_id, and neither filenames nor arrival order supply request ordering.
func consume(t *testing.T, directory string) map[string][]Event {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "content-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	requests := map[string][]Event{}
	for replay := 0; replay < 2; replay++ {
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("file permissions: %v %v", info, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s := bufio.NewScanner(bytes.NewReader(data))
			s.Buffer(make([]byte, MaxEventBytes), MaxEventBytes)
			for s.Scan() {
				if len(s.Bytes())+1 > MaxEventBytes {
					t.Fatal("collector event limit exceeded")
				}
				var event Event
				if err := json.Unmarshal(s.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if !seen[event.EventID] {
					seen[event.EventID] = true
					requests[event.RequestID] = append(requests[event.RequestID], event)
				}
			}
			if err := s.Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, events := range requests {
		sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	}
	return requests
}

func verify(t *testing.T, events []Event, input, output []byte, complete bool) {
	t.Helper()
	var in, out []byte
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for i, e := range events {
		if e.Seq != uint64(i) || e.EventID == "" || e.View != "client" || e.SchemaVersion != 1 {
			t.Fatalf("event envelope/sequence: %+v", e)
		}
		if e.Event == "content_chunk" {
			data := &in
			if e.Direction == "output" {
				data = &out
			}
			if e.Offset != int64(len(*data)) {
				t.Fatal("content offset gap")
			}
			*data = append(*data, e.Data...)
		}
	}
	end := events[len(events)-1]
	digest := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
	if end.Event != "content_end" || end.Complete == nil || *end.Complete != complete || end.InputBytes != int64(len(input)) || end.OutputBytes != int64(len(output)) || end.InputSHA256 != digest(input) || end.OutputSHA256 != digest(output) || !bytes.Equal(in, input) || !bytes.Equal(out, output) {
		t.Fatalf("reconstruction/completeness mismatch: end=%+v bytes=%d/%d", end, len(in), len(out))
	}
}

func TestChunkReconstructionRotationReplayAndConcurrency(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "content")
	l, err := Open(Config{Enabled: true, Directory: directory, FileBytes: MaxEventBytes, DirectoryBytes: 16 << 20, QueueEvents: 4096}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(strings.Repeat("系统\\\"\n\x00🙂", 10000))
	output := append([]byte(strings.Repeat("data: 工具结果\n\n", 5000)), 0xff, 0x00)
	var wg sync.WaitGroup
	for _, id := range []string{"request-a", "request-b"} {
		wg.Go(func() {
			c := l.Start(Metadata{RequestID: id, Protocol: "anthropic", ProviderProtocol: "openai", Provider: "fixture", Model: "model"}, input)
			for pos := 0; pos < len(output); pos += 97 {
				c.Write(output[pos:min(pos+97, len(output))])
			}
			c.Finish("success", true)
		})
	}
	wg.Wait()
	l.Close()
	if l.Status()["write_failures"] != 0 || l.Status()["dropped_events"] != 0 {
		t.Fatalf("unexpected loss: %v", l.Status())
	}
	requests := consume(t, directory)
	if len(requests) != 2 {
		t.Fatal("lost concurrent request")
	}
	for _, events := range requests {
		verify(t, events, input, output, true)
	}
	paths, _ := filepath.Glob(filepath.Join(directory, "content-*.jsonl"))
	if len(paths) < 2 {
		t.Fatal("rotation was not exercised")
	}
	for _, path := range paths {
		info, _ := os.Stat(path)
		if info.Size() > MaxEventBytes {
			t.Fatal("file size cap exceeded")
		}
	}
}

func TestQueueOverflowDoesNotBlockAndMarksTerminalPartial(t *testing.T) {
	// Deliberately stopped disk worker represents a slow/unavailable output.
	l := &Log{queue: make(chan packet, 1)}
	c := l.Start(Metadata{RequestID: "overload"}, []byte("input"))
	c.Write(bytes.Repeat([]byte("x"), ChunkBytes*2))
	if l.dropped.Load() != 2 {
		t.Fatal("queue did not enforce its bound")
	}
	<-l.queue
	c.Finish("success", true)
	var end Event
	if err := json.Unmarshal((<-l.queue).data, &end); err != nil || end.Complete == nil || *end.Complete {
		t.Fatalf("overflow incorrectly complete: %+v %v", end, err)
	}
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("output worker did not reach expected state")
}

func TestQuotaFailureRecoversWithoutDeletingHistoryAndCannotClaimComplete(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "content")
	l, err := Open(Config{Enabled: true, Directory: directory, FileBytes: MaxEventBytes, DirectoryBytes: MaxEventBytes}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := l.Start(Metadata{RequestID: "full"}, bytes.Repeat([]byte{0}, ChunkBytes*2))
	waitFor(t, func() bool { return l.failures.Load() > 0 })
	paths, _ := filepath.Glob(filepath.Join(directory, "content-*.jsonl"))
	if len(paths) == 0 {
		t.Fatal("quota pressure removed existing data")
	}
	// Only an external, confirmed-delivery policy may remove closed files.
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	c.Finish("success", true)
	l.Close()
	events := consume(t, directory)["full"]
	if len(events) != 1 || events[0].Event != "content_end" || events[0].Complete == nil || *events[0].Complete {
		t.Fatalf("disk loss falsely complete: %+v", events)
	}
}

func TestDefaultPrivacyLockAndInterruptedFiles(t *testing.T) {
	if l, err := Open(Config{}, nil); l != nil || err != nil {
		t.Fatal("default output must be absent")
	}
	directory := filepath.Join(t.TempDir(), "content")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: directory}
	if _, err := Open(config, nil); err == nil {
		t.Fatal("public directory accepted")
	}
	os.Chmod(directory, 0700)
	partial := filepath.Join(directory, "content-interrupted.active")
	os.WriteFile(partial, []byte("{\"partial\":"), 0600)
	l, err := Open(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(config, nil); other != nil || err == nil {
		t.Fatal("concurrent output directory owner accepted")
	}
	l.Close()
	data, err := os.ReadFile(strings.TrimSuffix(partial, ".active") + ".interrupted.jsonl")
	if err != nil || string(data) != "{\"partial\":" {
		t.Fatal("restart changed or discarded interrupted bytes")
	}
	for _, target := range []string{"directory", "lock"} {
		t.Run("symlink-"+target, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "output")
			if target == "directory" {
				os.Symlink(directory, path)
			} else {
				os.Mkdir(path, 0700)
				os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(path, ".lock"))
			}
			if got, err := Open(Config{Directory: path}, nil); got != nil || err == nil {
				t.Fatal("unsafe filesystem target accepted")
			}
		})
	}
}
