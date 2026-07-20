package parser

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/percona/go-mysql/log"
	slowlog "github.com/percona/go-mysql/log/slow"
	"github.com/percona/go-mysql/query"
	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// Engine handles parsing of MySQL slow logs
type Engine struct {
	storage      *storage.Storage
	batchSize    int
	filePatterns []string
	workers      int

	// testCommitErr, when non-nil, makes commitBatch fail immediately (tests only).
	testCommitErr error
}

// NewEngine creates a new parser engine.
// filePatterns empty → defaults; workers < 1 → 1 (sequential).
func NewEngine(store *storage.Storage, batchSize int, filePatterns []string, workers int) *Engine {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if len(filePatterns) == 0 {
		filePatterns = config.DefaultFilePatterns()
	}
	if workers < 1 {
		workers = 1
	}
	return &Engine{
		storage:      store,
		batchSize:    batchSize,
		filePatterns: append([]string(nil), filePatterns...),
		workers:      workers,
	}
}

// ParseResult represents the result of a parsing operation
type ParseResult struct {
	FilesProcessed int       `json:"files_processed"`
	FilesSkipped   int       `json:"files_skipped"`
	FilesFailed    int       `json:"files_failed"`
	EntriesParsed  int       `json:"entries_parsed"`
	EntriesStored  int       `json:"entries_stored"`
	Errors         []string  `json:"errors,omitempty"` // [L7] []string so JSON serializes
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time"`
	DurationStr    string    `json:"duration"`
}

// Duration returns the duration of the parsing operation
func (r *ParseResult) Duration() time.Duration {
	return r.EndTime.Sub(r.StartTime)
}

type parseOutcome int

const (
	outcomeParsed parseOutcome = iota
	outcomeSkipped
	outcomeFailed
)

// encounterAction is the decision from encounterFile [L6].
type encounterAction int

const (
	actionSkip encounterAction = iota
	actionFull
	actionIncremental
)

// encounterResult is returned by encounterFile.
type encounterResult struct {
	Action     encounterAction
	State      *models.FileState // non-nil for skip/incremental; for full, state to create or use
	NeedCreate bool              // true → caller must CreateFileGeneration before parse
}

// CanonicalPath resolves Abs then EvalSymlinks with fall-back-to-Abs [L2].
func CanonicalPath(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil
}

// ParseDirectory parses all slow log files in a directory
func (e *Engine) ParseDirectory(ctx context.Context, logDir string) (*ParseResult, error) {
	result := &ParseResult{
		StartTime: time.Now(),
		Errors:    make([]string, 0),
	}

	files, err := e.findLogFiles(logDir)
	if err != nil {
		return nil, fmt.Errorf("failed to find log files: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no slow log files found in directory: %s", logDir)
	}

	// workers > 1 → pool protocol [Phase 3]; workers == 1 → sequential (same commitBatch) [L13][L14]
	if e.workers > 1 {
		return e.parseDirectoryParallel(ctx, files)
	}

	for _, file := range files {
		select {
		case <-ctx.Done():
			result.EndTime = time.Now()
			result.DurationStr = result.EndTime.Sub(result.StartTime).String()
			return result, ctx.Err()
		default:
		}

		count, outcome, err := e.parseFile(ctx, file)
		switch outcome {
		case outcomeSkipped:
			result.FilesSkipped++
		case outcomeFailed:
			result.FilesFailed++
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("failed to parse %s: %v", file, err))
			}
		case outcomeParsed:
			result.FilesProcessed++
			result.EntriesParsed += count
			result.EntriesStored += count
		}
	}

	result.EndTime = time.Now()
	result.DurationStr = result.EndTime.Sub(result.StartTime).String()
	return result, nil
}

// ParseFile parses a single slow log file
func (e *Engine) ParseFile(ctx context.Context, filePath string) (*ParseResult, error) {
	result := &ParseResult{
		StartTime: time.Now(),
		Errors:    make([]string, 0),
	}

	count, outcome, err := e.parseFile(ctx, filePath)
	switch outcome {
	case outcomeSkipped:
		result.FilesSkipped = 1
	case outcomeFailed:
		result.FilesFailed = 1
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
		}
	case outcomeParsed:
		result.FilesProcessed = 1
		result.EntriesParsed = count
		result.EntriesStored = count
	}

	result.EndTime = time.Now()
	result.DurationStr = result.EndTime.Sub(result.StartTime).String()
	return result, nil
}

