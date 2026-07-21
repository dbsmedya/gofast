package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	duckdb "github.com/marcboeker/go-duckdb/v2"
	"github.com/dbsmedya/gofast/pkg/models"
)

// Storage handles DuckDB operations
type Storage struct {
	db        *sql.DB
	connector *duckdb.Connector
	readOnly  bool
}

// duckdbSecurityOpts hardens the DuckDB engine on every connection, applied as
// DSN options per DuckDB's security guide
// (https://duckdb.org/docs/operations_manual/securing_duckdb/overview).
// gofast never reaches the filesystem or network from SQL — ingestion uses the
// Go appender API and no extensions are loaded — so these cost nothing and
// close an exfiltration path that access_mode=read_only does NOT: without them,
// DuckDB SQL can read and write local files (read_csv, COPY ... TO) and install
// extensions even when the store is opened read-only.
//
//   - enable_external_access=false blocks read_csv/read_parquet/COPY/ATTACH/INSTALL.
//   - allow_community_extensions=false blocks third-party extension installs.
//   - lock_configuration=true (kept LAST) makes the above tamper-proof for the
//     database instance: no later SET can re-enable them. go-duckdb records all
//     DSN options into one config object applied atomically at open, so the lock
//     never blocks its sibling options here.
//
// disabled_filesystems='LocalFileSystem' is deliberately NOT included: it cannot
// be passed via the DSN and it breaks the database's own file writes/WAL and
// temp-spill for large queries. enable_external_access=false already blocks the
// SQL file functions, which is the actual threat.
const duckdbSecurityOpts = "allow_community_extensions=false&enable_external_access=false&lock_configuration=true"

// NewStorage creates a new Storage instance
// When readOnly is true, opens the database in read-only mode (no writes allowed)
// When readOnly is false, opens with read-write access and initializes schema
func NewStorage(dsn string, readOnly bool) (*Storage, error) {
	connectionDSN := dsn + "?" + duckdbSecurityOpts
	if readOnly {
		connectionDSN = dsn + "?access_mode=read_only&" + duckdbSecurityOpts
	}

	connector, err := duckdb.NewConnector(connectionDSN, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create duckdb connector: %w", err)
	}

	db := sql.OpenDB(connector)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		_ = connector.Close()
		return nil, fmt.Errorf("failed to ping duckdb: %w", err)
	}

	s := &Storage{db: db, connector: connector, readOnly: readOnly}

	if !readOnly {
		if err := s.initSchema(); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("failed to initialize schema: %w", err)
		}
	}

	return s, nil
}

