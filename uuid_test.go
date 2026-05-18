package scopecache

import (
	"sort"
	"sync"
	"testing"
)

// checkUUIDv7Shape asserts s is a canonical lowercase UUIDv7 string.
func checkUUIDv7Shape(t *testing.T, s string) {
	t.Helper()
	if len(s) != 36 {
		t.Fatalf("uuid %q: length %d, want 36", s, len(s))
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		t.Fatalf("uuid %q: hyphens not at 8-13-18-23", s)
	}
	if s[14] != '7' {
		t.Fatalf("uuid %q: version digit %q, want '7'", s, s[14])
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
		// variant 0b10xx — canonical
	default:
		t.Fatalf("uuid %q: variant digit %q, want one of 8/9/a/b", s, s[19])
	}
	if !isValidUUIDv7(s) {
		t.Fatalf("uuid %q: isValidUUIDv7 returned false", s)
	}
}

func TestUUIDv7_Format(t *testing.T) {
	var g uuidGenerator
	for i := 0; i < 1000; i++ {
		checkUUIDv7Shape(t, g.next())
	}
}

func TestUUIDv7_Monotonic(t *testing.T) {
	var g uuidGenerator
	prev := g.next()
	// Lowercase-hex strings with hyphens at fixed positions sort
	// lexically in the same order as the underlying 16 bytes.
	for i := 0; i < 200000; i++ {
		u := g.next()
		if u <= prev {
			t.Fatalf("not strictly increasing at #%d: %q <= %q", i, u, prev)
		}
		prev = u
	}
}

func TestUUIDv7_MonotonicConcurrent(t *testing.T) {
	var g uuidGenerator
	const goroutines = 50
	const perG = 4000

	var wg sync.WaitGroup
	chunks := make([][]string, goroutines)
	for gi := 0; gi < goroutines; gi++ {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			out := make([]string, 0, perG)
			for i := 0; i < perG; i++ {
				out = append(out, g.next())
			}
			chunks[gi] = out
		}(gi)
	}
	wg.Wait()

	all := make([]string, 0, goroutines*perG)
	for _, c := range chunks {
		all = append(all, c...)
	}
	sort.Strings(all)
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("duplicate uuid minted: %q", all[i])
		}
		if all[i] <= all[i-1] {
			t.Fatalf("non-monotonic after sort: %q <= %q", all[i], all[i-1])
		}
	}
}

func TestUUIDv7_RoundTrip(t *testing.T) {
	var g uuidGenerator
	for i := 0; i < 5000; i++ {
		u := g.next()
		b, err := parseUUIDv7(u)
		if err != nil {
			t.Fatalf("parseUUIDv7(%q): %v", u, err)
		}
		if formatUUID(b) != u {
			t.Fatalf("round-trip mismatch: %q -> bytes -> %q", u, formatUUID(b))
		}
	}
}

func TestFormatUUID(t *testing.T) {
	b := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0x7c, 0xde,
		0x8f, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd}
	got := formatUUID(b)
	want := "01234567-89ab-7cde-8f01-23456789abcd"
	if got != want {
		t.Fatalf("formatUUID = %q, want %q", got, want)
	}
}

func TestParseUUIDv7_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"too short":         "01234567-89ab-7cde-8f01-23456789abc",
		"too long":          "01234567-89ab-7cde-8f01-23456789abcdd",
		"uppercase hex":     "01234567-89AB-7cde-8f01-23456789abcd",
		"version 4 not 7":   "01234567-89ab-4cde-8f01-23456789abcd",
		"version 1 not 7":   "01234567-89ab-1cde-8f01-23456789abcd",
		"bad variant":       "01234567-89ab-7cde-0f01-23456789abcd",
		"hyphen misplaced":  "012345678-9ab-7cde-8f01-23456789abcd",
		"non-hex digit":     "01234567-89ab-7cde-8f01-23456789abcg",
		"spaces for hyphen": "01234567 89ab 7cde 8f01 23456789abcd",
	}
	for name, s := range cases {
		if _, err := parseUUIDv7(s); err == nil {
			t.Errorf("%s: parseUUIDv7(%q) accepted, want rejected", name, s)
		}
		if isValidUUIDv7(s) {
			t.Errorf("%s: isValidUUIDv7(%q) true, want false", name, s)
		}
	}
}

func BenchmarkUUIDGenerator_next(b *testing.B) {
	var g uuidGenerator
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = g.next()
	}
}

func BenchmarkFormatUUID(b *testing.B) {
	v := [16]byte{0x01, 0x92, 0xf3, 0xa0, 0x6e, 0x1c, 0x7c, 0x8a,
		0xb3, 0xd4, 0x1f, 0x2e, 0x3a, 0x4b, 0x5c, 0x6d}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = formatUUID(v)
	}
}

func TestParseUUIDv7_Accepts(t *testing.T) {
	// Canonical v7, every variant nibble (8/9/a/b) accepted.
	for _, s := range []string{
		"01234567-89ab-7cde-8f01-23456789abcd",
		"01234567-89ab-7cde-9f01-23456789abcd",
		"01234567-89ab-7cde-af01-23456789abcd",
		"01234567-89ab-7cde-bf01-23456789abcd",
	} {
		if _, err := parseUUIDv7(s); err != nil {
			t.Errorf("parseUUIDv7(%q): %v", s, err)
		}
	}
}
