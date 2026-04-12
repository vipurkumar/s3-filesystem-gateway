package s3fs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

// mockS3Client embeds *s3client.Client (nil) and intercepts GetObjectRange
// via a function field. We use a wrapper to satisfy chunkReader's dependency
// on *s3client.Client by building a thin shim layer.

// fakeChunkReader is a test-focused version that replaces the S3 call with
// an in-memory data source, exercising the exact same buffer/prefetch logic.
type fakeS3Source struct {
	mu   sync.Mutex
	data []byte
	// Track calls for assertions.
	calls []rangeCall
}

type rangeCall struct {
	offset, length int64
}

func (f *fakeS3Source) getRange(_ context.Context, _ string, off, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rangeCall{off, length})

	end := off + length
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	if off >= int64(len(f.data)) {
		return io.NopCloser(&emptyBuf{}), nil
	}
	chunk := make([]byte, end-off)
	copy(chunk, f.data[off:end])
	return io.NopCloser(&byteBuf{data: chunk}), nil
}

type emptyBuf struct{}

func (e *emptyBuf) Read([]byte) (int, error) { return 0, io.EOF }

type byteBuf struct {
	data []byte
	pos  int
}

func (b *byteBuf) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

// testChunkReader wraps the same logic as chunkReader but uses fakeS3Source
// instead of a real *s3client.Client.
type testChunkReader struct {
	src   *fakeS3Source
	s3Key string
	size  int64

	offset   int64
	buf      []byte
	bufStart int64
	bufLen   int

	chunkSize int
	seqCount  int
	lastEnd   int64
}

func newTestChunkReader(src *fakeS3Source, key string, size int64) *testChunkReader {
	return &testChunkReader{
		src:       src,
		s3Key:     key,
		size:      size,
		chunkSize: chunkSize1MB,
		lastEnd:   -1,
	}
}