// Close closes the database connection and connector
func (s *Storage) Close() error {
	err := s.db.Close()
	if s.connector != nil {
		if cerr := s.connector.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// initSchema creates the necessary tables and indexes
func (s *Storage) initSchema() error {
	if err := s.createTables(); err != nil {
		return err
	}
	// sanitized_sql can hold queries larger than DuckDB's 120 KiB ART key limit.
	if _, err := s.db.Exec("DROP INDEX IF EXISTS idx_sanitized_sql"); err != nil {
		return fmt.Errorf("failed to drop legacy idx_sanitized_sql: %w", err)
	}
	return s.CreateSlowLogIndexes()
}

// createTables creates tables and sequences without indexes on slow_logs.
// Greenfield schema [L9]: full post-Phase-2 column set.
func (s *Storage) createTables() error {
	schema := `
	CREATE SEQUENCE IF NOT EXISTS seq_slow_log_id START 1;

	CREATE TABLE IF NOT EXISTS files (
		hash VARCHAR(16) PRIMARY KEY,
		file_path VARCHAR NOT NULL,
		generation INTEGER NOT NULL DEFAULT 1,
		file_size BIGINT NOT NULL DEFAULT 0,
		file_mtime TIMESTAMP,
		first_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		parse_count INTEGER DEFAULT 0,
		last_parsed_at TIMESTAMP,
		prefix_hash VARCHAR(16),
		prefix_len BIGINT NOT NULL DEFAULT 0,
		tail_hash VARCHAR(16),
		tail_len BIGINT NOT NULL DEFAULT 0,
		resume_offset BIGINT NOT NULL DEFAULT 0,
		completed_size BIGINT NOT NULL DEFAULT 0,
		UNIQUE (file_path, generation)
	);

	CREATE INDEX IF NOT EXISTS idx_files_path ON files(file_path);

	CREATE TABLE IF NOT EXISTS slow_logs (
		id BIGINT PRIMARY KEY DEFAULT nextval('seq_slow_log_id'),
		file_hash VARCHAR(16),
		fingerprint_id VARCHAR(16) NOT NULL,
		sanitized_sql VARCHAR NOT NULL,
		sample_sql VARCHAR NOT NULL,
		user VARCHAR,
		host VARCHAR,
		db VARCHAR,
		query_time_sec DOUBLE,
		lock_time_sec DOUBLE,
		rows_sent UBIGINT,
		rows_examined UBIGINT,
		ts TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		tables VARCHAR[],
		event_offset BIGINT,
		event_hash VARCHAR(16)
	);

	-- Read-only enrichment view: derives the source DB-server hostname from the
	-- ingested slow-log filename (basename with trailing .log/.slow/.gz stripped),
	-- joined from the files table. Single source of truth for source_host; works
	-- retroactively for all existing rows. Writes still target slow_logs directly.
	CREATE OR REPLACE VIEW slow_logs_with_source AS
	SELECT s.*,
	       regexp_replace(
	         regexp_replace(f.file_path, '^.*[/\\]', ''),
	         '(\.(log|slow|gz))+$', '', 'i') AS source_host
	FROM slow_logs s
	LEFT JOIN files f ON s.file_hash = f.hash;
	`
	_, err := s.db.Exec(schema)
	return err
}

// DropSlowLogIndexes drops all indexes on slow_logs for faster bulk inserts.
func (s *Storage) DropSlowLogIndexes() error {
	indexes := []string{
		"idx_slow_logs_file_hash",
		"idx_fingerprint_id",
		"idx_ts",
		"idx_db",
		"idx_user",
		"idx_query_time",
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec("DROP INDEX IF EXISTS " + idx); err != nil {
			return fmt.Errorf("failed to drop index %s: %w", idx, err)
		}
	}
	return nil
}

// CreateSlowLogIndexes creates all indexes on slow_logs.
// Safe to call repeatedly (uses IF NOT EXISTS).
func (s *Storage) CreateSlowLogIndexes() error {
	indexes := `
	CREATE INDEX IF NOT EXISTS idx_slow_logs_file_hash ON slow_logs(file_hash);
	CREATE INDEX IF NOT EXISTS idx_fingerprint_id ON slow_logs(fingerprint_id);
	CREATE INDEX IF NOT EXISTS idx_ts ON slow_logs(ts);
	CREATE INDEX IF NOT EXISTS idx_db ON slow_logs(db);
	CREATE INDEX IF NOT EXISTS idx_user ON slow_logs(user);
	CREATE INDEX IF NOT EXISTS idx_query_time ON slow_logs(query_time_sec);
	`
	_, err := s.db.Exec(indexes)
	return err
}

// FinalizeIngestion runs post-ingest maintenance: dedupe+repair then indexes [D6].
func (s *Storage) FinalizeIngestion(ctx context.Context) error {
	if err := s.DedupeSlowLogs(ctx); err != nil {
		return err
	}
	return s.CreateSlowLogIndexes()
}

// DedupeSlowLogs keeps MAX(id) per (file_hash, event_offset); NULL-safe [R2-3].
func (s *Storage) DedupeSlowLogs(ctx context.Context) error {
	q := `
	DELETE FROM slow_logs
	WHERE event_offset IS NOT NULL
	  AND file_hash IS NOT NULL
	  AND id NOT IN (
	    SELECT MAX(id)
	    FROM slow_logs
	    WHERE event_offset IS NOT NULL
	      AND file_hash IS NOT NULL
	    GROUP BY file_hash, event_offset
	  );
	`
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("dedupe slow_logs: %w", err)
	}
	return nil
}

func stringsToAny(ss []string) []any {
	if ss == nil {
		return []any{}
	}
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// InsertSlowLogBatch inserts multiple slow log entries using the DuckDB Appender API.
// Appender writes full rows in table column order [L9].
func (s *Storage) InsertSlowLogBatch(ctx context.Context, entries []*models.SlowLogEntry, fileHash string) error {
	if len(entries) == 0 {
		return nil
	}

	n := len(entries)
	ids := make([]int64, 0, n)
	rows, err := s.db.QueryContext(ctx, "SELECT nextval('seq_slow_log_id') FROM generate_series(1, $1)", n)
	if err != nil {
		return fmt.Errorf("failed to reserve IDs: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan reserved ID: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to iterate reserved IDs: %w", err)
	}

	conn, err := s.connector.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect for appender: %w", err)
	}
	defer conn.Close()

	appender, err := duckdb.NewAppenderFromConn(conn, "", "slow_logs")
	if err != nil {
		return fmt.Errorf("failed to create appender: %w", err)
	}

	// Column order: id, file_hash, fingerprint_id, sanitized_sql, sample_sql, user, host, db,
	// query_time_sec, lock_time_sec, rows_sent, rows_examined, ts, created_at, tables,
	// event_offset, event_hash
	for i, entry := range entries {
		if err := appender.AppendRow(
			ids[i],
			fileHash,
			entry.FingerprintID,
			entry.SanitizedSQL,
			entry.SampleSQL,
			entry.User,
			entry.Host,
			entry.DB,
			entry.QueryTimeSec,
			entry.LockTimeSec,
			entry.RowsSent,
			entry.RowsExamined,
			entry.TS,
			entry.CreatedAt,
			stringsToAny(entry.Tables),
			entry.EventOffset,
			entry.EventHash,
		); err != nil {
			_ = appender.Close()
			return fmt.Errorf("failed to append row %d: %w", i, err)
		}
	}

	if err := appender.Close(); err != nil {
		return fmt.Errorf("failed to flush appender: %w", err)
	}

	return nil
}

// GetLatestFileState returns the newest generation row for canonicalPath, or nil if unseen.
func (s *Storage) GetLatestFileState(ctx context.Context, canonicalPath string) (*models.FileState, error) {
	q := `
	SELECT hash, file_path, generation, file_size, file_mtime, first_seen_at, parse_count,
	       last_parsed_at, prefix_hash, prefix_len, tail_hash, tail_len, resume_offset, completed_size
	FROM files
	WHERE file_path = ?
	ORDER BY generation DESC
	LIMIT 1
	`
	row := s.db.QueryRowContext(ctx, q, canonicalPath)
	st := &models.FileState{}
	var mtime, firstSeen sql.NullTime
	var lastParsed sql.NullTime
	var prefixHash, tailHash sql.NullString
	err := row.Scan(
		&st.Hash, &st.FilePath, &st.Generation, &st.FileSize, &mtime, &firstSeen, &st.ParseCount,
		&lastParsed, &prefixHash, &st.PrefixLen, &tailHash, &st.TailLen, &st.ResumeOffset, &st.CompletedSize,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetLatestFileState: %w", err)
	}
	if mtime.Valid {
		st.FileMtime = mtime.Time
	}
	if firstSeen.Valid {
		st.FirstSeen = firstSeen.Time
	}
	if lastParsed.Valid {
		t := lastParsed.Time
		st.LastParsedAt = &t
	}
	if prefixHash.Valid {
		st.PrefixHash = prefixHash.String
	}
	if tailHash.Valid {
		st.TailHash = tailHash.String
	}
	return st, nil
}

// CreateFileGeneration inserts a new generation row (prefix frozen, offsets 0) [L5].
func (s *Storage) CreateFileGeneration(ctx context.Context, state *models.FileState) error {
	q := `
	INSERT INTO files (
		hash, file_path, generation, file_size, file_mtime, parse_count,
		prefix_hash, prefix_len, tail_hash, tail_len, resume_offset, completed_size
	) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, 0, 0)
	`
	_, err := s.db.ExecContext(ctx, q,
		state.Hash,
		state.FilePath,
		state.Generation,
		state.FileSize,
		state.FileMtime,
		nullIfEmpty(state.PrefixHash),
		state.PrefixLen,
		nullIfEmpty(state.TailHash),
		state.TailLen,
	)
	if err != nil {
		return fmt.Errorf("CreateFileGeneration: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// AdvanceResumeOffset sets resume_offset = MAX(resume_offset, ?) after a durable insert [L12].
func (s *Storage) AdvanceResumeOffset(ctx context.Context, identityHash string, resumeOffset int64) error {
	q := `UPDATE files SET resume_offset = GREATEST(resume_offset, ?) WHERE hash = ?`
	_, err := s.db.ExecContext(ctx, q, resumeOffset, identityHash)
	if err != nil {
		return fmt.Errorf("AdvanceResumeOffset: %w", err)
	}
	return nil
}

// CompleteFileParse records successful completion of a full/incremental parse run.
func (s *Storage) CompleteFileParse(ctx context.Context, identityHash string, completedSize int64, mtime time.Time, tailHash string, tailLen int64) error {
	q := `
	UPDATE files SET
		completed_size = ?,
		file_mtime = ?,
		tail_hash = ?,
		tail_len = ?,
		file_size = ?,
		parse_count = parse_count + 1,
		last_parsed_at = CURRENT_TIMESTAMP
	WHERE hash = ?
	`
	_, err := s.db.ExecContext(ctx, q, completedSize, mtime, nullIfEmpty(tailHash), tailLen, completedSize, identityHash)
	if err != nil {
		return fmt.Errorf("CompleteFileParse: %w", err)
	}
	return nil
}

// GetFileStats returns statistics about recorded files [L16].
func (s *Storage) GetFileStats(ctx context.Context) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	var totalFiles int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&totalFiles)
	if err != nil {
		return nil, err
	}
	stats["total_files"] = totalFiles

	// unique_files = generations parsed exactly once (parse_count == 1) [L16]
	var uniqueFiles int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE parse_count = 1`).Scan(&uniqueFiles)
	if err != nil {
		return nil, err
	}
	stats["unique_files"] = uniqueFiles

	// duplicate_files = generations parsed >1 time [L16]
	var duplicateFiles int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE parse_count > 1`).Scan(&duplicateFiles)
	if err != nil {
		return nil, err
	}
	stats["duplicate_files"] = duplicateFiles

	var totalBytes int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(file_size), 0) FROM files`).Scan(&totalBytes)
	if err != nil {
		return nil, err
	}
	stats["total_bytes"] = totalBytes

	return stats, nil
}

// Query executes a raw SQL query (optional bound args) and returns results.
func (s *Storage) Query(ctx context.Context, query string, args ...interface{}) (*models.ReportResult, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}

	var result models.ReportResult
	result.Columns = columns

	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		row := make([]interface{}, len(columns))
		for i, v := range values {
			switch val := v.(type) {
			case time.Time:
				row[i] = val.Format(time.RFC3339)
			case []byte:
				var arr []string
				if err := json.Unmarshal(val, &arr); err == nil {
					row[i] = arr
				} else {
					row[i] = string(val)
				}
			case []any:
				strs := make([]string, 0, len(val))
				for _, item := range val {
					if s, ok := item.(string); ok {
						strs = append(strs, s)
					}
				}
				row[i] = strs
			default:
				row[i] = v
			}
		}
		result.Rows = append(result.Rows, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	result.Count = len(result.Rows)
	return &result, nil
}

// GetStats returns basic statistics about stored slow logs
func (s *Storage) GetStats(ctx context.Context) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM slow_logs").Scan(&count)
	if err != nil {
		return nil, err
	}
	stats["total_entries"] = count

	var minTS, maxTS sql.NullTime
	err = s.db.QueryRowContext(ctx, "SELECT MIN(ts), MAX(ts) FROM slow_logs").Scan(&minTS, &maxTS)
	if err != nil {
		return nil, err
	}
	if minTS.Valid {
		stats["oldest_entry"] = minTS.Time
	}
	if maxTS.Valid {
		stats["newest_entry"] = maxTS.Time
	}

	var fingerprintCount int64
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT sanitized_sql) FROM slow_logs").Scan(&fingerprintCount)
	if err != nil {
		return nil, err
	}
	stats["unique_fingerprints"] = fingerprintCount

	return stats, nil
}

// DB exposes the underlying *sql.DB for tests.
func (s *Storage) DB() *sql.DB {
	return s.db
}
