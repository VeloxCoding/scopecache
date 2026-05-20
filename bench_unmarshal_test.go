package scopecache

import (
	"encoding/json"
	"fmt"
	"testing"

	gojson "github.com/goccy/go-json"
)

// --- Auxiliary hot-path benches: Valid, Unmarshal-string
//
// These cover validation.go and buffer_locked.go. Both live on the
// write path (every /append, /upsert, /update); none of them was on
// the easy-to-see HTTP-envelope path that earlier rounds covered.
// stdlib vs goccy here decides whether routing the buffer-side calls
// through jsonValid / jsonUnmarshal is a real win or noise.

// bench_unmarshal_test.go — goccy/go-json-vs-stdlib comparison for
// the itemsRequest payload. /warm and /rebuild both accept this shape:
// a JSON object {"items":[{Item}, {Item}, ...]} where each Item is
// scope/id/seq/ts/payload. Realistic bulk-load sizes range from
// 100 to 10000 items.
//
// This bench backs the adoption decision recorded in CLAUDE.md
// (the "stdlib-only" exception note). On 2026-05-14 we measured
// stdlib vs easyjson vs goccy at 10k items: 8.1 ms / 3.75 ms / 1.85 ms
// respectively. easyjson was rejected on supply-chain grounds
// (Mail.ru, Russian); goccy/go-json won outright on both perf
// (4.37× stdlib, ~2× easyjson) and infrastructure (drop-in, no
// codegen).

func buildItemsRequestJSON(b *testing.B, n int) []byte {
	b.Helper()
	items := make([]Item, n)
	for i := 0; i < n; i++ {
		items[i] = Item{
			Scope:   fmt.Sprintf("scope-%d", i%10),
			ID:      fmt.Sprintf("item-%d", i),
			Seq:     uint64(i + 1),
			Ts:      1715600000000000 + int64(i),
			Payload: json.RawMessage(fmt.Sprintf(`{"idx":%d,"data":"row-%d"}`, i, i)),
		}
	}
	req := itemsRequest{Items: items}
	data, err := json.Marshal(req)
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func benchUnmarshalStdlib(b *testing.B, n int) {
	data := buildItemsRequestJSON(b, n)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req itemsRequest
		if err := json.Unmarshal(data, &req); err != nil {
			b.Fatal(err)
		}
	}
}

func benchUnmarshalGoccy(b *testing.B, n int) {
	data := buildItemsRequestJSON(b, n)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req itemsRequest
		if err := gojson.Unmarshal(data, &req); err != nil {
			b.Fatal(err)
		}
	}
}

// --- 100 items ---
func BenchmarkUnmarshal_itemsRequest_stdlib_100(b *testing.B) { benchUnmarshalStdlib(b, 100) }
func BenchmarkUnmarshal_itemsRequest_goccy_100(b *testing.B)  { benchUnmarshalGoccy(b, 100) }

// --- 1000 items ---
func BenchmarkUnmarshal_itemsRequest_stdlib_1000(b *testing.B) { benchUnmarshalStdlib(b, 1000) }
func BenchmarkUnmarshal_itemsRequest_goccy_1000(b *testing.B)  { benchUnmarshalGoccy(b, 1000) }

// --- 10000 items (decision threshold per CLAUDE.md exception note) ---
func BenchmarkUnmarshal_itemsRequest_stdlib_10000(b *testing.B) { benchUnmarshalStdlib(b, 10000) }
func BenchmarkUnmarshal_itemsRequest_goccy_10000(b *testing.B)  { benchUnmarshalGoccy(b, 10000) }

// --- json.Valid (validation.go validatePayload) ---

func BenchmarkValid_stdlib_smallObj(b *testing.B) {
	payload := []byte(`{"v":1,"name":"Alice","age":30}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = json.Valid(payload)
	}
}

func BenchmarkValid_goccy_smallObj(b *testing.B) {
	payload := []byte(`{"v":1,"name":"Alice","age":30}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gojson.Valid(payload)
	}
}

func BenchmarkValid_stdlib_1KiB(b *testing.B) {
	payload := []byte(`{"data":"` + string(make([]byte, 1024)) + `"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = json.Valid(payload)
	}
}

func BenchmarkValid_goccy_1KiB(b *testing.B) {
	payload := []byte(`{"data":"` + string(make([]byte, 1024)) + `"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gojson.Valid(payload)
	}
}

// --- Unmarshal string (buffer_locked.go precomputeRenderBytes) ---

func BenchmarkUnmarshal_renderString_stdlib(b *testing.B) {
	payload := json.RawMessage(`"<html><body><h1>Hello, world</h1></body></html>"`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s string
		_ = json.Unmarshal(payload, &s)
	}
}

func BenchmarkUnmarshal_renderString_goccy(b *testing.B) {
	payload := json.RawMessage(`"<html><body><h1>Hello, world</h1></body></html>"`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s string
		_ = gojson.Unmarshal(payload, &s)
	}
}
