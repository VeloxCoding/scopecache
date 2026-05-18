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

// validateIDSeqUUID enforces the "exactly one of id, seq or uuid"
// addressing contract used by /update and /delete: passing none or
// more than one is rejected. When an id is supplied its shape is
// validated via checkKeyField; a uuid must be a canonical UUIDv7; seq
// has no shape to validate beyond the non-zero check.
func validateIDSeqUUID(endpoint, id string, seq uint64, uuid string) error {
	hasID := id != ""
	hasSeq := seq != 0
	hasUUID := uuid != ""
	n := 0
	for _, h := range [...]bool{hasID, hasSeq, hasUUID} {
		if h {
			n++
		}
	}
	if n != 1 {
		return errors.New("exactly one of 'id', 'seq' or 'uuid' must be provided for the '" + endpoint + "' endpoint")
	}
	if hasID {
		return checkKeyField("id", id, MaxIDBytes)
	}
	if hasUUID && !isValidUUIDv7(uuid) {
		return errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string for the '" + endpoint + "' endpoint")
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
	if item.UUID == "" {
		// The cache mints a 36-char uuid after validation; count it now
		// so the per-item cap is enforced on the post-write shape.
		size += uuidStringLen
	}
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

// rejectClientUUID catches a client-supplied uuid on a write that
// mints its own (/append, /upsert, /update). UUID is cache-owned
// there; a clear 400 beats silently overwriting. Empty (field absent)
// passes. /warm and /rebuild are the exception — they adopt-or-mint
// via checkWriteUUID instead of calling this.
func rejectClientUUID(item Item, endpoint string) error {
	if item.UUID != "" {
		return errors.New("the 'uuid' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	return nil
}

// checkWriteUUID enforces the per-endpoint uuid contract for the three
// validateWriteItem callers. /append mints the uuid itself, so a
// client-supplied one is rejected. /warm and /rebuild adopt-or-mint: a
// supplied uuid is kept (it must be a canonical UUIDv7), an absent one
// is minted at commit time in buildReplacementState.
func checkWriteUUID(item Item, endpoint string) error {
	if endpoint == "/append" {
		return rejectClientUUID(item, endpoint)
	}
	if item.UUID != "" && !isValidUUIDv7(item.UUID) {
		return errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string")
	}
	return nil
}

func validateWriteItem(item *Item, endpoint string, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, endpoint); err != nil {
		return err
	}
	// `_events` is cache-only; auto-populate writes go through
	// store.appendOneTrusted which bypasses this validator. External
	// callers landing here on /append cannot target `_events` —
	// otherwise drainers would see mixed-shape entries (writeEvent
	// JSON from auto-populate vs arbitrary user payloads). `_inbox`
	// stays open: it's the app-populated fan-in by design.
	if endpoint == "/append" && item.Scope == EventsScopeName {
		return errors.New("scope '" + item.Scope + "' is reserved for cache-emitted events; /append is rejected (use a user-managed scope with events_mode=full to inject)")
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
	if err := checkWriteUUID(*item, endpoint); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpsertItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/upsert"); err != nil {
		return err
	}
	if isReservedScope(item.Scope) {
		return errors.New("scope '" + item.Scope + "' is reserved; in-place mutation (/upsert) is not supported on the drain-stream scopes (use /append)")
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
	if err := rejectClientUUID(*item, "/upsert"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpdateItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/update"); err != nil {
		return err
	}
	if isReservedScope(item.Scope) {
		return errors.New("scope '" + item.Scope + "' is reserved; in-place mutation (/update) is not supported on the drain-stream scopes")
	}
	if err := validateIDSeqUUID("/update", item.ID, item.Seq, item.UUID); err != nil {
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

// validateCounterAddRequest returns the parsed `by` on success so the
// handler can pass it straight to the store without re-dereferencing.
//
// maxItemBytes is the per-item cap. Counter items have a fully-
// determined size (48 fixed overhead + len(scope) + len(id) +
// counterCellOverhead) — no payload-size variance — so the candidate
// is checkable up-front. Without this gate, counterAddSlow's create
// and promote paths would silently commit counter items past
// MaxItemBytes on small caps.
func validateCounterAddRequest(req counterAddRequest, maxItemBytes int64) (by int64, returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/counter_add"); err != nil {
		return 0, err
	}
	if isReservedScope(req.Scope) {
		return 0, errors.New("scope '" + req.Scope + "' is reserved; counters are not supported on the drain-stream scopes")
	}
	if err := requireID(req.ID, "/counter_add"); err != nil {
		return 0, err
	}
	if req.By == nil {
		return 0, errors.New("the 'by' field is required for the '/counter_add' endpoint")
	}
	by = *req.By
	if by == 0 {
		return 0, errors.New("the 'by' field must not be zero")
	}
	if by > MaxCounterValue || by < -MaxCounterValue {
		return 0, errors.New("the 'by' field must be within ±(2^53-1)")
	}
	// Cap pre-flight on the candidate counter shape: non-nil counter
	// marker so approxItemSize charges counterCellOverhead instead of
	// len(Payload). maxItemBytes <= 0 disables the check for
	// internal/test callers that exercise shape rules without
	// provisioning a realistic per-item budget.
	if maxItemBytes > 0 {
		candidate := Item{Scope: req.Scope, ID: req.ID, counter: &counterCell{}}
		// The cache mints a 36-char uuid on the counter create path;
		// count it against the cap (the candidate above is pre-mint).
		if size := approxItemSize(candidate) + uuidStringLen; size > maxItemBytes {
			return 0, fmt.Errorf("the counter item's approximate size (%d bytes) exceeds the maximum of %d bytes", size, maxItemBytes)
		}
	}
	return by, nil
}

func validateDeleteRequest(req deleteRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete"); err != nil {
		return err
	}
	return validateIDSeqUUID("/delete", req.ID, req.Seq, req.UUID)
}

func validateDeleteScopeRequest(req deleteScopeRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete_scope"); err != nil {
		return err
	}
	if isReservedScope(req.Scope) {
		return errors.New("scope '" + req.Scope + "' is reserved and cannot be deleted")
	}
	return nil
}

func validateDeleteUpToRequest(req deleteUpToRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete_up_to"); err != nil {
		return err
	}
	hasSeq := req.MaxSeq != 0
	hasUUID := req.UUID != ""
	if hasSeq == hasUUID {
		return errors.New("exactly one of 'max_seq' or 'uuid' must be provided for the '/delete_up_to' endpoint")
	}
	if hasUUID && !isValidUUIDv7(req.UUID) {
		return errors.New("the 'uuid' field must be a canonical lowercase UUIDv7 string for the '/delete_up_to' endpoint")
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
