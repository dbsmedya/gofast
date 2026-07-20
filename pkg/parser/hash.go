package parser

import (
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"strconv"

	"github.com/zeebo/xxh3"
	"github.com/dbsmedya/gofast/pkg/models"
)

const hashWindowMax = 64 * 1024 // 64 KiB

// hash64Hex returns the 16-char lowercase hex of an xxh3-64 digest [L1].
// Use xxh3.Hash (64-bit), not xxh3.New() which is 128-bit.
func hash64Hex(data []byte) string {
	sum := xxh3.Hash(data)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], sum)
	return hex.EncodeToString(buf[:])
}

// identityHash computes files.hash for (canonical_path, generation) [L1].
// Layout: xxh3-64(canonical_path || 0x00 || strconv.FormatInt(generation, 10))
func identityHash(canonicalPath string, generation int64) string {
	// pre-size: path + 1 + digits
	b := make([]byte, 0, len(canonicalPath)+1+20)
	b = append(b, canonicalPath...)
	b = append(b, 0)
	b = append(b, strconv.FormatInt(generation, 10)...)
	return hash64Hex(b)
}

// hashFileRange hashes absolute byte range [start, start+length) with xxh3-64.
func hashFileRange(filePath string, start, length int64) (string, error) {
	if length <= 0 {
		return hash64Hex(nil), nil
	}
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return hash64Hex(buf[:n]), nil
}

// capturePrefix hashes the first min(size, 64KiB) bytes.
func capturePrefix(filePath string, size int64) (hash string, prefixLen int64, err error) {
	prefixLen = size
	if prefixLen > hashWindowMax {
		prefixLen = hashWindowMax
	}
	if prefixLen < 0 {
		prefixLen = 0
	}
	hash, err = hashFileRange(filePath, 0, prefixLen)
	return hash, prefixLen, err
}

// captureTail hashes [completedSize-tailLen, completedSize).
func captureTail(filePath string, completedSize int64) (hash string, tailLen int64, err error) {
	if completedSize <= 0 {
		return hash64Hex(nil), 0, nil
	}
	tailLen = completedSize
	if tailLen > hashWindowMax {
		tailLen = hashWindowMax
	}
	start := completedSize - tailLen
	hash, err = hashFileRange(filePath, start, tailLen)
	return hash, tailLen, err
}

// computeEventHash builds the content hash for dedupe/repair [R2-3].
// Format (0x00-separated; exact layout fixed here):
//
//	fingerprint_id | ts(unix nano) | host | user | db | query_time_sec |
//	lock_time_sec | rows_sent | rows_examined | sample_sql
//
// Floats use 'g' with -1 precision for stable round-trip of parsed doubles.
func computeEventHash(e *models.SlowLogEntry) string {
	b := make([]byte, 0, 256+len(e.SampleSQL))
	appendField := func(s string) {
		b = append(b, s...)
		b = append(b, 0)
	}
	appendField(e.FingerprintID)
	appendField(strconv.FormatInt(e.TS.UnixNano(), 10))
	appendField(e.Host)
	appendField(e.User)
	appendField(e.DB)
	appendField(strconv.FormatFloat(e.QueryTimeSec, 'g', -1, 64))
	appendField(strconv.FormatFloat(e.LockTimeSec, 'g', -1, 64))
	appendField(strconv.FormatUint(e.RowsSent, 10))
	appendField(strconv.FormatUint(e.RowsExamined, 10))
	// last field: no trailing separator needed for hash stability
	b = append(b, e.SampleSQL...)
	return hash64Hex(b)
}
