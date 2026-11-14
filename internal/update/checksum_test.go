package update

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestVerifyChecksum(t *testing.T) {
	bin := []byte("binary contents")
	sum := sha256.Sum256(bin)
	other := sha256.Sum256([]byte("other"))
	file := fmt.Sprintf("%x  pitlane_linux_amd64\n%x *pitlane_linux_arm64\n", sum, other)

	if err := VerifyChecksum([]byte(file), "pitlane_linux_amd64", sum); err != nil {
		t.Errorf("good checksum: %v", err)
	}
	if err := VerifyChecksum([]byte(file), "pitlane_linux_arm64", other); err != nil {
		t.Errorf("binary-mode marker: %v", err)
	}
	if err := VerifyChecksum([]byte(file), "pitlane_linux_amd64", other); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("bad checksum accepted: %v", err)
	}
	if err := VerifyChecksum([]byte(file), "pitlane_linux_riscv64", sum); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("missing entry accepted: %v", err)
	}
	if err := VerifyChecksum(nil, "pitlane_linux_amd64", sum); err == nil {
		t.Error("empty checksums file accepted")
	}
}

func TestParseChecksumsRejectsBadFiles(t *testing.T) {
	sum := sha256.Sum256([]byte("a"))
	other := sha256.Sum256([]byte("b"))
	for name, file := range map[string]string{
		"short digest":   "abcd  pitlane_linux_amd64\n",
		"not hex":        strings.Repeat("z", 64) + "  pitlane_linux_amd64\n",
		"three fields":   fmt.Sprintf("%x  pitlane linux\n", sum),
		"conflict":       fmt.Sprintf("%x  f\n%x  f\n", sum, other),
		"missing digest": "pitlane_linux_amd64\n",
	} {
		if _, err := ParseChecksums([]byte(file)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if m, err := ParseChecksums([]byte(fmt.Sprintf("%x  f\n%x  f\n\n", sum, sum))); err != nil || len(m) != 1 {
		t.Errorf("duplicate identical entry: %v %v", m, err)
	}
}
