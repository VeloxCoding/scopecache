// gateway_registry_test.go — coverage for the process-wide
// *Gateway registry. The four cases here pin the contract used by
// the caddymodule + future in-process consumers: register/lookup,
// nil-deregister, the reload-race compare-and-delete, and basic
// concurrent safety.

package scopecache

import (
	"sync"
	"testing"
)

func TestRegistry_RegisterAndLookup(t *testing.T) {
	gw := NewGateway(Config{})
	RegisterGateway("test-roundtrip", gw)
	t.Cleanup(func() { RegisterGateway("test-roundtrip", nil) })

	if got := LookupGateway("test-roundtrip"); got != gw {
		t.Fatalf("Lookup returned %v, want %v", got, gw)
	}
}

func TestRegistry_LookupMissingReturnsNil(t *testing.T) {
	if got := LookupGateway("does-not-exist-anywhere"); got != nil {
		t.Fatalf("Lookup of missing name returned %v, want nil", got)
	}
}

func TestRegistry_RegisterNilDeregisters(t *testing.T) {
	gw := NewGateway(Config{})
	RegisterGateway("test-nil-dereg", gw)
	RegisterGateway("test-nil-dereg", nil)

	if got := LookupGateway("test-nil-dereg"); got != nil {
		t.Fatalf("after nil-register Lookup returned %v, want nil", got)
	}
}

// Pins the contract that the caddymodule Cleanup path relies on:
// if a newer Provision has already overwritten the registration,
// the older instance's Cleanup must NOT clobber it.
func TestRegistry_DeregisterIfPreservesNewerRegistration(t *testing.T) {
	gwA := NewGateway(Config{})
	gwB := NewGateway(Config{})

	RegisterGateway("test-reload-race", gwA)
	RegisterGateway("test-reload-race", gwB) // simulates new Provision
	t.Cleanup(func() { RegisterGateway("test-reload-race", nil) })

	DeregisterGatewayIf("test-reload-race", gwA) // simulates old Cleanup

	if got := LookupGateway("test-reload-race"); got != gwB {
		t.Fatalf("DeregisterGatewayIf clobbered newer registration: got %v, want %v", got, gwB)
	}
}

// Sanity check that the RWMutex actually protects concurrent access.
// With -race this fires on data races; without it, it at least
// exercises the lock paths from many goroutines simultaneously.
func TestRegistry_ConcurrentSafe(t *testing.T) {
	gw := NewGateway(Config{})
	t.Cleanup(func() { RegisterGateway("test-conc", nil) })

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterGateway("test-conc", gw)
			_ = LookupGateway("test-conc")
			DeregisterGatewayIf("test-conc", gw)
		}()
	}
	wg.Wait()
}
