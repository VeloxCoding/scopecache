package scopecache

import "testing"

// verifyInvariants walks every scope under lock and checks coherence between
// the atomic counter, buf.bytes, the items slice, and the two indices. Any
// failure here indicates the concurrency story is broken somewhere above.
func verifyInvariants(t *testing.T, s *store) {
	t.Helper()

	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
	}
	defer func() {
		for shIdx := range s.shards {
			s.shards[shIdx].mu.RUnlock()
		}
	}()

	var totalBytesSum int64
	var scopeCount int64
	for shIdx := range s.shards {
		scopeCount += int64(len(s.shards[shIdx].scopes))
	}
	for shIdx := range s.shards {
		for name, buf := range s.shards[shIdx].scopes {
			buf.mu.RLock()

			var sum int64
			for _, it := range buf.items {
				sum += approxItemSize(*it)
			}
			if sum != buf.bytes {
				t.Errorf("scope %q: sum(approxItemSize)=%d != buf.bytes=%d", name, sum, buf.bytes)
			}

			if len(buf.bySeq) != len(buf.items) {
				t.Errorf("scope %q: len(bySeq)=%d != len(items)=%d", name, len(buf.bySeq), len(buf.items))
			}

			for i := 1; i < len(buf.items); i++ {
				if buf.items[i].Seq <= buf.items[i-1].Seq {
					t.Errorf("scope %q: items not strictly seq-ordered at %d (seq %d <= %d)",
						name, i, buf.items[i].Seq, buf.items[i-1].Seq)
					break
				}
			}

			for id, item := range buf.byID {
				bySeqItem, ok := buf.bySeq[item.Seq]
				if !ok {
					t.Errorf("scope %q: byID[%q].Seq=%d not in bySeq", name, id, item.Seq)
					continue
				}
				if bySeqItem.ID != id {
					t.Errorf("scope %q: byID[%q] points at seq=%d but bySeq[%d].ID=%q",
						name, id, item.Seq, item.Seq, bySeqItem.ID)
				}
			}

			totalBytesSum += buf.bytes
			buf.mu.RUnlock()
		}
	}

	expected := totalBytesSum + scopeCount*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != expected {
		t.Errorf("totalBytes=%d != sum(buf.bytes)=%d + %d×%d overhead = %d (drift=%d)",
			got, totalBytesSum, scopeCount, scopeBufferOverhead, expected, got-expected)
	}
	if totalBytesSum < 0 {
		t.Errorf("sum(buf.bytes)=%d is negative", totalBytesSum)
	}
}
