// Package builders resolves the identity of the entity that built a block
// from the two header fields that describe it: the extra data and the fee
// recipient.
//
// Nothing on-chain names a builder. Major block builders stamp a label into
// header.extraData ("Titan (titanbuilder.xyz)", "beaverbuild.org", ...), so
// that is the primary signal; the registry below maps those labels onto a
// stable grouping key and a display name. The registry is deliberately
// incomplete — every unregistered builder still groups consistently through
// the fallbacks, which key on the printable extra data and, failing that, on
// the fee recipient. Locally built blocks carry their execution client's
// default extra data ("geth", "reth/v1.2.3", ...) and therefore group under
// that client, which is the useful answer for them.
package builders

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Builder is the resolved identity of a block's builder. Key groups blocks
// (it is what block_builders.builder_key stores), Name is the display label,
// and Known is true only when the resolution came from the registry rather
// than from one of the fallbacks.
type Builder struct {
	Key   string
	Name  string
	Known bool
}

// registryEntry maps a case-insensitive substring of the block's extra data
// onto a builder. Match is stored lowercased; Resolve lowercases the extra
// data before comparing.
type registryEntry struct {
	// Match is the lowercase substring looked for in the extra data.
	Match string
	// Key is the stable grouping key stored on every block this builder
	// built. Changing one changes RegistryVersion and triggers the
	// indexer's relabel pass.
	Key string
	// Name is the display label.
	Name string
}

// registry is the label table. Order is significant only in that the first
// match wins; no two entries currently overlap. Keys are stable identifiers
// and should not be changed casually — they are what API consumers pin to.
//
// Deliberately no fee-recipient addresses: a builder's payout address is not
// verifiable from anything the indexer sees, and a wrong entry would
// mislabel blocks permanently. Extra-data labels are self-declared by the
// builder in the block itself.
var registry = []registryEntry{
	{Match: "titanbuilder", Key: "titan", Name: "Titan"},
	selfNamed("beaverbuild"),
	{Match: "rsync-builder", Key: "rsync", Name: "rsync-builder"},
	// The Flashbots builder stamps this (sic) phrase rather than a domain.
	{Match: "illuminate dmocratize dstribute", Key: "flashbots", Name: "Flashbots"},
	{Match: "bloxroute", Key: "bloxroute", Name: "bloXroute"},
	selfNamed("builder0x69"),
	selfNamed("penguinbuild"),
	{Match: "jetbldr", Key: "jetbldr", Name: "JetBldr"},
	{Match: "blocksmith", Key: "blocksmith", Name: "Blocksmith"},
	{Match: "eth-builder.com", Key: "eth-builder", Name: "eth-builder.com"},
	{Match: "gambit", Key: "gambit", Name: "Gambit"},
	{Match: "boba-builder", Key: "boba", Name: "boba-builder"},
	{Match: "tbuilder", Key: "tbuilder", Name: "tBuilder"},
	{Match: "payload.de", Key: "payload", Name: "Payload"},
	{Match: "buildai", Key: "buildai", Name: "BuildAI"},
	{Match: "quasar", Key: "quasar", Name: "Quasar"},
	selfNamed("nfactorial"),
	{Match: "eigenphi", Key: "eigenphi", Name: "EigenPhi"},
	{Match: "smithbot", Key: "smithbot", Name: "Smithbot"},
}

// selfNamed is a builder whose extra-data label is also its key and its
// display name.
func selfNamed(label string) registryEntry {
	return registryEntry{Match: label, Key: label, Name: label}
}

const (
	// minASCIIFallbackLen is the shortest trimmed extra data accepted as a
	// label. Below it the string carries no signal (a stray byte that
	// happens to be printable) and the fee recipient is the better key.
	minASCIIFallbackLen = 3
	// maxSlugLen bounds the generated key so a block stuffed with 32 bytes
	// of punctuation cannot produce an unwieldy key.
	maxSlugLen = 64
)

// Resolve identifies the builder of a block from its raw header fields.
// extraData is header.Extra as raw bytes and feeRecipient is header.Coinbase
// in any case. The result is never empty-keyed:
//
//  1. a registry label found (case-insensitively) in the extra data;
//  2. otherwise, when the trimmed extra data is printable ASCII of at least
//     three characters, key "extra:<slug>" with the trimmed text as name;
//  3. otherwise key "addr:<lowercase fee recipient>", named as a shortened
//     address.
//
// Only the first case sets Known.
func Resolve(extraData []byte, feeRecipient string) Builder {
	label := trimExtraData(extraData)
	lowered := strings.ToLower(label)
	if lowered != "" {
		for _, entry := range registry {
			if strings.Contains(lowered, entry.Match) {
				return Builder{Key: entry.Key, Name: entry.Name, Known: true}
			}
		}
	}

	if len(label) >= minASCIIFallbackLen {
		if slug := slugify(label); slug != "" {
			return Builder{Key: "extra:" + slug, Name: label}
		}
	}

	address := strings.ToLower(strings.TrimSpace(feeRecipient))
	if address == "" {
		// Header decoding guarantees a coinbase, so this only happens for
		// synthesized input. Grouping everything unattributable together
		// beats an empty key the column forbids.
		return Builder{Key: "addr:unknown", Name: "unknown"}
	}
	return Builder{Key: "addr:" + address, Name: shortenAddress(address)}
}

// RegistryVersion fingerprints the registry's contents. The indexer stores
// it alongside the labels it wrote; when a binary with a different registry
// starts, the mismatch triggers a pass that relabels existing rows from
// their (unchanged) raw header fields.
func RegistryVersion() string {
	h := sha256.New()
	for _, entry := range registry {
		// Field separators no key, name or match contains, so no two
		// registries can collide by re-splitting the same bytes.
		h.Write([]byte(entry.Match))
		h.Write([]byte{0})
		h.Write([]byte(entry.Key))
		h.Write([]byte{0})
		h.Write([]byte(entry.Name))
		h.Write([]byte{0, 0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// trimExtraData renders extra data as a label, or "" when it is not usable
// as one. Extra data is arbitrary bytes: clients pad it with NULs and
// builders wrap their label in whitespace, so both are trimmed. Anything
// non-printable left inside means the field is not text (a hash, a version
// blob) and the caller falls back to the fee recipient.
func trimExtraData(extraData []byte) string {
	trimmed := strings.TrimFunc(string(extraData), func(r rune) bool {
		return r <= ' ' || r == 0x7f
	})
	if trimmed == "" {
		return ""
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < 0x20 || trimmed[i] > 0x7e {
			return ""
		}
	}
	return trimmed
}

// slugify lowercases a label and collapses every run of non-alphanumeric
// characters into a single "-", so "Titan (titanbuilder.xyz)" becomes
// "titan-titanbuilder-xyz". The result is truncated to maxSlugLen and never
// starts or ends with "-".
func slugify(label string) string {
	var b strings.Builder
	b.Grow(len(label))
	pendingDash := false
	for i := 0; i < len(label); i++ {
		c := label[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteByte(c)
			continue
		}
		pendingDash = true
	}
	slug := b.String()
	if len(slug) > maxSlugLen {
		slug = slug[:maxSlugLen]
	}
	return strings.Trim(slug, "-")
}

// shortenAddress renders an address as 0x1234…abcd, the form the UI shows
// for builders that have no label at all.
func shortenAddress(address string) string {
	if len(address) < 12 {
		return address
	}
	return address[:6] + "…" + address[len(address)-4:]
}
