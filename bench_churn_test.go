package scopecache

import (
	"encoding/json"
	"sync/atomic"
	"testing"
)

// BenchmarkStore_AppendDeleteChurn measures steady-state insert+delete
// cycles — the workload pattern where itemSlabPool actually pays off.
// 32 scopes, each goroutine appends into round-robin scope then
// deletes the item by seq. Without slab, every append heap-allocates
// a fresh *Item; with slab, the deleted *Item recycles into the next
// append, so steady-state allocation rate drops to near zero.
func BenchmarkStore_AppendDeleteChurn(b *testing.B) {
	store := newStore(Config{
		ScopeMaxItems: 100_000_000,
		MaxStoreBytes: 128 << 30,
		MaxItemBytes:  1 << 20,
	})
	const numScopes = 32
	scopes := make([]string, numScopes)
	for i := range scopes {
		scopes[i] = "churn-" + string(rune('0'+i%10)) + string(rune('a'+i/10))
		if _, err := store.getOrCreateScope(scopes[i]); err != nil {
			b.Fatalf("pre-create: %v", err)
		}
	}
	payload := json.RawMessage(`{"data":"benchmark"}`)

	var counter atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := counter.Add(1)
			scope := scopes[int(i)%numScopes]
			item, err := store.appendOne(Item{Scope: scope, Payload: payload})
			if err != nil {
				b.Fatalf("appendOne: %v", err)
			}
			if _, err := store.deleteOne(scope, "", item.Seq); err != nil {
				b.Fatalf("deleteOne: %v", err)
			}
		}
	})
}
