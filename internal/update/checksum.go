package update

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
)

// ParseChecksums reads a sha256sum file ("<hex>  <name>" per line, with an
// optional "*" binary marker before the name). A name listed twice with
// different digests is an error: the file cannot be trusted.
func ParseChecksums(data []byte) (map[string][32]byte, error) {
	out := map[string][32]byte{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums line %d: expected \"<sha256>  <file>\"", n)
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != 32 {
			return nil, fmt.Errorf("checksums line %d: invalid SHA-256 %q", n, fields[0])
		}
		name := strings.TrimPrefix(fields[1], "*")
		var d [32]byte
		copy(d[:], sum)
		if prev, ok := out[name]; ok && prev != d {
			return nil, fmt.Errorf("checksums: conflicting entries for %s", name)
		}
		out[name] = d
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyChecksum checks that got is the digest listed for name in the
// checksums file. A missing entry is an error, never a pass.
func VerifyChecksum(checksums []byte, name string, got [32]byte) error {
	sums, err := ParseChecksums(checksums)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	if want != got {
		return fmt.Errorf("SHA-256 mismatch for %s: expected %x, downloaded %x", name, want, got)
	}
	return nil
}