// findLogFiles finds all potential slow log files in a directory using configured globs [L4].
// Results are deduplicated by canonical path so a symlink and its target that both match
// the patterns are only ingested once (prevents worker-pool identityHash double-registration).
func (e *Engine) findLogFiles(logDir string) ([]string, error) {
	var files []string
	seen := make(map[string]struct{})

	resolvedDir, err := filepath.EvalSymlinks(logDir)
	if err != nil {
		resolvedDir = logDir
	}

	err = filepath.Walk(resolvedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !e.matchFilePatterns(info.Name()) {
			return nil
		}
		canonical, cerr := CanonicalPath(path)
		if cerr != nil {
			// Fall back to the walk path if canonicalize fails
			canonical = path
		}
		if _, ok := seen[canonical]; ok {
			return nil
		}
		seen[canonical] = struct{}{}
		files = append(files, canonical)
		return nil
	})

	return files, err
}

func (e *Engine) matchFilePatterns(name string) bool {
	base := strings.ToLower(name)
	for _, pat := range e.filePatterns {
		p := strings.ToLower(pat)
		ok, err := filepath.Match(p, base)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// encounterFile decides skip / full / incremental for a canonical path [L6].
func (e *Engine) encounterFile(ctx context.Context, canonicalPath string, size int64, mtime time.Time) (*encounterResult, error) {
	row, err := e.storage.GetLatestFileState(ctx, canonicalPath)
	if err != nil {
		return nil, err
	}

	// 1. No row → create generation 1 → full parse
	if row == nil {
		st, err := e.newGenerationState(canonicalPath, 1, size, mtime)
		if err != nil {
			return nil, err
		}
		return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
	}

	// 2. Truncated below prefix → new generation
	if size < row.PrefixLen {
		st, err := e.newGenerationState(canonicalPath, row.Generation+1, size, mtime)
		if err != nil {
			return nil, err
		}
		return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
	}

	// 3. Prefix check (when prefix_len > 0)
	if row.PrefixLen > 0 {
		curPrefix, err := hashFileRange(canonicalPath, 0, row.PrefixLen)
		if err != nil {
			return nil, fmt.Errorf("prefix hash: %w", err)
		}
		if curPrefix != row.PrefixHash {
			st, err := e.newGenerationState(canonicalPath, row.Generation+1, size, mtime)
			if err != nil {
				return nil, err
			}
			return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
		}
	}

	switch {
	case size == row.CompletedSize:
		// Candidate skip — verify tail window at original range
		if row.TailLen > 0 && row.CompletedSize > 0 {
			start := row.CompletedSize - row.TailLen
			curTail, err := hashFileRange(canonicalPath, start, row.TailLen)
			if err != nil {
				return nil, fmt.Errorf("tail hash: %w", err)
			}
			if curTail != row.TailHash {
				st, err := e.newGenerationState(canonicalPath, row.Generation+1, size, mtime)
				if err != nil {
					return nil, err
				}
				return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
			}
		}
		return &encounterResult{Action: actionSkip, State: row}, nil

	case size > row.CompletedSize:
		// Continuity check [R2-2]
		if row.TailLen > 0 && row.CompletedSize > 0 {
			start := row.CompletedSize - row.TailLen
			curTail, err := hashFileRange(canonicalPath, start, row.TailLen)
			if err != nil {
				return nil, fmt.Errorf("continuity tail hash: %w", err)
			}
			if curTail != row.TailHash {
				st, err := e.newGenerationState(canonicalPath, row.Generation+1, size, mtime)
				if err != nil {
					return nil, err
				}
				return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
			}
		}
		return &encounterResult{Action: actionIncremental, State: row}, nil

	default: // size < completed_size → shrank
		st, err := e.newGenerationState(canonicalPath, row.Generation+1, size, mtime)
		if err != nil {
			return nil, err
		}
		return &encounterResult{Action: actionFull, State: st, NeedCreate: true}, nil
	}
}

func (e *Engine) newGenerationState(canonicalPath string, generation int64, size int64, mtime time.Time) (*models.FileState, error) {
	prefixHash, prefixLen, err := capturePrefix(canonicalPath, size)
	if err != nil {
		return nil, fmt.Errorf("capture prefix: %w", err)
	}
	return &models.FileState{
		Hash:       identityHash(canonicalPath, generation),
		FilePath:   canonicalPath,
		Generation: generation,
		FileSize:   size,
		FileMtime:  mtime,
		PrefixHash: prefixHash,
		PrefixLen:  prefixLen,
	}, nil
}

// parseFile parses a single slow log file with generation identity + resume [Phase 2].
func (e *Engine) parseFile(ctx context.Context, filePath string) (int, parseOutcome, error) {
	canonical, err := CanonicalPath(filePath)
	if err != nil {
		return 0, outcomeFailed, fmt.Errorf("canonicalize path: %w", err)
	}

	// Stat for encounter
	stat, err := os.Stat(canonical)
	if err != nil {
		return 0, outcomeFailed, fmt.Errorf("stat: %w", err)
	}

	enc, err := e.encounterFile(ctx, canonical, stat.Size(), stat.ModTime())
	if err != nil {
		return 0, outcomeFailed, err
	}
	if enc.Action == actionSkip {
		slog.Debug("file skipped", "path", canonical, "generation", enc.State.Generation)
		return 0, outcomeSkipped, nil
	}

	if enc.NeedCreate {
		slog.Info("new file generation", "path", canonical, "generation", enc.State.Generation, "hash", enc.State.Hash)
		if err := e.storage.CreateFileGeneration(ctx, enc.State); err != nil {
			return 0, outcomeFailed, err
		}
	} else if enc.Action == actionIncremental {
		slog.Info("incremental parse", "path", canonical, "resume_offset", enc.State.ResumeOffset, "hash", enc.State.Hash)
	}

	// [L8] Stat immediately before parse
	startStat, err := os.Stat(canonical)
	if err != nil {
		return 0, outcomeFailed, fmt.Errorf("start stat: %w", err)
	}
	sizeAtStart := startStat.Size()
	mtimeStart := startStat.ModTime()

	file, err := os.Open(canonical)
	if err != nil {
		return 0, outcomeFailed, fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	hostIPs := scanHostIPs(file)

	startOffset := int64(0)
	if enc.Action == actionIncremental {
		startOffset = enc.State.ResumeOffset
	}

	count, lastBoundary, err := e.parseWithPercona(ctx, file, enc.State.Hash, hostIPs, startOffset)
	if err != nil {
		return count, outcomeFailed, err
	}

	// [L8] final stat for growth
	finalStat, err := os.Stat(canonical)
	if err != nil {
		return count, outcomeFailed, fmt.Errorf("final stat: %w", err)
	}

	// Refresh resume from storage (advanced per batch); fall back to lastBoundary
	stateAfter, getErr := e.storage.GetLatestFileState(ctx, canonical)
	resume := lastBoundary
	if getErr == nil && stateAfter != nil && stateAfter.Hash == enc.State.Hash {
		resume = stateAfter.ResumeOffset
	}

	completedSize := sizeAtStart
	if finalStat.Size() != sizeAtStart {
		completedSize = resume // conservative [R2-1]
	}
	// Empty file [L15]
	if sizeAtStart == 0 {
		completedSize = 0
	}

	// Invariant: resume_offset <= completed_size
	if resume > completedSize {
		completedSize = resume
	}

	tailHash, tailLen, err := captureTail(canonical, completedSize)
	if err != nil {
		return count, outcomeFailed, fmt.Errorf("capture tail: %w", err)
	}

	if err := e.storage.CompleteFileParse(ctx, enc.State.Hash, completedSize, mtimeStart, tailHash, tailLen); err != nil {
		return count, outcomeFailed, err
	}

	return count, outcomeParsed, nil
}

// commitBatch inserts a batch then advances resume_offset [L14][L12].
// If testCommitErr is set (tests only), that error is returned instead.
func (e *Engine) commitBatch(ctx context.Context, fileHash string, batch []*models.SlowLogEntry, batchBoundary int64) error {
	if e.testCommitErr != nil {
		return e.testCommitErr
	}
	if len(batch) == 0 {
		return nil
	}
	if err := e.storage.InsertSlowLogBatch(ctx, batch, fileHash); err != nil {
		return fmt.Errorf("insert batch: %w", err)
	}
	// Advance after durable insert; failure → next run replays, MAX(id) repair [L12]
	if err := e.storage.AdvanceResumeOffset(ctx, fileHash, batchBoundary); err != nil {
		return fmt.Errorf("advance resume: %w", err)
	}
	return nil
}

// batchSink receives a completed batch and its max Event.Offset boundary.
type batchSink func(batch []*models.SlowLogEntry, boundary int64) error

// parseWithPercona parses using the Percona slow log parser from startOffset,
// committing via commitBatch (sequential path) [L14].
func (e *Engine) parseWithPercona(ctx context.Context, file *os.File, fileHash string, hostIPs []hostIP, startOffset int64) (int, int64, error) {
	sink := func(batch []*models.SlowLogEntry, boundary int64) error {
		return e.commitBatch(ctx, fileHash, batch, boundary)
	}
	return e.parseWithPerconaSink(ctx, file, hostIPs, startOffset, sink, nil)
}

// parseWithPerconaSink is the shared event loop; sink commits or enqueues batches.
// If entriesParsed is non-nil, it is incremented per event [L11].
func (e *Engine) parseWithPerconaSink(
	ctx context.Context,
	file *os.File,
	hostIPs []hostIP,
	startOffset int64,
	sink batchSink,
	entriesParsed *atomic.Int64,
) (int, int64, error) {
	opts := log.Options{}
	if startOffset > 0 {
		opts.StartOffset = uint64(startOffset)
	}
	p := slowlog.NewSlowLogParser(file, opts)

	parseDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				parseDone <- fmt.Errorf("parser panic: %v", r)
			}
		}()
		parseDone <- p.Start()
	}()

	eventChan := p.EventChan()
	batch := make([]*models.SlowLogEntry, 0, e.batchSize)
	totalCount := 0
	var lastBoundary int64
	done := false

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		boundary := lastBoundary
		if err := sink(batch, boundary); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	for !done {
		select {
		case <-ctx.Done():
			p.Stop()
			_ = flush()
			return totalCount, lastBoundary, ctx.Err()

		case err := <-parseDone:
			if err != nil {
				_ = flush()
				return totalCount, lastBoundary, fmt.Errorf("parser error: %w", err)
			}
			done = true

		case event, ok := <-eventChan:
			if !ok {
				done = true
				break
			}

			entry := e.convertEventToEntry(event, hostIPs)
			batch = append(batch, entry)
			totalCount++
			if entriesParsed != nil {
				entriesParsed.Add(1)
			}
			if int64(event.Offset) > lastBoundary {
				lastBoundary = int64(event.Offset)
			}

			if len(batch) >= e.batchSize {
				if err := flush(); err != nil {
					p.Stop()
					return totalCount, lastBoundary, err
				}
			}
		}
	}

	if err := flush(); err != nil {
		return totalCount, lastBoundary, err
	}
	return totalCount, lastBoundary, nil
}

