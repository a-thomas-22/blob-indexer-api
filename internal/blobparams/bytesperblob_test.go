package blobparams

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestBytesPerBlobValue locks the constant to its protocol-defined value so an
// accidental edit is caught immediately.
func TestBytesPerBlobValue(t *testing.T) {
	if BytesPerBlob != 131072 {
		t.Fatalf("BytesPerBlob = %d, want 131072 (4096 field elements * 32 bytes)", BytesPerBlob)
	}
}

// litRe matches the bare integer literal 131072 when it is not part of a longer
// number (e.g. it must not match inside 1310720 or 21310720).
var litRe = regexp.MustCompile(`(^|\D)(131072)(\D|$)`)

// TestBytesPerBlobMatchesSQLLiterals scans the bundled migration SQL files and
// the API package's Go sources for the literal 131072 and asserts every
// occurrence equals blobparams.BytesPerBlob.
//
// 131072 is a shared EIP-4844 protocol constant: it is both the blob byte size
// (4096 field elements * 32 bytes) and GAS_PER_BLOB
// (params.BlobTxBlobGasPerBlob). In this repo the SQL occurrences are blob-gas
// math (target_blob_gas / max_blob_gas), while Go-side blob_size_bytes usages
// are the byte size; both equal 131072 by definition. This test guards that
// single shared literal so it can't silently drift from the centralized
// constant.
//
// The test only READS those files; it never modifies them.
func TestBytesPerBlobMatchesSQLLiterals(t *testing.T) {
	// Tests run with the package directory (internal/blobparams) as the working
	// directory, so reach sibling packages via relative paths.
	targets := []struct {
		dir       string
		extension string
	}{
		{filepath.Join("..", "db", "migrations"), ".sql"},
		{filepath.Join("..", "api"), ".go"},
	}

	totalOccurrences := 0
	for _, target := range targets {
		entries, err := os.ReadDir(target.dir)
		if err != nil {
			t.Fatalf("reading %s: %v", target.dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != target.extension {
				continue
			}
			path := filepath.Join(target.dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			for _, m := range litRe.FindAllStringSubmatch(string(data), -1) {
				literal := m[2]
				got, err := strconv.Atoi(literal)
				if err != nil {
					t.Fatalf("%s: parsing %q: %v", path, literal, err)
				}
				if got != BytesPerBlob {
					t.Errorf("%s: literal %d does not equal blobparams.BytesPerBlob (%d)", path, got, BytesPerBlob)
				}
				totalOccurrences++
			}
		}
	}

	if totalOccurrences == 0 {
		t.Fatal("expected to find at least one 131072 literal across migrations and the api package, found none; the scan paths may be wrong")
	}
}
