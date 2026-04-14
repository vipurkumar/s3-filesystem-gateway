package s3fs

import (
	"bytes"
	"errors"
	"os"
	"sort"
	"testing"
)

func seedXattrFile(t *testing.T, mock *mockS3, key string) {
	t.Helper()
	mock.put(key, []byte("hello"), map[string]string{
		MetaKeyUID:  "1000",
		MetaKeyGID:  "1000",
		MetaKeyMode: "644",
	})
}

func TestXattr_RoundTrip(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "file.bin")

	val := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := fs.SetXattr("/file.bin", "checksum", val, 0 /*EITHER*/); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	got, err := fs.GetXattr("/file.bin", "checksum")
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Fatalf("value mismatch: got %x want %x", got, val)
	}

	names, err := fs.ListXattrs("/file.bin")
	if err != nil {
		t.Fatalf("ListXattrs: %v", err)
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "checksum" {
		t.Fatalf("unexpected list: %v", names)
	}

	if err := fs.RemoveXattr("/file.bin", "checksum"); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
	if _, err := fs.GetXattr("/file.bin", "checksum"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist after remove, got %v", err)
	}
}

func TestXattr_BinaryValueBase64(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "bin.dat")

	// Full byte range including NULs — base64 must protect the S3 header.
	val := make([]byte, 256)
	for i := range val {
		val[i] = byte(i)
	}
	if err := fs.SetXattr("/bin.dat", "raw", val, 0); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	got, _ := fs.GetXattr("/bin.dat", "raw")
	if !bytes.Equal(got, val) {
		t.Fatalf("binary round-trip mismatch")
	}
}

func TestXattr_CreateAndReplaceFlags(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "f")

	// REPLACE on missing → ErrNotExist
	if err := fs.SetXattr("/f", "missing", []byte("x"), 2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("REPLACE missing: expected ErrNotExist, got %v", err)
	}

	// CREATE new → ok
	if err := fs.SetXattr("/f", "new", []byte("a"), 1); err != nil {
		t.Fatalf("CREATE new: %v", err)
	}
	// CREATE existing → ErrExist
	if err := fs.SetXattr("/f", "new", []byte("b"), 1); !errors.Is(err, os.ErrExist) {
		t.Fatalf("CREATE existing: expected ErrExist, got %v", err)
	}
	// REPLACE existing → ok, value updated
	if err := fs.SetXattr("/f", "new", []byte("c"), 2); err != nil {
		t.Fatalf("REPLACE existing: %v", err)
	}
	got, _ := fs.GetXattr("/f", "new")
	if string(got) != "c" {
		t.Fatalf("expected 'c', got %q", got)
	}
}

func TestXattr_ValueTooBig(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "f")
	big := make([]byte, xattrValueMax+1)
	err := fs.SetXattr("/f", "big", big, 0)
	if !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected ErrInvalid for oversized value, got %v", err)
	}
}

func TestXattr_RejectsNamespacePrefix(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "f")

	// Wire names are naked — any namespace prefix is a protocol
	// violation that should return ErrInvalid.
	for _, name := range []string{"user.x", "trusted.foo", "security.selinux", "system.y"} {
		if err := fs.SetXattr("/f", name, []byte("x"), 0); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("SET %q: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestXattr_ListFiltersNonXattrMetadata(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	seedXattrFile(t, mock, "f") // seeds Uid/Gid/Mode

	fs.SetXattr("/f", "a", []byte("1"), 0)
	fs.SetXattr("/f", "b", []byte("2"), 0)

	names, err := fs.ListXattrs("/f")
	if err != nil {
		t.Fatalf("ListXattrs: %v", err)
	}
	sort.Strings(names)
	want := []string{"a", "b"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("unexpected list: got %v want %v", names, want)
	}
}

func TestXattr_Copy_PopulatesDestination(t *testing.T) {
	fs, mock, cleanup := setupTestFS(t)
	defer cleanup()
	mock.put("src.bin", []byte("payload"), map[string]string{MetaKeyMode: "644"})

	if err := fs.Copy("/src.bin", "/dst.bin"); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	mock.mu.RLock()
	dst, ok := mock.objects["dst.bin"]
	mock.mu.RUnlock()
	if !ok {
		t.Fatalf("dst.bin missing after Copy")
	}
	if string(dst.data) != "payload" {
		t.Fatalf("dst data mismatch: %q", dst.data)
	}
}