// convertEventToEntry converts a log.Event to our SlowLogEntry model
func (e *Engine) convertEventToEntry(event *log.Event, hostIPs []hostIP) *models.SlowLogEntry {
	sql := stripUseStatement(event.Query)

	db := event.Db
	if db == "" {
		db = extractDBFromQuery(event.Query)
	}

	fingerprint := query.Fingerprint(sql)

	queryTime := 0.0
	if v, ok := event.TimeMetrics["Query_time"]; ok {
		queryTime = v
	}
	lockTime := 0.0
	if v, ok := event.TimeMetrics["Lock_time"]; ok {
		lockTime = v
	}
	rowsSent := uint64(0)
	if v, ok := event.NumberMetrics["Rows_sent"]; ok {
		rowsSent = v
	}
	rowsExamined := uint64(0)
	if v, ok := event.NumberMetrics["Rows_examined"]; ok {
		rowsExamined = v
	}

	tables := extractTables(sql, db)
	fingerprintID := query.Id(fingerprint)

	host := event.Host
	if host == "" {
		host = lookupHostIP(hostIPs, event.Offset)
	}

	entry := &models.SlowLogEntry{
		FingerprintID: fingerprintID,
		SanitizedSQL:  fingerprint,
		SampleSQL:     sql,
		User:          event.User,
		Host:          host,
		DB:            db,
		QueryTimeSec:  queryTime,
		LockTimeSec:   lockTime,
		RowsSent:      rowsSent,
		RowsExamined:  rowsExamined,
		TS:            event.Ts,
		CreatedAt:     time.Now(),
		Tables:        tables,
		EventOffset:   int64(event.Offset),
	}
	entry.EventHash = computeEventHash(entry)
	return entry
}

