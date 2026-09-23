package taskqueues

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewFilesAreScopedAndPaginated(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "evidence.md")
	if e := os.WriteFile(name, []byte(strings.Repeat("界", 21000)), 0600); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{"proposal:evidence.md": name}
	first, e := readReviewFile(files, json.RawMessage(`{"path":"proposal:evidence.md"}`))
	if e != nil {
		t.Fatal(e)
	}
	page := first.(map[string]any)
	if page["has_more"] != true || page["next_offset"] != 20000 {
		t.Fatal(page)
	}
	if _, e = readReviewFile(files, json.RawMessage(`{"path":"/etc/passwd"}`)); e == nil {
		t.Fatal("read arbitrary path")
	}
	if e = os.Remove(name); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("/etc/passwd", name); e != nil {
		t.Fatal(e)
	}
	if _, e = readReviewFile(files, json.RawMessage(`{"path":"proposal:evidence.md"}`)); e == nil {
		t.Fatal("followed replaced symlink")
	}
}