func (r *testChunkReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	if off == r.lastEnd {
		r.seqCount++
		r.adaptChunkSize()
	} else {
		r.seqCount = 0
		r.chunkSize = chunkSize1MB
	}

	totalRead := 0
	for totalRead < len(p) && off < r.size {
		if r.bufLen > 0 && off >= r.bufStart && off < r.bufStart+int64(r.bufLen) {
			bufOff := int(off - r.bufStart)
			n := copy(p[totalRead:], r.buf[bufOff:r.bufLen])
			totalRead += n
			off += int64(n)
			continue
		}
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

func (r *testChunkReader) fetchChunk(off int64) error {
	length := int64(r.chunkSize)
	if off+length > r.size {
		length = r.size - off
	}
	if length <= 0 {
		return io.EOF
	}
	rc, err := r.src.getRange(context.Background(), r.s3Key, off, length)
	if err != nil {
		return err
	}
	defer rc.Close()

	if cap(r.buf) < int(length) {
		r.buf = make([]byte, length)
	} else {
		r.buf = r.buf[:length]
	}
	n, err := io.ReadFull(rc, r.buf)
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	if err != nil && err != io.EOF {
		return err
	}
	r.bufStart = off
	r.bufLen = n
	r.buf = r.buf[:n]
	return nil
}

func (r *testChunkReader) adaptChunkSize() {
	switch {
	case r.seqCount >= seqThreshold2 && r.chunkSize < chunkSize16MB:
		r.chunkSize = chunkSize16MB
	case r.seqCount >= seqThreshold1 && r.chunkSize < chunkSize4MB:
		r.chunkSize = chunkSize4MB
	}
}

func (r *testChunkReader) Seek(offset int64) error {
	if offset < 0 {
		return fmt.Errorf("negative offset")
	}
	r.offset = offset
	if r.bufLen > 0 && (offset < r.bufStart || offset >= r.bufStart+int64(r.bufLen)) {
		r.bufLen = 0
	}
	r.seqCount = 0
	r.chunkSize = chunkSize1MB
	r.lastEnd = -1
	return nil
}

func (r *testChunkReader) ChunkSize() int { return r.chunkSize }
func (r *testChunkReader) SeqCount() int  { return r.seqCount }

// --- Tests ---

// makeData creates deterministic test data of given size.
func makeData(size int) []byte {
	d := make([]byte, size)
	for i := range d {
		d[i] = byte(i % 251) // prime to avoid alignment tricks
	}
	return d
}

func TestSequentialReadsGrowChunkSize(t *testing.T) {
	// Use 5MB of data so multiple chunks are needed.
	dataSize := 5 * 1024 * 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	buf := make([]byte, 64*1024) // 64KB read buffer
	off := int64(0)

	// Read sequentially through the whole file.
	for off < int64(dataSize) {
		n, err := r.ReadAt(buf, off)
		if n > 0 {
			// Verify data correctness.
			for i := 0; i < n; i++ {
				expected := byte((int(off) + i) % 251)
				if buf[i] != expected {
					t.Fatalf("data mismatch at offset %d: got %d, want %d", off+int64(i), buf[i], expected)
				}
			}
			off += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read error at offset %d: %v", off, err)
		}
	}

	if off != int64(dataSize) {
		t.Fatalf("expected to read %d bytes, got %d", dataSize, off)
	}

	// After many sequential reads the chunk size should have grown.
	if r.ChunkSize() < chunkSize4MB {
		t.Errorf("expected chunk size >= 4MB after sequential reads, got %d", r.ChunkSize())
	}
}

func TestChunkSizeGrowthProgression(t *testing.T) {
	dataSize := 20 * 1024 * 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	buf := make([]byte, 64*1024)
	off := int64(0)

	var sawChunk4MB, sawChunk16MB bool
	for off < int64(dataSize) {
		n, err := r.ReadAt(buf, off)
		off += int64(n)

		if r.ChunkSize() >= chunkSize4MB {
			sawChunk4MB = true
		}
		if r.ChunkSize() >= chunkSize16MB {
			sawChunk16MB = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
	}

	if !sawChunk4MB {
		t.Error("never reached 4MB chunk size during sequential read")
	}
	if !sawChunk16MB {
		t.Error("never reached 16MB chunk size during sequential read")
	}
}

func TestRandomSeek(t *testing.T) {
	dataSize := 4 * 1024 * 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	// Read some data sequentially to build up seqCount.
	buf := make([]byte, 1024)
	for i := 0; i < 10; i++ {
		off := int64(i) * 1024
		n, err := r.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			t.Fatalf("seq read error: %v", err)
		}
		_ = n
	}

	// Now seek to a random spot.
	seekOff := int64(2*1024*1024 + 500)
	if err := r.Seek(seekOff); err != nil {
		t.Fatalf("seek error: %v", err)
	}

	// After seek, chunk size should be reset to 1MB.
	if r.ChunkSize() != chunkSize1MB {
		t.Errorf("expected chunk size reset to 1MB after seek, got %d", r.ChunkSize())
	}
	if r.SeqCount() != 0 {
		t.Errorf("expected seqCount reset to 0 after seek, got %d", r.SeqCount())
	}

	// Read at the seeked offset.
	n, err := r.ReadAt(buf, seekOff)
	if err != nil && err != io.EOF {
		t.Fatalf("read after seek error: %v", err)
	}
	for i := 0; i < n; i++ {
		expected := byte((int(seekOff) + i) % 251)
		if buf[i] != expected {
			t.Fatalf("data mismatch after seek at offset %d", seekOff+int64(i))
		}
	}
}

func TestReadAtVariousOffsets(t *testing.T) {
	dataSize := 2 * 1024 * 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	offsets := []int64{0, 100, 1023, 1024, 65535, 1024*1024 - 1, 1024 * 1024, int64(dataSize) - 100}
	buf := make([]byte, 256)

	for _, off := range offsets {
		t.Run(fmt.Sprintf("offset_%d", off), func(t *testing.T) {
			n, err := r.ReadAt(buf, off)
			if err != nil && err != io.EOF {
				t.Fatalf("read error at offset %d: %v", off, err)
			}
			if n == 0 && off < int64(dataSize) {
				t.Fatalf("expected to read data at offset %d", off)
			}
			for i := 0; i < n; i++ {
				expected := byte((int(off) + i) % 251)
				if buf[i] != expected {
					t.Fatalf("data mismatch at offset %d+%d: got %d, want %d", off, i, buf[i], expected)
				}
			}
		})
	}
}

func TestReadBeyondEOF(t *testing.T) {
	dataSize := 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	buf := make([]byte, 256)
	_, err := r.ReadAt(buf, int64(dataSize)+100)
	if err != io.EOF {
		t.Fatalf("expected EOF when reading beyond file, got: %v", err)
	}
}

func TestSeekNegativeOffset(t *testing.T) {
	src := &fakeS3Source{data: makeData(1024)}
	r := newTestChunkReader(src, "test/file", 1024)

	err := r.Seek(-1)
	if err == nil {
		t.Fatal("expected error for negative seek offset")
	}
}

func TestSeekWithinBuffer(t *testing.T) {
	dataSize := 2 * 1024 * 1024
	data := makeData(dataSize)
	src := &fakeS3Source{data: data}
	r := newTestChunkReader(src, "test/file", int64(dataSize))

	// Read to populate buffer.
	buf := make([]byte, 1024)
	_, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("initial read error: %v", err)
	}

	callsBefore := len(src.calls)

	// Seek within the already-buffered region (first 1MB chunk).
	if err := r.Seek(512); err != nil {
		t.Fatalf("seek error: %v", err)
	}

	// Read again — should NOT trigger a new S3 call since data is buffered.
	_, err = r.ReadAt(buf, 512)
	if err != nil && err != io.EOF {
		t.Fatalf("read after seek error: %v", err)
	}

	callsAfter := len(src.calls)
	if callsAfter != callsBefore {
		t.Errorf("expected no new S3 calls when reading within buffer, got %d new calls", callsAfter-callsBefore)
	}

	// Verify data correctness.
	for i := 0; i < len(buf); i++ {
		expected := byte((512 + i) % 251)
		if buf[i] != expected {
			t.Fatalf("data mismatch at offset %d", 512+i)
		}
	}
}

// Ensure chunkReader compiles against real s3client.Client (type check only).
func TestChunkReaderTypeCheck(t *testing.T) {
	var _ *chunkReader = newChunkReader((s3client.S3API)((*s3client.Client)(nil)), "key", 100, nil, "")
}

// mockS3ForReader is a test mock that satisfies s3client.S3API for reader tests.
type mockS3ForReader struct {
	s3client.S3API // embed to satisfy interface
	getRangeFn     func(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
}

func (m *mockS3ForReader) GetObjectRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	return m.getRangeFn(ctx, key, off, length)
}

func TestChunkReaderDataCacheHit(t *testing.T) {
	dir := t.TempDir()
	dc, err := cache.NewDataCache(cache.DataCacheConfig{
		Dir:              dir,
		MaxSize:          10 * 1024 * 1024,
		EvictionInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Stop()

	data := []byte("hello from cache")
	err = dc.Put("test-key", "etag-1", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	s3Called := false
	mock := &mockS3ForReader{
		getRangeFn: func(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
			s3Called = true
			return nil, fmt.Errorf("should not be called")
		},
	}

	r := newChunkReader(mock, "test-key", int64(len(data)), dc, "etag-1")
	buf := make([]byte, len(data))
	n, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello from cache" {
		t.Errorf("got %q, want %q", string(buf[:n]), "hello from cache")
	}
	if s3Called {
		t.Error("S3 should not have been called on cache hit")
	}
}

func TestChunkReaderDataCacheMiss(t *testing.T) {
	dir := t.TempDir()
	dc, err := cache.NewDataCache(cache.DataCacheConfig{
		Dir:              dir,
		MaxSize:          10 * 1024 * 1024,
		EvictionInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Stop()

	data := []byte("from S3")
	mock := &mockS3ForReader{
		getRangeFn: func(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
			end := off + length
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			return io.NopCloser(bytes.NewReader(data[off:end])), nil
		},
	}

	r := newChunkReader(mock, "miss-key", int64(len(data)), dc, "etag-miss")
	buf := make([]byte, len(data))
	n, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "from S3" {
		t.Errorf("got %q, want %q", string(buf[:n]), "from S3")
	}
}
