package s3fs

import (
	"context"
	"fmt"
	"io"

	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

const (
	chunkSize1MB  = 1 << 20  // 1 MB
	chunkSize4MB  = 4 << 20  // 4 MB
	chunkSize16MB = 16 << 20 // 16 MB

	// After this many consecutive sequential reads, bump chunk size.
	seqThreshold1 = 4  // after 4 sequential reads, grow to 4MB
	seqThreshold2 = 12 // after 12 sequential reads, grow to 16MB
)

// chunkReader implements ranged reads with adaptive prefetch against S3.
type chunkReader struct {
	s3    *s3client.Client
	s3Key string
	size  int64 // total file size

	offset int64 // logical read position

	buf      []byte // prefetch buffer
	bufStart int64  // offset in file where buf[0] corresponds
	bufLen   int    // valid bytes in buf

	chunkSize int   // current chunk fetch size
	seqCount  int   // number of consecutive sequential reads
	lastEnd   int64 // offset immediately after previous read (to detect sequential)
}

// newChunkReader creates a chunkReader for the given S3 object.
func newChunkReader(s3 *s3client.Client, key string, size int64) *chunkReader {
	return &chunkReader{
		s3:        s3,
		s3Key:     key,
		size:      size,
		chunkSize: chunkSize1MB,
		lastEnd:   -1,
	}
}

// ReadAt reads len(p) bytes from the file starting at byte offset off.
// It returns the number of bytes read and any error.
func (r *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}

	// Track sequential access for adaptive prefetch.
	if off == r.lastEnd {
		r.seqCount++
		r.adaptChunkSize()
	} else {
		r.seqCount = 0
		r.chunkSize = chunkSize1MB
	}

	totalRead := 0
	for totalRead < len(p) && off < r.size {
		// Check if the requested offset is within the current buffer.
		if r.bufLen > 0 && off >= r.bufStart && off < r.bufStart+int64(r.bufLen) {
			// Copy from buffer.
			bufOff := int(off - r.bufStart)
			n := copy(p[totalRead:], r.buf[bufOff:r.bufLen])
			totalRead += n
			off += int64(n)
			continue
		}

		// Need to fetch a new chunk from S3.
		if err := r.fetchChunk(off); err != nil {
			if totalRead > 0 {
				break
			}
			return 0, err
		}
	}

	r.lastEnd = off

	if totalRead == 0 {
		return 0, io.EOF
	}
	var err error
	if off >= r.size {
		err = io.EOF
	}
	return totalRead, err
}

// fetchChunk loads a chunk from S3 starting at the given offset.
func (r *chunkReader) fetchChunk(off int64) error {
	length := int64(r.chunkSize)
	if off+length > r.size {
		length = r.size - off
	}
	if length <= 0 {
		return io.EOF
	}

	rc, err := r.s3.GetObjectRange(context.Background(), r.s3Key, off, length)
	if err != nil {
		return fmt.Errorf("fetch chunk at %d: %w", off, err)
	}
	defer rc.Close()

	// Ensure buffer is large enough.
	if cap(r.buf) < int(length) {
		r.buf = make([]byte, length)
	} else {
		r.buf = r.buf[:length]
	}

	n, err := io.ReadFull(rc, r.buf)
	if err == io.ErrUnexpectedEOF {
		// Partial read is fine — server may return less than requested.
		err = nil
	}
	if err != nil && err != io.EOF {
		return fmt.Errorf("read chunk: %w", err)
	}

	r.bufStart = off
	r.bufLen = n
	r.buf = r.buf[:n]
	return nil
}

// adaptChunkSize grows the chunk size after sustained sequential reads.
func (r *chunkReader) adaptChunkSize() {
	switch {
	case r.seqCount >= seqThreshold2 && r.chunkSize < chunkSize16MB:
		r.chunkSize = chunkSize16MB
	case r.seqCount >= seqThreshold1 && r.chunkSize < chunkSize4MB:
		r.chunkSize = chunkSize4MB
	}
}

// Seek repositions the logical offset. The buffer is invalidated only when
// the new position falls outside the current buffer, keeping seeks within
// already-fetched data essentially free.
func (r *chunkReader) Seek(offset int64) error {
	if offset < 0 {
		return fmt.Errorf("negative offset")
	}
	r.offset = offset
	// If the new offset is outside the buffer, invalidate to avoid confusion,
	// but don't fetch eagerly — the next ReadAt will do that.
	if r.bufLen > 0 && (offset < r.bufStart || offset >= r.bufStart+int64(r.bufLen)) {
		r.bufLen = 0
	}
	// Reset sequential tracking since seek is a non-sequential operation.
	r.seqCount = 0
	r.chunkSize = chunkSize1MB
	r.lastEnd = -1
	return nil
}

// Close releases any held resources.
func (r *chunkReader) Close() error {
	r.buf = nil
	r.bufLen = 0
	return nil
}
