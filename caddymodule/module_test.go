package caddymodule

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// validateConfig must reject negative integer directives. The standalone
// binary's env-var parsers ignore non-positive values with a warning and
// fall back to defaults; the Caddy module historically did neither, so
// `max_store_mb -1` would silently produce a -1 MiB cap and brick the
// cache.
func TestValidateConfig_RejectsNegative(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Handler)
		want string
	}{
		{"scope_max_items", func(h *Handler) { h.ScopeMaxItems = -1 }, "scope_max_items"},
		{"max_store_mb", func(h *Handler) { h.MaxStoreMB = -1 }, "max_store_mb"},
		{"max_item_mb", func(h *Handler) { h.MaxItemMB = -5 }, "max_item_mb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			tc.set(h)
			err := h.validateConfig()
			if err == nil {
				t.Fatalf("expected error for negative %s; got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending key %q", err.Error(), tc.want)
			}
		})
	}
}

// validateConfig must reject values whose unit conversion in
// Provision would silently overflow int64:
//
//   - max_store_mb / max_item_mb: `int64(value) << 20` (MiB → bytes)
//   - init_timeout_sec:           `time.Duration(value) * time.Second`
//     (seconds → nanoseconds)
//
// Pre-fix a huge value wrapped to a tiny or negative cap, falling
// back to defaults or producing a misleading "small" cap. Post-fix
// each conversion is gated by an explicit upper bound and the
// operator gets a loud configuration error.
func TestValidateConfig_RejectsOverflowOnUnitShift(t *testing.T) {
	cases := []struct {
		name      string
		set       func(*Handler)
		wantField string
	}{
		{
			name:      "max_store_mb above MiB shift bound",
			set:       func(h *Handler) { h.MaxStoreMB = maxConfigMB + 1 },
			wantField: "max_store_mb",
		},
		{
			name:      "max_item_mb above MiB shift bound",
			set:       func(h *Handler) { h.MaxItemMB = maxConfigMB + 1 },
			wantField: "max_item_mb",
		},
		{
			name:      "init_timeout_sec above seconds bound",
			set:       func(h *Handler) { h.InitTimeoutSec = int(maxConfigSec) + 1 },
			wantField: "init_timeout_sec",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			tc.set(h)
			err := h.validateConfig()
			if err == nil {
				t.Fatalf("expected overflow rejection for %s; got nil", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name the offending key %q", err.Error(), tc.wantField)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("error %q does not mention overflow", err.Error())
			}
		})
	}
}

// The values JUST AT the upper bound must be accepted — the fix is
// "> upper", not ">= upper". Confirms the boundary is inclusive on the
// safe side.
func TestValidateConfig_AcceptsValuesAtUpperBound(t *testing.T) {
	h := &Handler{
		MaxStoreMB:     maxConfigMB,
		MaxItemMB:      maxConfigMB,
		InitTimeoutSec: int(maxConfigSec),
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("at-bound config rejected: %v", err)
	}
}

// Zero is the documented sentinel for "use compile-time default" — must
// stay accepted.
func TestValidateConfig_AcceptsZero(t *testing.T) {
	h := &Handler{} // all fields zero
	if err := h.validateConfig(); err != nil {
		t.Errorf("zero config rejected: %v", err)
	}
}

// init_timeout_sec parses as a non-negative integer onto
// InitTimeoutSec. 0 (default) = no timeout. Used by Provision to
// wrap the init context with a hard deadline before exec.
func TestUnmarshalCaddyfile_InitTimeoutSec(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		input := `scopecache {
			init_timeout_sec 600
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.InitTimeoutSec != 600 {
			t.Errorf("InitTimeoutSec=%d, want 600", h.InitTimeoutSec)
		}
	})

	t.Run("rejects negative", func(t *testing.T) {
		h := &Handler{InitTimeoutSec: -1}
		if err := h.validateConfig(); err == nil {
			t.Fatal("expected error for negative init_timeout_sec; got nil")
		}
	})
}
