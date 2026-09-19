package builders

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name         string
		extraData    string
		feeRecipient string
		wantKey      string
		wantName     string
		wantKnown    bool
	}{
		{
			name:         "registry match on a domain label",
			extraData:    "Titan (titanbuilder.xyz)",
			feeRecipient: "0x4838B106FCe9647Bdf1E7877BF73cE8B0BAD5f97",
			wantKey:      "titan",
			wantName:     "Titan",
			wantKnown:    true,
		},
		{
			name:         "registry match is case insensitive",
			extraData:    "BeaverBuild.org",
			feeRecipient: "0xaaaa0000aaaa0000aaaa0000aaaa0000aaaa0000",
			wantKey:      "beaverbuild",
			wantName:     "beaverbuild",
			wantKnown:    true,
		},
		{
			name:         "registry match tolerates NUL padding and whitespace",
			extraData:    "  rsync-builder  \x00\x00",
			feeRecipient: "0xbbbb0000bbbb0000bbbb0000bbbb0000bbbb0000",
			wantKey:      "rsync",
			wantName:     "rsync-builder",
			wantKnown:    true,
		},
		{
			name:         "flashbots stamps a phrase rather than a domain",
			extraData:    "Illuminate Dmocratize Dstribute",
			feeRecipient: "0xcccc0000cccc0000cccc0000cccc0000cccc0000",
			wantKey:      "flashbots",
			wantName:     "Flashbots",
			wantKnown:    true,
		},
		{
			name:         "ascii fallback keeps the label and slugifies the key",
			extraData:    "reth/v1.2.3",
			feeRecipient: "0xdddd0000dddd0000dddd0000dddd0000dddd0000",
			wantKey:      "extra:reth-v1-2-3",
			wantName:     "reth/v1.2.3",
		},
		{
			name:         "ascii fallback collapses punctuation runs",
			extraData:    "\x00 Acme!!! Builder ---  \x00",
			feeRecipient: "0xeeee0000eeee0000eeee0000eeee0000eeee0000",
			wantKey:      "extra:acme-builder",
			wantName:     "Acme!!! Builder ---",
		},
		{
			name:         "ascii fallback truncates an overlong slug",
			extraData:    strings.Repeat("a", 80),
			feeRecipient: "0xffff0000ffff0000ffff0000ffff0000ffff0000",
			wantKey:      "extra:" + strings.Repeat("a", maxSlugLen),
			wantName:     strings.Repeat("a", 80),
		},
		{
			name:         "address fallback when extra data is empty",
			extraData:    "",
			feeRecipient: "0x1234567890AbCdEf1234567890abcdef0000ABCD",
			wantKey:      "addr:0x1234567890abcdef1234567890abcdef0000abcd",
			wantName:     "0x1234…abcd",
		},
		{
			name:         "address fallback when extra data is binary",
			extraData:    "\x01\x02\xff\xfe\x03",
			feeRecipient: "0x1111222233334444555566667777888899990000",
			wantKey:      "addr:0x1111222233334444555566667777888899990000",
			wantName:     "0x1111…0000",
		},
		{
			name:         "address fallback when the label is too short",
			extraData:    "ab",
			feeRecipient: "0x2222222233334444555566667777888899990000",
			wantKey:      "addr:0x2222222233334444555566667777888899990000",
			wantName:     "0x2222…0000",
		},
		{
			name:         "punctuation-only label falls back to the address",
			extraData:    "!!!!",
			feeRecipient: "0x3333222233334444555566667777888899990000",
			wantKey:      "addr:0x3333222233334444555566667777888899990000",
			wantName:     "0x3333…0000",
		},
		{
			name:      "unknown group when nothing identifies the block",
			extraData: "",
			wantKey:   "addr:unknown",
			wantName:  "unknown",
		},
		{
			name:         "short fee recipient is shown as-is",
			extraData:    "",
			feeRecipient: "0xabc",
			wantKey:      "addr:0xabc",
			wantName:     "0xabc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve([]byte(tc.extraData), tc.feeRecipient)
			if got.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", got.Key, tc.wantKey)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if got.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v", got.Known, tc.wantKnown)
			}
		})
	}
}

