package parser

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
)

// inbound messages (writer-owned protocol) [R2-4][R3-1]

type fileStartMsg struct {
	identityHash string
	filePath     string
	ack          chan error
}

type batchMsg struct {
	identityHash  string
	entries       []*models.SlowLogEntry
	batchBoundary int64
}

type fileDoneMsg struct {
	identityHash  string
	parseErr      error
	completedSize int64
	mtime         time.Time
	tailHash      string
	tailLen       int64
}

type workItem struct {
	path      string
	canonical string
	state     *models.FileState
	action    encounterAction
}

type fileReg struct {
	ack   chan error
	acked bool
}

// parseDirectoryParallel runs the worker-pool ingestion protocol [Phase 3].
func (e *Engine) parseDirectoryParallel(ctx context.Context, files []string) (*ParseResult, error) {
	result := &ParseResult{
		StartTime: time.Now(),
		Errors:    make([]string, 0),
	}

	var (
		entriesParsed  atomic.Int64
		filesProcessed atomic.Int64
		filesFailed    atomic.Int64
		filesSkipped   atomic.Int64
		errorsMu       sync.Mutex
		errorMsgs      []string
	)

	inbound := make(chan any, e.workers*8)
	failCh := make(chan struct{})
	var failOnce sync.Once
	closeFail := func() { failOnce.Do(func() { close(failCh) }) }

	var entriesStored int64
	var writerErr error
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		entriesStored, writerErr = e.runWriter(ctx, inbound, failCh, closeFail)
	}()

	workCh := make(chan workItem, len(files))
	var workersWG sync.WaitGroup

	nWorkers := e.workers
	if nWorkers > len(files) {
		nWorkers = len(files)
	}
	if nWorkers < 1 {
		nWorkers = 1
	}

	for i := 0; i < nWorkers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			e.runWorker(ctx, workCh, inbound, failCh, &entriesParsed, &filesProcessed, &filesFailed, &errorsMu, &errorMsgs)
		}()
	}

	// Orchestrator: encounter + dispatch [L6].
	// Dedup by canonical path so symlink aliases of the same file are never
	// registered twice under one identityHash (would orphan an ack and deadlock).
	seenCanonical := make(map[string]struct{}, len(files))
	dispatchDone := false
	for _, file := range files {
		if dispatchDone {
			break
		}
		select {
		case <-ctx.Done():
			dispatchDone = true
		case <-failCh:
			dispatchDone = true
		default:
			canonical, err := CanonicalPath(file)
			if err != nil {
				filesFailed.Add(1)
				errorsMu.Lock()
				errorMsgs = append(errorMsgs, fmt.Sprintf("failed to parse %s: %v", file, err))
				errorsMu.Unlock()
				continue
			}
			if _, dup := seenCanonical[canonical]; dup {
				// Alias already dispatched this run (e.g. symlink + target both matched).
				filesSkipped.Add(1)
				continue
			}
			seenCanonical[canonical] = struct{}{}

			stat, err := os.Stat(canonical)
			if err != nil {
				filesFailed.Add(1)
				errorsMu.Lock()
				errorMsgs = append(errorMsgs, fmt.Sprintf("failed to parse %s: %v", file, err))
				errorsMu.Unlock()
				continue
			}
			enc, err := e.encounterFile(ctx, canonical, stat.Size(), stat.ModTime())
			if err != nil {
				filesFailed.Add(1)
				errorsMu.Lock()
				errorMsgs = append(errorMsgs, fmt.Sprintf("failed to parse %s: %v", file, err))
				errorsMu.Unlock()
				continue
			}
			if enc.Action == actionSkip {
				filesSkipped.Add(1)
				continue
			}
			if enc.NeedCreate {
				if err := e.storage.CreateFileGeneration(ctx, enc.State); err != nil {
					filesFailed.Add(1)
					errorsMu.Lock()
					errorMsgs = append(errorMsgs, fmt.Sprintf("failed to parse %s: %v", file, err))
					errorsMu.Unlock()
					continue
				}
			}
			workCh <- workItem{path: file, canonical: canonical, state: enc.State, action: enc.Action}
		}
	}

	close(workCh)
	workersWG.Wait()
	close(inbound) // orchestrator is sole closer [R2-4]
	writerWG.Wait()

	result.EntriesParsed = int(entriesParsed.Load())
	result.EntriesStored = int(entriesStored)
	result.FilesProcessed = int(filesProcessed.Load())
	result.FilesFailed = int(filesFailed.Load())
	result.FilesSkipped = int(filesSkipped.Load())
	result.Errors = errorMsgs
	result.EndTime = time.Now()
	result.DurationStr = result.EndTime.Sub(result.StartTime).String()

	if writerErr != nil {
		return result, writerErr
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func (e *Engine) runWorker(
	ctx context.Context,
	workCh <-chan workItem,
	inbound chan<- any,
	failCh <-chan struct{},
	entriesParsed, filesProcessed, filesFailed *atomic.Int64,
	errorsMu *sync.Mutex,
	errorMsgs *[]string,
) {
	send := func(msg any) bool {
		select {
		case inbound <- msg:
			return true
		case <-ctx.Done():
			return false
		case <-failCh:
			return false
		}
	}

	for item := range workCh {
		select {
		case <-ctx.Done():
			return
		case <-failCh:
			return
		default:
		}

		ack := make(chan error, 1)
		if !send(fileStartMsg{identityHash: item.state.Hash, filePath: item.canonical, ack: ack}) {
			return
		}

		_, doneMsg := e.workerParseFile(ctx, item, inbound, failCh, entriesParsed)
		_ = send(doneMsg) // if false, writer still covers via shutdown ack

		var ackErr error
		select {
		case ackErr = <-ack:
		case <-ctx.Done():
			ackErr = ctx.Err()
		case <-failCh:
			// wait briefly for writer negative ack
			select {
			case ackErr = <-ack:
			case <-time.After(50 * time.Millisecond):
				ackErr = fmt.Errorf("ingestion failed")
			}
		}

		if ackErr != nil {
			filesFailed.Add(1)
			errorsMu.Lock()
			*errorMsgs = append(*errorMsgs, fmt.Sprintf("failed to parse %s: %v", item.path, ackErr))
			errorsMu.Unlock()
		} else {
			filesProcessed.Add(1)
		}
	}
}

func (e *Engine) workerParseFile(
	ctx context.Context,
	item workItem,
	inbound chan<- any,
	failCh <-chan struct{},
	entriesParsed *atomic.Int64,
) (int, fileDoneMsg) {
	done := fileDoneMsg{identityHash: item.state.Hash}

	startStat, err := os.Stat(item.canonical)
	if err != nil {
		done.parseErr = err
		return 0, done
	}
	sizeAtStart := startStat.Size()
	mtimeStart := startStat.ModTime()
	done.mtime = mtimeStart

	file, err := os.Open(item.canonical)
	if err != nil {
		done.parseErr = err
		return 0, done
	}
	defer file.Close()

	hostIPs := scanHostIPs(file)
	startOffset := int64(0)
	if item.action == actionIncremental {
		startOffset = item.state.ResumeOffset
	}

	sink := func(batch []*models.SlowLogEntry, boundary int64) error {
		cp := make([]*models.SlowLogEntry, len(batch))
		copy(cp, batch)
		msg := batchMsg{identityHash: item.state.Hash, entries: cp, batchBoundary: boundary}
		select {
		case inbound <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-failCh:
			return fmt.Errorf("ingestion failed")
		}
	}

	count, lastBoundary, parseErr := e.parseWithPerconaSink(ctx, file, hostIPs, startOffset, sink, entriesParsed)
	if parseErr != nil {
		done.parseErr = parseErr
		return count, done
	}

	finalStat, err := os.Stat(item.canonical)
	if err != nil {
		done.parseErr = err
		return count, done
	}

	resume := lastBoundary
	completedSize := sizeAtStart
	if finalStat.Size() != sizeAtStart {
		completedSize = resume
	}
	if sizeAtStart == 0 {
		completedSize = 0
	}
	if resume > completedSize {
		completedSize = resume
	}

	tailHash, tailLen, err := captureTail(item.canonical, completedSize)
	if err != nil {
		done.parseErr = err
		return count, done
	}
	done.completedSize = completedSize
	done.tailHash = tailHash
	done.tailLen = tailLen
	return count, done
}

func (e *Engine) runWriter(ctx context.Context, inbound <-chan any, failCh chan struct{}, closeFail func()) (entriesStored int64, writerErr error) {
	registered := make(map[string]*fileReg)

	ackOne := func(hash string, err error) {
		r, ok := registered[hash]
		if !ok || r.acked {
			return
		}
		r.acked = true
		r.ack <- err // unconditional plain send [R3-1]
	}
	ackAllUnacked := func(err error) {
		for _, r := range registered {
			if !r.acked {
				r.acked = true
				r.ack <- err
			}
		}
	}

	fail := func(err error) {
		if writerErr == nil {
			writerErr = err
		}
		closeFail()
		ackAllUnacked(err)
	}

	for msg := range inbound {
		// Immediate cancel handling [R3-1]
		select {
		case <-ctx.Done():
			ackAllUnacked(ctx.Err())
			// continue draining remaining messages after this one
			e.handleWriterMsg(ctx, msg, registered, ackOne, &entriesStored, fail, true, writerErr)
			for m := range inbound {
				e.handleWriterMsg(ctx, m, registered, ackOne, &entriesStored, fail, true, writerErr)
			}
			ackAllUnacked(ctx.Err())
			if writerErr == nil {
				writerErr = ctx.Err()
			}
			return entriesStored, writerErr
		default:
		}

		// Already failed: drain with negative acks
		select {
		case <-failCh:
			e.handleWriterMsg(ctx, msg, registered, ackOne, &entriesStored, fail, true, writerErr)
			for m := range inbound {
				e.handleWriterMsg(ctx, m, registered, ackOne, &entriesStored, fail, true, writerErr)
			}
			ackAllUnacked(writerErr)
			return entriesStored, writerErr
		default:
		}

		e.handleWriterMsg(ctx, msg, registered, ackOne, &entriesStored, fail, writerErr != nil, writerErr)
		if writerErr != nil {
			// fail already closed failCh; keep draining
			for m := range inbound {
				e.handleWriterMsg(ctx, m, registered, ackOne, &entriesStored, fail, true, writerErr)
			}
			ackAllUnacked(writerErr)
			return entriesStored, writerErr
		}
	}

	// Channel closed and drained — ack any leftover [R3-1 shutdown]
	for hash, r := range registered {
		if r.acked {
			continue
		}
		switch {
		case writerErr != nil:
			ackOne(hash, writerErr)
		case ctx.Err() != nil:
			ackOne(hash, ctx.Err())
		default:
			ackOne(hash, fmt.Errorf("missing terminal message"))
		}
	}
	return entriesStored, writerErr
}

func (e *Engine) handleWriterMsg(
	ctx context.Context,
	msg any,
	registered map[string]*fileReg,
	ackOne func(string, error),
	entriesStored *int64,
	fail func(error),
	draining bool,
	writerErr error,
) {
	switch m := msg.(type) {
	case fileStartMsg:
		if existing, ok := registered[m.identityHash]; ok {
			// Defense in depth: never overwrite an in-flight registration (orphan ack = deadlock).
			if !existing.acked {
				m.ack <- fmt.Errorf("duplicate identity registration for %s", m.identityHash)
				return
			}
		}
		if draining {
			registered[m.identityHash] = &fileReg{ack: m.ack}
			ackOne(m.identityHash, errOr(writerErr, ctx))
			return
		}
		registered[m.identityHash] = &fileReg{ack: m.ack}

	case batchMsg:
		if draining {
			return
		}
		if err := e.commitBatch(ctx, m.identityHash, m.entries, m.batchBoundary); err != nil {
			fail(err)
			return
		}
		*entriesStored += int64(len(m.entries))

	case fileDoneMsg:
		if draining {
			ackOne(m.identityHash, errOr(writerErr, ctx))
			return
		}
		if m.parseErr != nil {
			ackOne(m.identityHash, m.parseErr)
			return
		}
		if err := e.storage.CompleteFileParse(ctx, m.identityHash, m.completedSize, m.mtime, m.tailHash, m.tailLen); err != nil {
			fail(err)
			return
		}
		ackOne(m.identityHash, nil)
	}
}

// errOr prefers the real writer failure, then context cancellation, then a generic message.
func errOr(writerErr error, ctx context.Context) error {
	if writerErr != nil {
		return writerErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("ingestion failed")
}
