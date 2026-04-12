package s3fs

import (
	"os"
	"testing"
	"time"
)

func TestFileInfo_AllMethods(t *testing.T) {
	now := time.Now()
	fi := &fileInfo{
		name:     "test.txt",
		size:     1024,
		mode:     0644,
		modTime:  now,
		isDir:    false,
		inode:    42,
		numLinks: 1,
	}

	if fi.Name() != "test.txt" {
		t.Errorf("Name() = %q, want %q", fi.Name(), "test.txt")
	}
	if fi.Size() != 1024 {
		t.Errorf("Size() = %d, want 1024", fi.Size())
	}
	if fi.Mode() != 0644 {
		t.Errorf("Mode() = %v, want 0644", fi.Mode())
	}
	if !fi.ModTime().Equal(now) {
		t.Errorf("ModTime() mismatch")
	}
	if fi.IsDir() {
		t.Error("IsDir() = true, want false")
	}
	if fi.Sys() != nil {
		t.Error("Sys() should return nil")
	}
	if !fi.ATime().Equal(now) {
		t.Error("ATime() should equal ModTime()")
	}
	if !fi.CTime().Equal(now) {
		t.Error("CTime() should equal ModTime()")
	}
	if fi.NumLinks() != 1 {
		t.Errorf("NumLinks() = %d, want 1", fi.NumLinks())
	}
}

func TestFileInfo_Directory(t *testing.T) {
	fi := &fileInfo{
		name:     "mydir",
		isDir:    true,
		mode:     os.ModeDir | 0755,
		numLinks: 2,
	}

	if !fi.IsDir() {
		t.Error("IsDir() = false, want true")
	}
	if fi.NumLinks() != 2 {
		t.Errorf("NumLinks() = %d, want 2", fi.NumLinks())
	}
}

func TestNewFileInfoFromS3_FileWithoutMeta(t *testing.T) {
	now := time.Now()
	fi := newFileInfoFromS3("file.txt", 500, now, false, 10, nil)

	if fi.Name() != "file.txt" {
		t.Errorf("Name() = %q", fi.Name())
	}
	if fi.Size() != 500 {
		t.Errorf("Size() = %d", fi.Size())
	}
	if fi.Mode() != DefaultFileMode {
		t.Errorf("Mode() = %v, want %v", fi.Mode(), DefaultFileMode)
	}
	if fi.IsDir() {
		t.Error("should not be a directory")
	}
	if fi.NumLinks() != 1 {
		t.Errorf("NumLinks() = %d, want 1", fi.NumLinks())
	}
	if fi.inode != 10 {
		t.Errorf("inode = %d, want 10", fi.inode)
	}
}

func TestNewFileInfoFromS3_FileWithMeta(t *testing.T) {
	meta := map[string]string{
		MetaKeyMode: "0755",
	}
	fi := newFileInfoFromS3("script.sh", 100, time.Now(), false, 20, meta)

	if fi.Mode() != os.FileMode(0755) {
		t.Errorf("Mode() = %v, want 0755", fi.Mode())
	}
}

func TestNewFileInfoFromS3_Directory(t *testing.T) {
	fi := newFileInfoFromS3("mydir", 0, time.Now(), true, 30, nil)

	if !fi.IsDir() {
		t.Error("should be a directory")
	}
	if fi.Mode()&os.ModeDir == 0 {
		t.Error("mode should have ModeDir bit set")
	}
	if fi.NumLinks() != 2 {
		t.Errorf("NumLinks() = %d, want 2", fi.NumLinks())
	}
}

func TestNewFileInfoFromS3_DirWithModeMeta(t *testing.T) {
	meta := map[string]string{
		MetaKeyMode: "0700",
	}
	fi := newFileInfoFromS3("secure", 0, time.Now(), true, 31, meta)

	if fi.Mode()&os.ModeDir == 0 {
		t.Error("mode should have ModeDir bit set even with custom mode")
	}
	// The permission bits should be 0700
	if fi.Mode().Perm() != os.FileMode(0700) {
		t.Errorf("Perm() = %v, want 0700", fi.Mode().Perm())
	}
}

