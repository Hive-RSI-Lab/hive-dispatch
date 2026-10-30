package wire

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// MaxHive bounds a hive binary sent across.
const MaxHive = 1 << 30

// DescribeHive names the hive binary at path by its content and machine.
func DescribeHive(path string) (HiveBin, error) {
	var b HiveBin
	m, err := ELFMachine(path)
	if err != nil {
		return b, err
	}
	f, err := os.Open(path)
	if err != nil {
		return b, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return b, err
	}
	if n > MaxHive {
		return b, fmt.Errorf("%s is %d bytes, over the %d a hive binary may be", path, n, MaxHive)
	}
	return HiveBin{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Machine: m}, nil
}

// ELFMachine is the machine the executable at path is built for (EM_X86_64).
func ELFMachine(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s isn't an executable this can read: %w", path, err)
	}
	defer f.Close()
	return f.Machine.String(), nil
}
