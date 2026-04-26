package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestFilePersisterWriteAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	p, err := NewFilePersister(path)
	if err != nil {
		t.Fatalf("NewFilePersister: %v", err)
	}

	rec := PersistenceRecord{
		RequestedAt:  "2026-03-20T12:00:00Z",
		APIKey:       "sk-test",
		Model:        "gpt-5",
		Provider:     "openai",
		TotalTokens:  42,
		InputTokens:  10,
		OutputTokens: 32,
	}

	if err := p.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var decoded PersistenceRecord
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v. Content: %s", err, string(content))
	}
	if decoded.APIKey != "sk-test" || decoded.TotalTokens != 42 {
		t.Fatalf("decoded = %+v, want sk-test / 42", decoded)
	}
}

func TestReplayRebuildsStatistics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	p, err := NewFilePersister(path)
	if err != nil {
		t.Fatalf("NewFilePersister: %v", err)
	}

	for i := 0; i < 3; i++ {
		pr := PersistenceRecord{
			RequestedAt: "2026-03-20T12:00:00Z",
			APIKey:      "sk-test",
			Model:       "gpt-5",
			Provider:    "openai",
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		}
		if err := p.Write(pr); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stats := NewRequestStatistics()
	count, err := Replay(path, stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	snap := stats.Snapshot()
	if snap.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", snap.TotalRequests)
	}
	if snap.TotalTokens != 90 {
		t.Fatalf("TotalTokens = %d, want 90", snap.TotalTokens)
	}
}

func TestReplayFileNotExists(t *testing.T) {
	stats := NewRequestStatistics()
	count, err := Replay("/nonexistent/path/file.jsonl", stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestReplayEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stats := NewRequestStatistics()
	count, err := Replay(path, stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestReplaySkipsEmptyLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := []byte("\n\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stats := NewRequestStatistics()
	count, err := Replay(path, stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestReplaySkipsUnparseableLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	pr := PersistenceRecord{
		RequestedAt: "2026-03-20T12:00:00Z",
		APIKey:      "sk-valid",
		Model:       "gpt-5",
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}
	validJSON, _ := json.Marshal(pr)
	content := append(validJSON, '\n')
	content = append(content, []byte("not valid json\n")...)
	content = append(content, validJSON...)
	content = append(content, '\n')

	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stats := NewRequestStatistics()
	count, err := Replay(path, stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (one line skipped)", count)
	}
}

func TestReplayDoesNotPersistDuringReplay(t *testing.T) {
	// Ensure that replay does not in turn call persistRecord and write
	// duplicate entries to the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	p, err := NewFilePersister(path)
	if err != nil {
		t.Fatalf("NewFilePersister: %v", err)
	}
	pr := PersistenceRecord{
		RequestedAt: "2026-03-20T12:00:00Z",
		APIKey:      "sk-test",
		Model:       "gpt-5",
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}
	if err := p.Write(pr); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Set up a global persister so persistRecord would write if not guarded.
	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "replay-out.jsonl")
	tmpPersister, err := NewFilePersister(tmpPath)
	if err != nil {
		t.Fatalf("NewFilePersister: %v", err)
	}
	oldPersister := persister
	persister = tmpPersister
	defer func() {
		persister.Close()
		persister = oldPersister
	}()

	stats := NewRequestStatistics()
	count, err := Replay(path, stats)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	if err := tmpPersister.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The replay output file should be empty — replay should not persist.
	replayOut, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(replayOut) != 0 {
		t.Fatalf("replay persisted %d bytes unexpectedly: %s", len(replayOut), string(replayOut))
	}
}

func TestPersistRecordNoOpWithNilPersister(t *testing.T) {
	// No panic when persister is nil.
	persistRecord(coreusage.Record{
		APIKey: "sk-test",
		Model:  "gpt-5",
		Detail: coreusage.Detail{TotalTokens: 100},
	})
}

func TestInitCloseUsagePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	if err := InitUsagePersistence(path); err != nil {
		t.Fatalf("InitUsagePersistence: %v", err)
	}
	if persister == nil {
		t.Fatal("persister is nil after InitUsagePersistence")
	}

	// Write a record through the global persister
	pr := PersistenceRecord{
		RequestedAt: "2026-03-20T12:00:00Z",
		APIKey:      "sk-test",
		Model:       "gpt-5",
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}
	if err := persister.Write(pr); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := CloseUsagePersistence(); err != nil {
		t.Fatalf("CloseUsagePersistence: %v", err)
	}

	// Verify the content
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("file is empty after write")
	}
}

func TestInitUsagePersistenceEmptyPath(t *testing.T) {
	if err := InitUsagePersistence(""); err != nil {
		t.Fatalf("InitUsagePersistence: %v", err)
	}
	if persister != nil {
		t.Fatal("persister is not nil after empty path init")
	}
}

func TestRecordPersistsToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	if err := InitUsagePersistence(path); err != nil {
		t.Fatalf("InitUsagePersistence: %v", err)
	}
	defer CloseUsagePersistence()

	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-test",
		Model:       "gpt-5",
		Provider:    "openai",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	if err := CloseUsagePersistence(); err != nil {
		t.Fatalf("CloseUsagePersistence: %v", err)
	}

	// Verify file content
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("file is empty after Record() + persist")
	}

	var decoded PersistenceRecord
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v. Content: %s", err, string(content))
	}
	if decoded.APIKey != "sk-test" {
		t.Fatalf("APIKey = %s, want sk-test", decoded.APIKey)
	}
	if decoded.TotalTokens != 30 {
		t.Fatalf("TotalTokens = %d, want 30", decoded.TotalTokens)
	}
	if decoded.LatencyMs != 1500 {
		t.Fatalf("LatencyMs = %d, want 1500", decoded.LatencyMs)
	}
}