// useDBRegex matches USE <database>; statements at the start of a query
var useDBRegex = regexp.MustCompile(`(?i)^\s*use\s+` + "`?" + `(\w+)` + "`?" + `\s*;?\s*`)

func extractDBFromQuery(sql string) string {
	match := useDBRegex.FindStringSubmatch(sql)
	if len(match) >= 2 {
		return match[1]
	}
	return ""
}

func stripUseStatement(sql string) string {
	loc := useDBRegex.FindStringIndex(sql)
	if loc == nil {
		return sql
	}
	return strings.TrimSpace(sql[loc[1]:])
}

var triggerKeywords = map[string]bool{
	"from": true, "join": true, "into": true, "update": true, "table": true,
}

var sqlKeywords = map[string]bool{
	"as": true, "set": true, "select": true, "where": true, "on": true,
	"and": true, "or": true, "not": true, "null": true, "dual": true,
	"values": true, "in": true, "is": true, "left": true, "right": true,
	"inner": true, "outer": true, "cross": true, "full": true,
	"group": true, "order": true, "having": true, "limit": true,
	"union": true, "use": true, "if": true, "exists": true,
	"between": true, "like": true, "case": true, "when": true,
	"then": true, "else": true, "end": true, "all": true,
	"straight_join": true, "natural": true, "using": true,
	"default": true, "true": true, "false": true, "each": true,
	"for": true, "force": true, "ignore": true, "index": true,
	"low_priority": true, "delayed": true, "high_priority": true,
	"distinctrow": true, "distinct": true, "interval": true,
}

