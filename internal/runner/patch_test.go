package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPatchWorker(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	dll := filepath.Join(dir, "bin", "Runner.Worker.dll")
	orig := append(append([]byte("MZ\x00header"), resultsVar...), []byte("\x00tail")...)
	_ = os.WriteFile(dll, orig, 0o644)

	if err := PatchWorker(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dll)
	if len(got) != len(orig) || bytes.Contains(got, resultsVar) || !bytes.Contains(got, renamedVar) {
		t.Fatalf("patch wrong: %q", got)
	}
	if err := PatchWorker(dir); err != nil {
		t.Fatalf("second patch should be a no-op: %v", err)
	}

	_ = os.WriteFile(dll, []byte("no such string"), 0o644)
	if err := PatchWorker(dir); err == nil {
		t.Fatal("expected an error when the string is missing")
	}
}
