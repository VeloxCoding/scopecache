// validation.go owns shape validation for every public write/delete
// entry point. Validators run at the top of each *store method, so
// both HTTP traffic (which arrives via the handler layer) and Go-API
// traffic (which arrives via *Gateway → *store) hit the same gate.
// On rejection validators return errors wrapped with ErrInvalidInput
// so handlers can map them to 400 via errors.Is.
//
// Conventions baked in here:
//
//   - Top-level validators (validateWriteItem, validateUpsertItem,
//     validateUpdateItem, validateCounterAddRequest, validateDelete*)
//     defer wrapValidation so every return path picks up the
//     ErrInvalidInput wrap. Add a new validator with the same shape.
//   - Reads are permissive: invalid scope/id at the Gateway boundary
//     silently miss with hit=false. Strict shape validation runs
//     only at the HTTP layer (parseLookupTarget); validators in this
//     file are the write/delete path's gates.
//   - HTTP status mapping is the handler's responsibility: 400 for
//     ErrInvalidInput, 409 for *CounterPayloadError /
//     *ScopeDetachedError, 507 for *ScopeFullError /
//     *ScopeCapacityError / *StoreFullError. Validators only signal
//     "shape is wrong"; capacity / conflict signals come from store.go.
//   - checkItemSize owns the renderBytes precompute for write paths,
//     so the normal validated write path does not re-run
//     json.Unmarshal on the same bytes. Buffer-write code keeps a
//     defensive recompute for internal callers / tests that bypass
//     the validator.

package scopecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalidInput is the sentinel every top-level validator wraps
// failures with, so handlers map shape errors to 400 via errors.Is
// without per-validator type inspection. The wrapped reason string is
// preserved for diagnostic output (badRequest writes the full chain
// to the response body).
var ErrInvalidInput = errors.New("scopecache: invalid input")

// wrapValidation tags a non-nil error as a validation failure by
// wrapping it with ErrInvalidInput. Top-level validators call this
// via deferred return so every path picks up the wrap.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
}

// checkKeyField enforces the shape rules for scope/id strings: length
// cap, no surrounding whitespace, no embedded control characters,
// must be valid UTF-8. Rejecting control bytes avoids log/URL
// poisoning and keeps values safe to splice into diagnostic output;
// rejecting invalid UTF-8 prevents JSON-marshal silently rewriting
// malformed bytes to U+FFFD, which would break round-tripping
// (input "\xff" returns as "�").
//
// The control-char check iterates BYTES, not runes. Range-over-string
// would yield utf8.RuneError (0xFFFD) for malformed UTF-8, which is
// >0x7f and would pass the check even though a raw 0x00..0x1f byte
// was present. Byte iteration catches those regardless of UTF-8
// validity; the separate utf8.ValidString below covers the
// high-byte case so valid multi-byte UTF-8 passes through but
// malformed sequences are rejected.
func checkKeyField(fieldName, value string, maxLen int) error {
	if len(value) > maxLen {
		return errors.New("the '" + fieldName + "' field must not exceed " + strconv.Itoa(maxLen) + " bytes")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("the '" + fieldName + "' field must not have leading or trailing whitespace")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return errors.New("the '" + fieldName + "' field must not contain control characters")
		}
	}
	if !utf8.ValidString(value) {
		return errors.New("the '" + fieldName + "' field must be valid UTF-8")
	}
	return nil
}

func validateScope(scope, endpoint string) error {
	if scope == "" {
		return errors.New("the 'scope' field is required for the '" + endpoint + "' endpoint")
	}
	return checkKeyField("scope", scope, MaxScopeBytes)
}

// validateID validates an id when one is provided. Empty id is legal
// (id is optional on /append); endpoints that require an id call
// requireID instead.
func validateID(id string) error {
	if id == "" {
		return nil
	}
	return checkKeyField("id", id, MaxIDBytes)
}

func requireID(id, endpoint string) error {
	if id == "" {
		return errors.New("the 'id' field is required for the '" + endpoint + "' endpoint")
	}
	return checkKeyField("id", id, MaxIDBytes)
}

// validateIDOrSeq enforces the "exactly one of id or seq" addressing
// contract used by /update and /delete: passing neither (id=="" &&
// seq==0) or both is rejected. When an id is supplied, its shape is
// validated via checkKeyField; seq has no shape to validate beyond
// the non-zero check.
func validateIDOrSeq(endpoint, id string, seq uint64) error {
	hasID := id != ""
	hasSeq := seq != 0
	if hasID == hasSeq {
		return errors.New("exactly one of 'id' or 'seq' must be provided for the '" + endpoint + "' endpoint")
	}
	if hasID {
		return checkKeyField("id", id, MaxIDBytes)
	}
	return nil
}