type tokenKind int

const (
	tkIdent tokenKind = iota
	tkDot
	tkLParen
	tkRParen
	tkSemicolon
)

type sqlToken struct {
	kind tokenKind
	val  string
}

func tokenize(sql string) []sqlToken {
	tokens := make([]sqlToken, 0, 32)
	n := len(sql)
	i := 0
	for i < n {
		ch := sql[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '`':
			j := i + 1
			for j < n && sql[j] != '`' {
				j++
			}
			if j < n {
				tokens = append(tokens, sqlToken{tkIdent, sql[i+1 : j]})
				j++
			}
			i = j
		case ch == '\'':
			j := i + 1
			for j < n {
				if sql[j] == '\\' {
					j += 2
					continue
				}
				if sql[j] == '\'' {
					j++
					break
				}
				j++
			}
			i = j
		case ch == '"':
			j := i + 1
			for j < n && sql[j] != '"' {
				j++
			}
			if j < n {
				tokens = append(tokens, sqlToken{tkIdent, sql[i+1 : j]})
				j++
			}
			i = j
		case ch == '.':
			tokens = append(tokens, sqlToken{tkDot, ""})
			i++
		case ch == '(':
			tokens = append(tokens, sqlToken{tkLParen, ""})
			i++
		case ch == ')':
			tokens = append(tokens, sqlToken{tkRParen, ""})
			i++
		case ch == ';':
			tokens = append(tokens, sqlToken{tkSemicolon, ""})
			i++
		case ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch >= 0x80:
			j := i + 1
			for j < n {
				c := sql[j]
				if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c >= 0x80 {
					j++
				} else {
					break
				}
			}
			tokens = append(tokens, sqlToken{tkIdent, sql[i:j]})
			i = j
		default:
			i++
		}
	}
	return tokens
}