func TestReplayRestoresFullStatistics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	// Phase 1: Write some records, then Close
	if err := InitUsagePersistence(path); err != nil {
		t.Fatalf("InitUsagePersistence: %v", err)
	}

	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-a",
		Model:       "m1",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{TotalTokens: 100, InputTokens: 50, OutputTokens: 50},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-b",
		Model:       "m2",
		RequestedAt: time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{TotalTokens: 200, InputTokens: 100, OutputTokens: 100},
		Failed:      true,
	})

	if err := CloseUsagePersistence(); err != nil {
		t.Fatalf("CloseUsagePersistence: %v", err)
	}

	// Phase 2: Replay into a fresh stats instance
	fresh := NewRequestStatistics()
	count, err := Replay(path, fresh)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	snap := fresh.Snapshot()
	if snap.TotalRequests != 2 {
		t.Fatalf("TotalRequests = %d, want 2", snap.TotalRequests)
	}
	if snap.TotalTokens != 300 {
		t.Fatalf("TotalTokens = %d, want 300", snap.TotalTokens)
	}
	if snap.SuccessCount != 1 {
		t.Fatalf("SuccessCount = %d, want 1", snap.SuccessCount)
	}
	if snap.FailureCount != 1 {
		t.Fatalf("FailureCount = %d, want 1", snap.FailureCount)
	}

	apiA := snap.APIs["sk-a"]
	if apiA.TotalTokens != 100 {
		t.Fatalf("sk-a TotalTokens = %d, want 100", apiA.TotalTokens)
	}

	apiB := snap.APIs["sk-b"]
	if apiB.TotalTokens != 200 {
		t.Fatalf("sk-b TotalTokens = %d, want 200", apiB.TotalTokens)
	}

	if _, ok := snap.RequestsByDay["2026-03-20"]; !ok {
		t.Fatal("RequestsByDay missing 2026-03-20")
	}
	if snap.RequestsByHour["12"] != 1 {
		t.Fatalf("RequestsByHour[12] = %d, want 1", snap.RequestsByHour["12"])
	}
	if snap.RequestsByHour["14"] != 1 {
		t.Fatalf("RequestsByHour[14] = %d, want 1", snap.RequestsByHour["14"])
	}
}
