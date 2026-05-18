// uuid.go owns UUIDv7 minting and strict UUIDv7 validation. Stdlib
// only (math/rand/v2 + time + bit manipulation) — no third-party
// dependency, consistent with the root module's stdlib-only rule.
//
// Every Item carries a `uuid`: a UUIDv7 the cache mints on single-item
// writes (/append, /upsert-create, /counter_add-create) and on the
// /warm // /rebuild input items that arrive without one. It is the
// stable link back to the source-of-truth row.
//
// Layout (RFC 9562 §5.7): bytes 0-5 are a 48-bit unix-millisecond
// timestamp; byte 6 high nibble is version 7; byte 8 top two bits are
// the variant (0b10). The remaining 74 bits — the 12-bit rand_a field
// and the 62-bit rand_b field — are filled entirely with randomness.
// That is RFC 9562 §5.7 "method 1".
//
// No counter, no lock, no shared state. newUUIDv7 is a pure function:
// one clock read plus two math/rand/v2 draws. An earlier draft used a
// store-wide monotonic counter so that same-millisecond UUIDs were
// strictly ordered; that counter needed a synchronised increment,
// which funnelled every concurrent write through one cache line and
// cost ~20% append throughput. Nothing in the cache needs
// within-millisecond UUID ordering — firstUUID/lastUUID track insert
// order directly, byUUID is an exact-match index, /delete_up_to
// resolves a boundary UUID to a seq — so the counter was pure cost.
//
// Uniqueness is probabilistic, not structural — and that is safe
// here. Two UUIDs minted in different milliseconds can never collide
// (distinct timestamp prefix). Within one millisecond, 74 random bits
// give a birthday-collision probability of ~N²/2^75; at the cache's
// measured peak (~200k writes/s ≈ 200 per ms) that is ~1e-18 per
// millisecond — unobservable over any realistic process lifetime. A
// cache is disposable and rebuildable besides; even an (effectively
// impossible) collision would only make one item unreachable by uuid,
// never corrupt state.
//
// Why math/rand/v2, not crypto/rand. A `uuid` is an item identity,
// not a secret or a capability — possessing a scope name grants
// access, never knowing an item's uuid. A per-mint crypto/rand read
// costs a getrandom syscall (~500 ns measured); math/rand/v2's
// auto-seeded ChaCha8 source delivers strong-quality bits in a few ns
// with no syscall. Its global source is goroutine-safe, so newUUIDv7
// is safe to call concurrently with no synchronisation of its own.

package scopecache

import (
	"errors"
	mathrand "math/rand/v2"
	"time"
)

// errInvalidUUIDv7 is returned by parseUUIDv7 for any input that is not
// a canonical lowercase-hex UUIDv7 string.
var errInvalidUUIDv7 = errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string")

// uuidStringLen is the length of a canonical UUID string (36 chars).
// The validators' size pre-checks add it for the not-yet-minted uuid
// of a create write (the cache mints after validation).
const uuidStringLen = 36

// hexDigits indexes a nibble to its lowercase-hex character.
const hexDigits = "0123456789abcdef"

// newUUIDv7 mints a fresh UUIDv7 as a 36-char lowercase-hex string.
// Pure function, no shared state — safe to call concurrently from any
// goroutine. See the file header for the no-counter / collision
// argument.
func newUUIDv7() string {
	ms := uint64(time.Now().UnixMilli())
	randA := mathrand.Uint64() // only the low 12 bits are used (rand_a)
	randB := mathrand.Uint64() // only the low 62 bits are used (rand_b)

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte((randA>>8)&0x0F)  // version 7 + rand_a high nibble
	b[7] = byte(randA)                   // rand_a low byte
	b[8] = 0x80 | byte((randB>>56)&0x3F) // variant 0b10 + rand_b
	b[9] = byte(randB >> 48)
	b[10] = byte(randB >> 40)
	b[11] = byte(randB >> 32)
	b[12] = byte(randB >> 24)
	b[13] = byte(randB >> 16)
	b[14] = byte(randB >> 8)
	b[15] = byte(randB)
	return formatUUID(b)
}

// formatUUID renders 16 bytes as a 36-char lowercase-hex UUID string
// with hyphens at the canonical 8-4-4-4-12 positions.
func formatUUID(b [16]byte) string {
	var s [36]byte
	dst := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			s[dst] = '-'
			dst++
		}
		s[dst] = hexDigits[b[i]>>4]
		s[dst+1] = hexDigits[b[i]&0x0F]
		dst += 2
	}
	return string(s[:])
}

// parseUUIDv7 strictly validates a canonical UUIDv7 string and returns
// its 16 bytes. Rejects uppercase hex, wrong length, misplaced hyphens,
// non-hex digits, and any version nibble other than 7 or variant other
// than 0b10.
func parseUUIDv7(s string) ([16]byte, error) {
	var b [16]byte
	if len(s) != 36 {
		return b, errInvalidUUIDv7
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return b, errInvalidUUIDv7
	}
	src := 0
	for i := 0; i < 16; i++ {
		if src == 8 || src == 13 || src == 18 || src == 23 {
			src++ // skip the hyphen
		}
		hi, ok1 := hexVal(s[src])
		lo, ok2 := hexVal(s[src+1])
		if !ok1 || !ok2 {
			return b, errInvalidUUIDv7
		}
		b[i] = hi<<4 | lo
		src += 2
	}
	if b[6]>>4 != 0x7 {
		return b, errInvalidUUIDv7 // version must be 7
	}
	if b[8]>>6 != 0b10 {
		return b, errInvalidUUIDv7 // variant must be 0b10
	}
	return b, nil
}

// isValidUUIDv7 reports whether s is a canonical UUIDv7 string.
func isValidUUIDv7(s string) bool {
	_, err := parseUUIDv7(s)
	return err == nil
}

// hexVal decodes one lowercase-hex digit; ok is false for uppercase or
// any non-hex byte (strict canonical form).
func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}
