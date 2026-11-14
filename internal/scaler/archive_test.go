package scaler

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestArchiveLogTruncates(t *testing.T) {
	var out bytes.Buffer
	err := archiveLog(&out, 10<<20, func(w io.Writer) error {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 50 { // a job printing far more than the limit
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, notice, _ := strings.Cut(out.String(), "\n[pitlane]")
	if len(body) != 10<<20 || !strings.Contains(notice, "truncated") {
		t.Fatalf("kept %d bytes, notice %q", len(body), notice)
	}

	out.Reset()
	if err := archiveLog(&out, 10<<20, func(w io.Writer) error { _, err := io.WriteString(w, "short"); return err }); err != nil || out.String() != "short" {
		t.Fatalf("short log altered: %q %v", out.String(), err)
	}
}
