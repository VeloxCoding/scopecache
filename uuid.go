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
// timestamp; byte 6 high nibble is version 7; the 12-bit rand_a field
// (byte 6 low nibble + byte 7) is used as a monotonic counter; byte 8
// top two bits are the variant (0b10); the remaining 62 bits are
// random.
//
// Why math/rand/v2, not crypto/rand. A `uuid` is an item *identity*
// (the link to a source-of-truth row), not a secret or a capability —
// possessing a scope name is what grants access, never knowing an
// item's uuid. A per-mint crypto/rand read costs a getrandom syscall
// (~500 ns measured), which nearly doubled /append latency; math/rand/
// v2's auto-seeded ChaCha8 source delivers strong-quality bits in a
// few ns with no syscall. Uniqueness + ordering come from the
// monotonic counter below, NOT from the random bits.
//
// Monotonicity, lock-free. The generator's whole state is one
// atomic.Uint64, `packed`, holding (ms << 12 | counter): a 48-bit
// millisecond timestamp and a 12-bit counter. next() runs a CAS loop:
// load packed, derive the next (ms, counter) — new ms resets the
// counter, same ms increments it, a >4095 overflow borrows the next
// millisecond (RFC 9562 §6.2 method 2) — then CompareAndSwap. A
// backwards clock step (NTP) is clamped: the new ms is never below
// the stored one.
//
// Every successful CAS publishes a strictly greater `packed` value,
// so the sequence of emitted (ms, counter) pairs is strictly
// monotonic across all goroutines. That makes bytes 0-7 of every
// minted UUID strictly increasing BY CONSTRUCTION — which is both the
// ordering guarantee and the uniqueness guarantee (the random bytes
// 8-15 add entropy but play no part in either). No mutex, no
// post-hoc assertion: the CAS *is* the monotonicity proof.
//
// Lock-free matters on the write path: the generator is store-wide
// (one per store, shared by every scope), so a mutex here would
// serialise every concurrent /append regardless of scope. The CAS
// loop contends only on the single cache line and retries at most a
// few times under realistic write rates. next() takes no lock, so a
// caller holding a buffer's b.mu can call it freely — no lock order
// to reason about.
//
// Why math/rand/v2, not crypto/rand. A `uuid` is an item *identity*
// (the link to a source-of-truth row), not a secret or a capability —
// possessing a scope name is what grants access, never knowing an
// item's uuid. A per-mint crypto/rand read costs a getrandom syscall
// (~500 ns measured), which nearly doubled /append latency; math/rand/
// v2's auto-seeded ChaCha8 source delivers strong-quality bits in a
// few ns with no syscall.

package scopecache

import (
	"errors"
	mathrand "math/rand/v2"
	"sync/atomic"
	"time"
)

// errInvalidUUIDv7 is returned by parseUUIDv7 for any input that is not
// a canonical lowercase-hex UUIDv7 string.
var errInvalidUUIDv7 = errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string")

// uuidCounterMax is the largest value the 12-bit rand_a counter holds.
const uuidCounterMax = 0xFFF

// uuidStringLen is the length of a canonical UUID string (36 chars).
// The validators' size pre-checks add it for the not-yet-minted uuid
// of a create write (the cache mints after validation).
const uuidStringLen = 36

// hexDigits indexes a nibble to its lowercase-hex character.
const hexDigits = "0123456789abcdef"

// uuidGenerator mints strictly-monotonic UUIDv7 strings, lock-free.
// One instance per store, shared by every scope. Zero value is ready
// to use. See the file header for the CAS-monotonicity argument.
type uuidGenerator struct {
	// packed is (ms << 12 | counter): a 48-bit millisecond timestamp
	// and the 12-bit rand_a counter, advanced by CAS in next().
	packed atomic.Uint64
}

// next mints the next UUIDv7 as a 36-char lowercase-hex string.
// Lock-free and infallible — the CAS loop cannot fail to make
// progress, and the math/rand/v2 draw cannot error.
func (g *uuidGenerator) next() string {
	nowMs := uint64(time.Now().UnixMilli())
	var ms, counter uint64
	for {
		old := g.packed.Load()
		oldMs, oldCounter := old>>12, old&0xFFF
		if nowMs > oldMs {
			ms, counter = nowMs, 0
		} else {
			// Same millisecond, or the clock stepped backwards (NTP) —
			// clamp to oldMs so the timestamp never regresses, and bump
			// the counter. On a >4095 overflow borrow the next ms.
			ms, counter = oldMs, oldCounter+1
			if counter > uuidCounterMax {
				ms, counter = oldMs+1, 0
			}
		}
		if g.packed.CompareAndSwap(old, ms<<12|counter) {
			break
		}
	}
	// 62 random bits for rand_b (6 in byte 8, 56 in bytes 9-15). The
	// math/rand/v2 global source is auto-seeded ChaCha8 — strong
	// quality, no syscall (see the file header on the crypto/rand
	// trade-off).
	rnd := mathrand.Uint64()

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(counter>>8) // version 7 + counter high nibble
	b[7] = byte(counter)           // counter low byte
	b[8] = 0x80 | byte(rnd&0x3F)   // variant 0b10 + 6 random bits
	b[9] = byte(rnd >> 8)
	b[10] = byte(rnd >> 16)
	b[11] = byte(rnd >> 24)
	b[12] = byte(rnd >> 32)
	b[13] = byte(rnd >> 40)
	b[14] = byte(rnd >> 48)
	b[15] = byte(rnd >> 56)

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