func TestNewDirInfo(t *testing.T) {
	fi := newDirInfo("testdir", 55)

	if fi.Name() != "testdir" {
		t.Errorf("Name() = %q", fi.Name())
	}
	if fi.Size() != 0 {
		t.Errorf("Size() = %d", fi.Size())
	}
	if fi.Mode()&os.ModeDir == 0 {
		t.Error("mode should have ModeDir")
	}
	if fi.Mode().Perm() != os.FileMode(DefaultDirMode) {
		t.Errorf("Perm() = %v, want %v", fi.Mode().Perm(), os.FileMode(DefaultDirMode))
	}
	if !fi.IsDir() {
		t.Error("IsDir() should be true")
	}
	if fi.NumLinks() != 2 {
		t.Errorf("NumLinks() = %d, want 2", fi.NumLinks())
	}
	if fi.inode != 55 {
		t.Errorf("inode = %d, want 55", fi.inode)
	}
}

func TestParseMode_ValidOctal(t *testing.T) {
	meta := map[string]string{MetaKeyMode: "0600"}
	mode := parseMode(meta, false)
	if mode != os.FileMode(0600) {
		t.Errorf("parseMode = %v, want 0600", mode)
	}
}

func TestParseMode_InvalidString(t *testing.T) {
	meta := map[string]string{MetaKeyMode: "notanumber"}
	mode := parseMode(meta, false)
	if mode != DefaultFileMode {
		t.Errorf("parseMode(invalid) = %v, want %v", mode, DefaultFileMode)
	}
}

func TestParseMode_MissingKey(t *testing.T) {
	mode := parseMode(map[string]string{}, false)
	if mode != DefaultFileMode {
		t.Errorf("parseMode(missing) file = %v, want %v", mode, DefaultFileMode)
	}

	mode = parseMode(map[string]string{}, true)
	if mode != os.ModeDir|DefaultDirMode {
		t.Errorf("parseMode(missing) dir = %v, want %v", mode, os.ModeDir|DefaultDirMode)
	}
}

func TestParseMode_NilMeta(t *testing.T) {
	mode := parseMode(nil, false)
	if mode != DefaultFileMode {
		t.Errorf("parseMode(nil) = %v, want %v", mode, DefaultFileMode)
	}
}

func TestParseMode_DirWithValidMode(t *testing.T) {
	meta := map[string]string{MetaKeyMode: "0700"}
	mode := parseMode(meta, true)
	if mode&os.ModeDir == 0 {
		t.Error("dir mode should have ModeDir bit")
	}
	if mode.Perm() != os.FileMode(0700) {
		t.Errorf("Perm() = %v, want 0700", mode.Perm())
	}
}

func TestPosixMetadata(t *testing.T) {
	m := posixMetadata(1000, 1000, 0644)

	if m[MetaKeyUID] != "1000" {
		t.Errorf("UID = %q, want 1000", m[MetaKeyUID])
	}
	if m[MetaKeyGID] != "1000" {
		t.Errorf("GID = %q, want 1000", m[MetaKeyGID])
	}
	if m[MetaKeyMode] != "644" {
		t.Errorf("Mode = %q, want 644", m[MetaKeyMode])
	}
}

func TestPosixMetadata_DirMode(t *testing.T) {
	m := posixMetadata(0, 0, os.ModeDir|0755)
	// Perm() strips the ModeDir bit, so we should get "755"
	if m[MetaKeyMode] != "755" {
		t.Errorf("Mode = %q, want 755", m[MetaKeyMode])
	}
}

func TestFileInfoETag(t *testing.T) {
	meta := map[string]string{"Uid": "1000", "Gid": "1000", "Mode": "0644"}
	fi := newFileInfoFromS3("test.txt", 100, time.Now(), false, 42, meta)
	if fi.etag != "" {
		t.Errorf("expected empty etag without etag param, got %q", fi.etag)
	}

	fi2 := newFileInfoFromS3WithETag("test.txt", 100, time.Now(), false, 42, meta, "abc123")
	if fi2.etag != "abc123" {
		t.Errorf("expected etag 'abc123', got %q", fi2.etag)
	}
}
