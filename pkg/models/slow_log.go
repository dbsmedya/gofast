package models

import (
	"time"
)

// FileInfo represents information about a parsed file (legacy shape used by stats UIs).
type FileInfo struct {
	Hash       string    `json:"hash" db:"hash"`
	FilePath   string    `json:"file_path" db:"file_path"`
	FileSize   int64     `json:"file_size" db:"file_size"`
	FileMtime  time.Time `json:"file_mtime" db:"file_mtime"`
	FirstSeen  time.Time `json:"first_seen_at" db:"first_seen_at"`
	ParseCount int       `json:"parse_count" db:"parse_count"`
}

// FileState is the per-generation resume/continuity state for a slow-log file.
type FileState struct {
	Hash          string     `json:"hash" db:"hash"`
	FilePath      string     `json:"file_path" db:"file_path"`
	Generation    int64      `json:"generation" db:"generation"`
	FileSize      int64      `json:"file_size" db:"file_size"`
	FileMtime     time.Time  `json:"file_mtime" db:"file_mtime"`
	FirstSeen     time.Time  `json:"first_seen_at" db:"first_seen_at"`
	ParseCount    int        `json:"parse_count" db:"parse_count"`
	LastParsedAt  *time.Time `json:"last_parsed_at,omitempty" db:"last_parsed_at"`
	PrefixHash    string     `json:"prefix_hash" db:"prefix_hash"`
	PrefixLen     int64      `json:"prefix_len" db:"prefix_len"`
	TailHash      string     `json:"tail_hash" db:"tail_hash"`
	TailLen       int64      `json:"tail_len" db:"tail_len"`
	ResumeOffset  int64      `json:"resume_offset" db:"resume_offset"`
	CompletedSize int64      `json:"completed_size" db:"completed_size"`
}

// SlowLogEntry represents a parsed MySQL slow log entry
type SlowLogEntry struct {
	ID            string    `json:"id" db:"id"`
	FileHash      string    `json:"file_hash" db:"file_hash"` // Reference to files.hash (generation identity)
	FingerprintID string    `json:"fingerprint_id" db:"fingerprint_id"`
	SanitizedSQL  string    `json:"sanitized_sql" db:"sanitized_sql"`
	SampleSQL     string    `json:"sample_sql" db:"sample_sql"`
	User          string    `json:"user" db:"user"`
	Host          string    `json:"host" db:"host"`
	DB            string    `json:"db" db:"db"`
	QueryTimeSec  float64   `json:"query_time_sec" db:"query_time_sec"`
	LockTimeSec   float64   `json:"lock_time_sec" db:"lock_time_sec"`
	RowsSent      uint64    `json:"rows_sent" db:"rows_sent"`
	RowsExamined  uint64    `json:"rows_examined" db:"rows_examined"`
	TS            time.Time `json:"ts" db:"ts"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	Tables        []string  `json:"tables" db:"tables"`
	EventOffset   int64     `json:"event_offset" db:"event_offset"`
	EventHash     string    `json:"event_hash" db:"event_hash"`
}

// ParseJob represents a parsing job
type ParseJob struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"` // pending, running, completed, failed
	LogDir           string     `json:"log_dir"`
	Files            []string   `json:"files"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	Error            string     `json:"error,omitempty"`
	EntriesProcessed int        `json:"entries_processed"`
}

// ReportQuery represents a query to the report endpoint
type ReportQuery struct {
	SQL string `json:"sql" binding:"required"`
}

// ReportResult represents the result of a report query
type ReportResult struct {
	Columns []string        `json:"columns"`
	Rows    [][]interface{} `json:"rows"`
	Count   int             `json:"count"`
}
