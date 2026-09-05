package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The boundary files are emitted verbatim — bare JSON to stdout, or a 0600
// file with --export (step-I/O persistence design §5).
func TestEmitStepFile(t *testing.T) {
	raw := []byte(`{"taskId":"t1","context":{"prompt":"do it"}}`)
	var out bytes.Buffer
	if err := emitStepFile(&out, raw, ""); err != nil {
		t.Fatal(err)
	}
	if out.String() != string(raw)+"\n" {
		t.Errorf("stdout = %q, want the bytes plus one newline", out.String())
	}
	out.Reset()
	if err := emitStepFile(&out, []byte("{}\n"), ""); err != nil || out.String() != "{}\n" {
		t.Errorf("a file that ends in a newline gets no second one: %q %v", out.String(), err)
	}
	out.Reset()
	path := filepath.Join(t.TempDir(), "task.json")
	if err := emitStepFile(&out, raw, path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("export did not write the bytes verbatim: %v %q", err, got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %o, want 0600", st.Mode().Perm())
	}
	if !bytes.Contains(out.Bytes(), []byte(fmt.Sprintf("wrote %d bytes to ", len(raw)))) {
		t.Errorf("export summary = %q", out.String())
	}
}
