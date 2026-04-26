package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var (
	replaying atomic.Bool
	persister *FilePersister
)

// PersistenceRecord is the flat JSON-serializable record written to the JSONL file.
type PersistenceRecord struct {
	RequestedAt     string `json:"requested_at"`
	APIKey          string `json:"api_key"`
	Model           string `json:"model"`
	Provider        string `json:"provider"`
	Source          string `json:"source"`
	AuthIndex       string `json:"auth_index"`
	LatencyMs       int64  `json:"latency_ms"`
	Failed          bool   `json:"failed"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

// FilePersister appends JSON lines to a file.
type FilePersister struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
}

// NewFilePersister creates the parent directory (if needed) and opens the file for append.
func NewFilePersister(path string) (*FilePersister, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	return &FilePersister{
		file:   f,
		writer: bufio.NewWriter(f),
	}, nil
}

// Write encodes rec as JSON and appends it as a single line.
func (p *FilePersister) Write(rec PersistenceRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := p.writer.Write(append(data, '\n')); err != nil {
		return err
	}
	return p.writer.Flush()
}

// Close flushes and closes the underlying file.
func (p *FilePersister) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.writer.Flush(); err != nil {
		_ = p.file.Close()
		return err
	}
	return p.file.Close()
}

// Replay reads the JSONL file at path and feeds each record into stats.
// Returns the number of records replayed.
func Replay(path string, stats *RequestStatistics) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	replaying.Store(true)
	defer replaying.Store(false)

	var count int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var pr PersistenceRecord
		if err := json.Unmarshal(line, &pr); err != nil {
			log.Warnf("usage persistence: skipping unparseable line: %v", err)
			continue
		}

		requestedAt, err := time.Parse(time.RFC3339Nano, pr.RequestedAt)
		if err != nil {
			requestedAt = time.Now()
		}

		rec := coreusage.Record{
			Provider:    pr.Provider,
			Model:       pr.Model,
			APIKey:      pr.APIKey,
			Source:      pr.Source,
			AuthIndex:   pr.AuthIndex,
			RequestedAt: requestedAt,
			Latency:     time.Duration(pr.LatencyMs) * time.Millisecond,
			Failed:      pr.Failed,
			Detail: coreusage.Detail{
				InputTokens:     pr.InputTokens,
				OutputTokens:    pr.OutputTokens,
				ReasoningTokens: pr.ReasoningTokens,
				CachedTokens:    pr.CachedTokens,
				TotalTokens:     pr.TotalTokens,
			},
		}
		stats.Record(context.Background(), rec)
		count++
	}
	if err := scanner.Err(); err != nil {
		return count, err
	}
	return count, nil
}

// InitUsagePersistence creates the persister, replays existing data, and stores the global reference.
func InitUsagePersistence(path string) error {
	if path == "" {
		return nil
	}

	p, err := NewFilePersister(path)
	if err != nil {
		return err
	}

	count, err := Replay(path, defaultRequestStatistics)
	if err != nil {
		_ = p.Close()
		return err
	}
	if count > 0 {
		log.Infof("usage persistence: replayed %d records from %s", count, path)
	}

	persister = p
	return nil
}

// CloseUsagePersistence closes the global persister.
func CloseUsagePersistence() error {
	if persister == nil {
		return nil
	}
	p := persister
	persister = nil
	return p.Close()
}

func persistRecord(record coreusage.Record) {
	if replaying.Load() || persister == nil {
		return
	}

	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}

	latencyMs := int64(0)
	if record.Latency > 0 {
		latencyMs = record.Latency.Milliseconds()
	}

	pr := PersistenceRecord{
		RequestedAt:     requestedAt.Format(time.RFC3339Nano),
		APIKey:          record.APIKey,
		Model:           record.Model,
		Provider:        record.Provider,
		Source:          record.Source,
		AuthIndex:       record.AuthIndex,
		LatencyMs:       latencyMs,
		Failed:          record.Failed,
		InputTokens:     record.Detail.InputTokens,
		OutputTokens:    record.Detail.OutputTokens,
		ReasoningTokens: record.Detail.ReasoningTokens,
		CachedTokens:    record.Detail.CachedTokens,
		TotalTokens:     record.Detail.TotalTokens,
	}

	if err := persister.Write(pr); err != nil {
		log.Warnf("usage persistence: failed to write record: %v", err)
	}
}