// validatePayload enforces the three-part payload contract (RFC
// §4.1): payload must be present (not missing, not literal `null`),
// must be syntactically valid JSON, and must be valid UTF-8.
//
// The HTTP path's encoding/json decode catches malformed JSON during
// the structural scan that populates RawMessage, but direct Gateway
// callers (Append / Upsert / Update / Warm / Rebuild) hand the slice
// in as-is. Without the explicit json.Valid check, invalid bytes
// would be stored opaquely and re-served by /get, /head, /tail,
// /render and `_events` envelopes, breaking any downstream consumer
// that json.Unmarshals.
//
// The UTF-8 check closes the same round-trip-corruption hazard
// checkKeyField guards on scope/id: json.Valid accepts a bare 0x80
// inside a JSON string (it's syntactically a string), but on
// re-marshal encoding/json silently rewrites those bytes to U+FFFD
// — input "\x80" comes back as "�" on the next /get. Both checks
// are linear scans; json.Valid runs first as the cheaper structural
// gate.
func validatePayload(p json.RawMessage) error {
	if len(p) == 0 || bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
		return errors.New("the 'payload' field is required")
	}
	if !jsonValid(p) {
		return errors.New("the 'payload' field must be a valid JSON value")
	}
	if !utf8.Valid(p) {
		return errors.New("the 'payload' field must be valid UTF-8")
	}
	return nil
}

// checkItemSize measures the post-write item shape (Payload +
// renderBytes) against the per-item cap. The validator owns the
// renderBytes precompute: it sets the field once here, and downstream
// buffer-write paths reuse it instead of re-running json.Unmarshal on
// the same bytes (a saved alloc proportional to the decoded length).
//
// PRECONDITION: item must already have passed validatePayload — the
// JSON-validity invariant lets precomputeRenderBytes treat the input
// as well-formed.
func checkItemSize(item *Item, maxItemBytes int64) error {
	if item.renderBytes == nil {
		item.renderBytes = precomputeRenderBytes(item.Payload)
	}
	size := approxItemSize(*item)
	if size > maxItemBytes {
		return errors.New("the item's approximate size (" + strconv.FormatInt(size, 10) +
			" bytes) exceeds the maximum of " + strconv.FormatInt(maxItemBytes, 10) + " bytes")
	}
	return nil
}

func normalizeLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("the 'limit' parameter must be a positive integer")
	}

	if n > MaxLimit {
		return MaxLimit, nil
	}

	return n, nil
}

func normalizeOffset(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("the 'offset' parameter must be a non-negative integer")
	}

	return n, nil
}

func normalizeHours(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("the 'hours' parameter must be a non-negative integer")
	}

	// The caller multiplies hours by μs/hour (3.6e9). Values above this
	// threshold would overflow int64 during that multiplication — the
	// practical ceiling is still ~2.9 million years, far beyond any
	// sensible age filter.
	const usPerHour = int64(time.Hour / time.Microsecond)
	if n > math.MaxInt64/usPerHour {
		return 0, errors.New("the 'hours' parameter is unreasonably large")
	}

	return n, nil
}

// rejectClientTs catches a non-zero Ts on a write request body. Ts is
// cache-owned (see Item docstring in types.go); a clear 400 here is
// more useful than silently overwriting. ts=0 is JSON-decode's "field
// absent" default and passes through — clients that explicitly send
// ts=0 (1970-01-01) get the same treatment as omitting the field;
// the buffer stamps now() either way.
func rejectClientTs(item Item, endpoint string) error {
	if item.Ts != 0 {
		return errors.New("the 'ts' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	return nil
}

func validateWriteItem(item *Item, endpoint string, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, endpoint); err != nil {
		return err
	}
	if err := validateID(item.ID); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	if err := rejectClientTs(*item, endpoint); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpsertItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/upsert"); err != nil {
		return err
	}
	if err := requireID(item.ID, "/upsert"); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '/upsert' endpoint")
	}
	if err := rejectClientTs(*item, "/upsert"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpdateItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/update"); err != nil {
		return err
	}
	if err := validateIDOrSeq("/update", item.ID, item.Seq); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if err := rejectClientTs(*item, "/update"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateDeleteRequest(req deleteRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete"); err != nil {
		return err
	}
	return validateIDOrSeq("/delete", req.ID, req.Seq)
}

func validateDeleteScopeRequest(req deleteScopeRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	return validateScope(req.Scope, "/delete_scope")
}

func validateDeleteUpToRequest(req deleteUpToRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete_up_to"); err != nil {
		return err
	}
	if req.MaxSeq == 0 {
		return errors.New("the 'max_seq' field is required and must be a positive integer for the '/delete_up_to' endpoint")
	}
	return nil
}

func groupItemsByScope(items []Item) map[string][]Item {
	grouped := make(map[string][]Item)
	for _, item := range items {
		grouped[item.Scope] = append(grouped[item.Scope], item)
	}
	return grouped
}