// collectCTENamesAt reads CTE aliases from tokens starting just after WITH.
// Does not skip past the statement — only walks far enough to list names (fail-open).
func collectCTENamesAt(tokens []sqlToken, i int) map[string]bool {
	ctes := make(map[string]bool)
	if i < len(tokens) && tokens[i].kind == tkIdent && strings.ToLower(tokens[i].val) == "recursive" {
		i++
	}
	for i < len(tokens) {
		if tokens[i].kind != tkIdent {
			break
		}
		name := strings.ToLower(tokens[i].val)
		// Next non-paren structure must be AS (or optional col list then AS)
		j := i + 1
		if j < len(tokens) && tokens[j].kind == tkLParen {
			depth := 1
			j++
			for j < len(tokens) && depth > 0 {
				switch tokens[j].kind {
				case tkLParen:
					depth++
				case tkRParen:
					depth--
				}
				j++
			}
		}
		if j >= len(tokens) || tokens[j].kind != tkIdent || strings.ToLower(tokens[j].val) != "as" {
			break // not a CTE list entry (likely SELECT) — stop
		}
		ctes[name] = true
		j++ // past AS
		if j >= len(tokens) || tokens[j].kind != tkLParen {
			break
		}
		// skip body
		depth := 1
		j++
		for j < len(tokens) && depth > 0 {
			switch tokens[j].kind {
			case tkLParen:
				depth++
			case tkRParen:
				depth--
			}
			j++
		}
		i = j
		// next CTE name or SELECT
		if i < len(tokens) && tokens[i].kind == tkIdent {
			nxt := strings.ToLower(tokens[i].val)
			if nxt == "select" || nxt == "with" {
				break
			}
			continue
		}
		break
	}
	return ctes
}

func extractTables(sql string, dbContext string) []string {
	sql = stripUseStatement(sql)
	tokens := tokenize(sql)
	seen := make(map[string]bool)
	var tables []string
	cteSet := make(map[string]bool)
	atStmtStart := true

	for i := 0; i < len(tokens); i++ {
		t := tokens[i]

		if t.kind == tkSemicolon {
			cteSet = make(map[string]bool)
			atStmtStart = true
			continue
		}

		// At statement start, capture CTE names then keep scanning the same tokens
		// so CTE bodies still contribute base tables [Phase 4].
		if t.kind == tkIdent && atStmtStart && strings.ToLower(t.val) == "with" {
			cteSet = collectCTENamesAt(tokens, i+1)
			atStmtStart = false
			// fall through — do not skip WITH body
		} else if t.kind == tkIdent {
			atStmtStart = false
		}

		if t.kind != tkIdent || !triggerKeywords[strings.ToLower(t.val)] {
			continue
		}
		trigger := strings.ToLower(t.val)

		i++
		if i >= len(tokens) {
			break
		}
		next := tokens[i]

		if next.kind == tkLParen {
			continue
		}
		if next.kind != tkIdent {
			continue
		}
		if sqlKeywords[strings.ToLower(next.val)] {
			continue
		}

		tableName := next.val
		dbName := ""
		qualified := false

		if i+2 < len(tokens) && tokens[i+1].kind == tkDot && tokens[i+2].kind == tkIdent {
			dbName = tableName
			tableName = tokens[i+2].val
			i += 2
			qualified = true
		}

		if trigger != "into" && i+1 < len(tokens) && tokens[i+1].kind == tkLParen {
			continue
		}

		// Exclude bare CTE names only
		if !qualified && cteSet[strings.ToLower(tableName)] {
			continue
		}

		var full string
		if dbName != "" {
			full = dbName + "." + tableName
		} else if dbContext != "" {
			full = dbContext + "." + tableName
		} else {
			full = tableName
		}

		if !seen[full] {
			seen[full] = true
			tables = append(tables, full)
		}
	}
	return tables
}

var hostFallbackRe = regexp.MustCompile(`User@Host: (?:[^\[]+|\[[^[]+\]).*?@ \S* \[(.*?)\]`)

type hostIP struct {
	offset int64
	ip     string
}

func scanHostIPs(file *os.File) []hostIP {
	var result []hostIP
	scanner := bufio.NewScanner(file)
	var offset int64
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# User@Host:") {
			if m := hostFallbackRe.FindStringSubmatch(line); len(m) >= 2 && m[1] != "" {
				result = append(result, hostIP{offset: offset, ip: m[1]})
			}
		}
		offset += int64(len(scanner.Bytes())) + 1
	}
	if _, err := file.Seek(0, 0); err != nil {
		// best-effort rewind; parser will fail clearly if position is wrong
		_ = err
	}
	return result
}

func lookupHostIP(hosts []hostIP, eventOffset uint64) string {
	if len(hosts) == 0 {
		return ""
	}
	idx := sort.Search(len(hosts), func(i int) bool {
		return hosts[i].offset > int64(eventOffset)
	})
	if idx > 0 {
		return hosts[idx-1].ip
	}
	return ""
}
