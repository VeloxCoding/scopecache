package scopecache

import (
	"encoding/json"
	"testing"

	gojson "github.com/goccy/go-json"
)

// bench_marshal_test.go — micro-benchmarks for the response-envelope
// json.Marshal call sites. Each Benchmark here measures json.Marshal
// of one response struct in isolation, so the cost of the marshal
// step is visible separately from the gateway/store work and the
// HTTP I/O.
//
// Why this file exists:
//
//   - writeJSONResponse in handlers.go is the canonical envelope writer
//     for every non-cap-protected response. It calls json.Marshal
//     directly. The cost of that call lives entirely in reflection
//     over the struct's fields.
//   - emitEvent in events.go calls json.Marshal(writeEvent) on every
//     successful mutation when events_mode != off. Same pattern.
//   - Item.MarshalJSON was hand-rolled in 964c9bf to drop its
//     reflection cost from ~630 to ~340 ns. Verifying that baseline
//     here keeps the test honest if someone reverts.
//
// Numbers from `go test -bench=BenchmarkMarshal -benchmem` on this
// host inform the next round of hand-rolling — anything > ~150 ns is
// a candidate for the appendItemJSON-style byte-builder treatment.

// --- Inner / shared structs -------------------------------------------

func BenchmarkMarshal_Item(b *testing.B) {
	item := Item{
		Scope:   "users",
		ID:      "alice",
		Seq:     42,
		Ts:      1715600000123456,
		Payload: json.RawMessage(`{"name":"Alice","age":30}`),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(item)
	}
}

func BenchmarkMarshal_writeAck(b *testing.B) {
	id := "alice"
	ack := writeAck{
		Scope: "users",
		ID:    &id,
		Seq:   42,
		Ts:    1715600000123456,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(ack)
	}
}

// --- Write responses --------------------------------------------------

func BenchmarkMarshal_AppendResponse(b *testing.B) {
	id := "alice"
	resp := AppendResponse{
		OK:      true,
		Created: true,
		Item: writeAck{
			Scope: "users",
			ID:    &id,
			Seq:   42,
			Ts:    1715600000123456,
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_UpsertResponse(b *testing.B) {
	id := "alice"
	resp := UpsertResponse{
		OK:      true,
		Created: false,
		Item: writeAck{
			Scope: "users",
			ID:    &id,
			Seq:   42,
			Ts:    1715600000123456,
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_UpdateResponse(b *testing.B) {
	resp := UpdateResponse{OK: true, Created: false, Count: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_CounterAddResponse(b *testing.B) {
	resp := CounterAddResponse{OK: true, Created: false, Value: 7}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_DeleteResponse(b *testing.B) {
	resp := DeleteResponse{OK: true, Hit: true, Count: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_DeleteScopeResponse(b *testing.B) {
	resp := DeleteScopeResponse{OK: true, Hit: true, Count: 3}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

// --- Bulk / observability ---------------------------------------------

func BenchmarkMarshal_WipeResponse(b *testing.B) {
	resp := WipeResponse{OK: true, Scopes: 8, Items: 152, FreedMB: MB(44158)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_WarmResponse(b *testing.B) {
	resp := WarmResponse{OK: true, Scopes: 2}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_RebuildResponse(b *testing.B) {
	resp := RebuildResponse{OK: true, Scopes: 2, Items: 4}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

func BenchmarkMarshal_StatsResponse(b *testing.B) {
	resp := StatsResponse{
		OK:            true,
		Scopes:        4,
		Items:         12,
		ApproxStoreMB: MB(3564),
		LastWriteTS:   1715600000999000,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(resp)
	}
}

// --- stdlib vs goccy comparison on the hot types -------------------
// These mirror the benches above but use gojson.Marshal directly so
// the per-call cost via the production helper (jsonMarshal in
// json.go) is visible. Subtract the matching stdlib row above to
// see the savings goccy buys on each response type.

func BenchmarkMarshalGoccy_AppendResponse(b *testing.B) {
	id := "alice"
	resp := AppendResponse{
		OK: true, Created: true,
		Item: writeAck{Scope: "users", ID: &id, Seq: 42, Ts: 1715600000123456},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = gojson.Marshal(resp)
	}
}

func BenchmarkMarshalGoccy_StatsResponse(b *testing.B) {
	resp := StatsResponse{
		OK:            true,
		Scopes:        4,
		Items:         12,
		ApproxStoreMB: MB(3564),
		LastWriteTS:   1715600000999000,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = gojson.Marshal(resp)
	}
}

func BenchmarkMarshalGoccy_DeleteResponse(b *testing.B) {
	resp := DeleteResponse{OK: true, Hit: true, Count: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = gojson.Marshal(resp)
	}
}