// Every registry entry must resolve from its own label, and no entry may be
// shadowed by an earlier one: a shadowed label would silently mislabel every
// block of that builder.
func TestRegistryEntriesResolveToThemselves(t *testing.T) {
	seenKeys := make(map[string]string, len(registry))
	for _, entry := range registry {
		if entry.Match != strings.ToLower(entry.Match) {
			t.Errorf("entry %q: Match must be lowercase", entry.Key)
		}
		if entry.Key == "" || entry.Name == "" {
			t.Errorf("entry %q: Key and Name must be set", entry.Match)
		}
		if prior, dup := seenKeys[entry.Key]; dup {
			t.Errorf("entry %q: key already used by %q", entry.Match, prior)
		}
		seenKeys[entry.Key] = entry.Match

		got := Resolve([]byte("x "+entry.Match+" x"), "0xfeed")
		if got.Key != entry.Key || !got.Known {
			t.Errorf("Resolve(%q) = %+v, want key %q known", entry.Match, got, entry.Key)
		}
	}
}

func TestRegistryVersion(t *testing.T) {
	first := RegistryVersion()
	if len(first) != 16 {
		t.Fatalf("RegistryVersion() = %q, want 16 hex characters", first)
	}
	if first != RegistryVersion() {
		t.Fatal("RegistryVersion() is not stable across calls")
	}

	// A label change must change the fingerprint, or the relabel pass would
	// never run for it.
	original := registry
	t.Cleanup(func() { registry = original })
	registry = append(append([]registryEntry(nil), original...), registryEntry{
		Match: "zzz-test-builder", Key: "zzz", Name: "ZZZ",
	})
	if changed := RegistryVersion(); changed == first {
		t.Fatal("RegistryVersion() did not change after a registry change")
	}

	registry = original
	if restored := RegistryVersion(); restored != first {
		t.Fatalf("RegistryVersion() = %q after restore, want %q", restored, first)
	}
}

// Header extra data is arbitrary bytes, and a builder's registered label can
// sit next to one that is not printable ASCII. The registry must still match:
// the printable-ASCII trim the fallbacks rely on rejects the whole value in
// that case, so matching after it would silently demote a known builder to
// the fee-recipient fallback.
func TestResolveMatchesRegistryThroughNonASCIIExtraData(t *testing.T) {
	tests := []struct {
		name  string
		extra []byte
	}{
		{"trailing multibyte rune", []byte("Gambit ☃")},
		{"embedded NUL", append([]byte("beaverbuild.org"), 0x00, 0x01)},
		{"invalid utf-8 byte", append([]byte("rsync-builder"), 0xff)},
		{"leading control byte", append([]byte{0x02}, []byte("Titan (titanbuilder.xyz)")...)},
	}
	want := map[string]string{
		"trailing multibyte rune": "gambit",
		"embedded NUL":            "beaverbuild",
		"invalid utf-8 byte":      "rsync",
		"leading control byte":    "titan",
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.extra, "0xfeedfacefeedfacefeedfacefeedfacefeedface")
			if !got.Known || got.Key != want[tc.name] {
				t.Fatalf("Resolve(%q) = %+v, want known key %q", tc.extra, got, want[tc.name])
			}
		})
	}
}

// Extra data with no registered label and any non-printable byte still falls
// through to the fee recipient: the raw-byte match must not widen what counts
// as a usable label.
func TestResolveStillFallsBackWithoutARegistryMatch(t *testing.T) {
	got := Resolve([]byte("mystery\x00builder\xff"), "0xFEEDFACEFEEDFACEFEEDFACEFEEDFACEFEEDFACE")
	if got.Known {
		t.Fatalf("Resolve() = %+v, want an unknown builder", got)
	}
	if got.Key != "addr:0xfeedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("Resolve() key = %q, want the fee-recipient fallback", got.Key)
	}
}
