package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrProtocol is returned for every accepted-stream failure: malformed SSE framing, invalid or wrong-typed chunk JSON, and body read failures all surface through it wrapping their cause. The transport/stream layer never classifies the retryability of these errors, skips a malformed event to keep going, or converts one into another physical request — that decision belongs to the caller above this boundary.
var ErrProtocol = errors.New("streaming protocol violation")

// httpStream is the accepted-model stream returned by Transport.Stream over one 2xx response body: it owns exactly that body (no second reader exists), yields parsed StreamDelta values through Recv, and releases its resources through Close called exactly once by its single consumer.
type httpStream struct {
	body      io.ReadCloser // the 2xx response body; ownership transferred from Transport.Stream at acceptance.
	reader    *bufio.Reader // buffered line framing over that one body only.
	chunkPath string        // retained wire-diagnostics chunks artifact for this exchange; "" disables appending entirely.
	done      bool          // terminal reached ([DONE], EOF, read failure) or Close called: Recv reports io.EOF from now on.
}

// newStream wraps an accepted response body with its buffered reader and diagnostics sink. It performs no I/O beyond wrapping.
func newStream(body io.ReadCloser, chunkPath string) *httpStream {
	return &httpStream{body: body, reader: bufio.NewReader(body), chunkPath: chunkPath}
}

// Recv yields the next parsed delta from this accepted stream following the fixed SSE behavior exactly: a blank line ends an event; all data lines of one event join with newlines into its payload; comment lines beginning ":" and other SSE fields are ignored; events without any data line are ignored; [DONE] returns io.EOF; raw body EOF first decodes a pending unterminated data event, if any, before the next receive reports io.EOF. Valid JSON is decoded without SDK types (only the first choice is used because requests fix n:1), every present non-null canonical field must carry its OpenAI-compatible type or parsing fails with ErrProtocol while null remains absent for optional fields, and unknown fields are preserved in extras at message/content-part/tool-call scope. Framing/JSON failures and body read errors return a typed protocol error wrapping their cause; this method never skips a malformed event nor converts an error into another request. Concurrent Recv calls are unsupported by contract; after Close it reports io.EOF.
func (s *httpStream) Recv() (StreamDelta, error) {
	if s == nil || s.body == nil || s.done { // closed or already terminal: EOF forever, exactly like the retained producer's done flag.
		return StreamDelta{}, io.EOF
	}

	var data []string // all data line values of the event being accumulated so far, in wire order; joined at its boundary.
	for {
		line, rerr := s.reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n") // CRLF and LF framing are both accepted.
		}

		switch {
		case line == "" && rerr != nil: // terminal read with no bytes this round (EOF or a body failure).
			if len(data) > 0 {
				return s.decode(data)
			} // the last data lines arrived without their terminating blank line: flush that pending event first, then EOF/failure is reported on the next receive.
			s.done = true // nothing remains after this round; later receives see only io.EOF through done (a read failure surfaces exactly once below).
			if rerr != io.EOF {
				return StreamDelta{}, protocolReadError(rerr)
			}
			return StreamDelta{}, io.EOF

		case line == "": // blank line ends an event; one without any data lines is ignored entirely (its other fields were discarded as they arrived).
			if len(data) > 0 {
				return s.decode(data)
			}
			continue

		case strings.HasPrefix(line, ":"): // SSE comment: ignored even when it shares the round of a terminal read.

		default: // every other field (event:, id:, retry:) carries no payload for this transport and is ignored; data lines accumulate their value with at most one leading space stripped after the field name — the retained rule exactly.
			if !strings.HasPrefix(line, "data:") {
				break
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}

		if rerr != nil { // terminal read that still delivered a final unterminated fragment; it was processed above as an ordinary line.
			if len(data) > 0 {
				return s.decode(data)
			} // body EOF (or failure) with data lines pending decodes them first — this is also where [DONE] without trailing newline terminates, via decode's own terminator check.
			s.done = true // no payload survived that final fragment: terminal from here on.
			if rerr != io.EOF {
				return StreamDelta{}, protocolReadError(rerr)
			}
			return StreamDelta{}, io.EOF
		}
	}
}

// Close releases this accepted stream's response body exactly once; a second call is a no-op returning nil. Recv after close reports io.EOF through its error return per the public contract. A close failure does not retroactively change any delta already delivered and never becomes another physical request — it is cleanup-only for whoever consumed this stream.
func (s *httpStream) Close() error { // the single consumer calls this exactly once; repeated closes are absorbed so deferred cleanup cannot double-close a released body.
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true // from now on Recv reports io.EOF even when the stream was closed before being fully consumed (early abort of an accepted stream).
	body := s.body
	s.body = nil // idempotence: a later Close has nothing left to release.
	s.reader = nil
	return body.Close()
}

// protocolReadError wraps one non-EOF body read failure in the typed protocol error, keeping both sentinels reachable through errors.Is/As — no retry classification is attached or implied here.
func protocolReadError(cause error) error {
	return fmt.Errorf("%w: stream body read failed: %w", ErrProtocol, cause)
}

// decode turns one accumulated event's data lines into the next delta or terminal marker: they join with newlines, [DONE] ends the stream (io.EOF), and any other payload must be complete valid JSON — its raw bytes are appended to this exchange's chunks artifact on successful decode before it is parsed. Malformed payloads return a typed protocol error wrapping their cause; they are never skipped nor retried by anything in this package.
func (s *httpStream) decode(data []string) (StreamDelta, error) { // pure over its inputs apart from the diagnostics append — no framing state changes here except [DONE]'s done flag.
	payload := strings.Join(data, "\n")         // all data lines of one event join with a single newline between values.
	if strings.TrimSpace(payload) == "[DONE]" { // retained terminator check on the joined payload: "data:[DONE]", "data: [DONE]", and multi-line joins containing it all end the stream identically.
		s.done = true
		return StreamDelta{}, io.EOF
	}

	raw := json.RawMessage(payload)
	if !json.Valid(raw) { // malformed JSON is a protocol error wrapping its decode cause — never skipped, never retried as another request.
		var probe any
		cause := json.Unmarshal([]byte(payload), &probe)
		return StreamDelta{}, fmt.Errorf("%w: stream event payload is not valid JSON (%d bytes): %w", ErrProtocol, len(raw), cause)
	}

	delta, err := parseStreamChunkRaw(raw) // typed normalization without SDK types; wrong-typed canonical fields fail here with the same sentinel.
	if err != nil {
		return StreamDelta{}, err
	}
	appendDebugChunk(s.chunkPath, raw) // retained diagnostics: only successfully decoded payloads reach this line, and any write failure is ignored inside it so results never change.
	return delta, nil
}

// parseStreamChunkRaw decodes one raw SSE JSON payload into its normalized StreamDelta without SDK types, enforcing the fixed parser contract: the top level must be a chunk object (choices array of objects plus optional usage), at most the first choice is used because requests fix n:1, every present non-null canonical field must carry its OpenAI-compatible type while null remains absent for optional fields, unknown fields are preserved in extras at message/content-part/tool-call scope through the retained denylists, omitted tool indexes receive the lowest unused index per event before any delta leaves this function, and usage normalization retains the current signed-int producer exactly: uncached = max(prompt - cached, 0), cached as reported, output = completion tokens — with no range checks or overflow policy added anywhere.
func parseStreamChunkRaw(raw json.RawMessage) (StreamDelta, error) { // shared by the stream decoder and direct parser tests/fuzzing; every retained extra value is cloned from its source bytes on insertion.
	var top struct {
		Choices []json.RawMessage `json:"choices"` // array of raw choice objects when present non-null; a wrong JSON type fails this unmarshal as a protocol error carrying the field name below.
		Usage   *rawStreamUsage   `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(raw, &top); err != nil { // top-level structural typing: choices must be an array (of objects checked per element) and usage an object when present non-null; null stays absent for both.
		return StreamDelta{}, fmt.Errorf("%w: stream event payload did not decode into the OpenAI-compatible chunk shape at field %s: %v", ErrProtocol, unmarshalField(err), err)
	}

	out := StreamDelta{}
	if top.Usage != nil { // present non-null usage normalizes through the retained arithmetic; absent and null both leave out.Usage as its zero value (unknown/not reported).
		cached := 0
		if top.Usage.PromptTokensDetails != nil {
			cached = top.Usage.PromptTokensDetails.CachedTokens
		}
		input := top.Usage.PromptTokens - cached // signed-int arithmetic retained verbatim, including its overflow behavior — the single floor below is the only clamp.
		if input < 0 {
			input = 0
		}
		usage := Usage{InputTokens: input, CachedInputTokens: cached, OutputTokens: top.Usage.CompletionTokens}
		out.Usage = &usage // one value per present non-null usage object; a later chunk's usage is what the assembler retains.
	}

	if len(top.Choices) == 0 { // no choice in this event (e.g. a trailing usage-only chunk): HasChoice stays false and nothing else of it is read.
		return out, nil
	}
	out.HasChoice = true

	var first map[string]json.RawMessage
	if err := json.Unmarshal(top.Choices[0], &first); err != nil { // the single choice used must be an object; a scalar or array element at choices[0] is not OpenAI-compatible.
		return StreamDelta{}, fmt.Errorf("%w: stream event choices[0] is not a JSON object", ErrProtocol)
	}

	if rawFinish, ok := first["finish_reason"]; ok && !isJSONNull(rawFinish) { // null and absent finish reasons are both "absent" per the fixed parser rule.
		var reason string
		if err := json.Unmarshal(rawFinish, &reason); err != nil { // a present non-null finish_reason must be its OpenAI-compatible type: JSON string.
			return StreamDelta{}, fmt.Errorf("%w: stream event choice.finish_reason has the wrong type (want string): %v", ErrProtocol, err)
		}
		out.FinishReason = reason // raw wire value retained verbatim; an empty string means no reason was reported for this chunk.
	}

	rawDeltaRaw, ok := first["delta"] // absent or null delta objects mean this choice carries no fragment data at all (e.g. a finish-only event).
	if !ok || isJSONNull(rawDeltaRaw) {
		return out, nil
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rawDeltaRaw, &rawMap); err != nil { // present non-null delta must be its OpenAI-compatible type: JSON object.
		return StreamDelta{}, fmt.Errorf("%w: stream event choice.delta is not a JSON object", ErrProtocol)
	}

	rawRole, roleErr := decodeStringField(rawMap["role"], "stream event choice.delta.role") // null stays absent; any other non-string value fails the type contract inside the helper.
	if roleErr != nil {
		return StreamDelta{}, roleErr
	}
	out.Role = rawRole // carried through as the raw wire string — role normalization is assembler work, never parser work on this delta.

	rawRefusal, refusalErr := decodeStringField(rawMap["refusal"], "stream event choice.delta.refusal")
	if refusalErr != nil {
		return StreamDelta{}, refusalErr
	}
	out.RefusalFragment = rawRefusal

	// The fieldless canonical members are type-checked here before their canonical exclusion applies: enforcement is validation-only, never retention or output.
	if _, nameErr := decodeStringField(rawMap["name"], "stream event choice.delta.name"); nameErr != nil { // present non-null name must be a JSON string even though no StreamDelta field carries it.
		return StreamDelta{}, nameErr
	}
	if _, idErr := decodeStringField(rawMap["tool_call_id"], "stream event choice.delta.tool_call_id"); idErr != nil { // same rule for call correlation identity.
		return StreamDelta{}, idErr
	}
	if fnErr := decodeObjectField(rawMap["function_call"], "stream event choice.delta.function_call"); fnErr != nil { // legacy function-call shape must be an object when present non-null.
		return StreamDelta{}, fnErr
	}

	if err := parseContentInto(rawMap, &out); err != nil { // polymorphic delta.content handled in one place so its two wire forms share every strictness rule.
		return StreamDelta{}, err
	}

	fragments, err := parseToolFragments(rawMap) // tool-call fragments with per-event index normalization inside; wrong-typed subfields fail there too.
	if err != nil {
		return StreamDelta{}, err
	}
	out.ToolFragments = fragments

	for key, value := range rawMap { // every delta-level field not canonical at message scope becomes a retained extra byte-for-byte (cloned on insertion). MessageExtra starts nil per package convention (empty == nil) and materializes lazily below.
		if isCanonicalMessageDeltaField(key) {
			continue
		}
		if out.MessageExtra == nil { // first non-canonical key: materialize the map only now so deltas without extras keep their canonical nil representation.
			out.MessageExtra = Extra{}
		}
		out.MessageExtra[key] = cloneRaw(value)
	}

	return out, nil
}

// rawStreamUsage mirrors the OpenAI usage object's numeric fields exactly as signed ints; prompt_tokens_details.cached_tokens is the only nested value this parser consumes. Absent (null/missing) sub-objects leave their producers at zero — normalizing an absent cached count to zero IS the retained behavior, and total tokens have no Usage field here because usage totals are out of scope for this phase's values.
type rawStreamUsage struct {
	PromptTokens        int `json:"prompt_tokens,omitempty"` // total prompt token count as reported (cached included per provider convention).
	CompletionTokens    int `json:"completion_tokens,omitempty"`
	TotalTokens         int `json:"total_tokens,omitempty"` // present on many providers; nothing in this package reads it further.
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// decodeStringField decodes one raw value under the fixed parser rule for string-typed canonical fields: absent and null both yield "" (absence is never an error), a present non-null JSON string yields its value, and any other present non-null type fails with ErrProtocol naming the field — wrong types are protocol errors, not silent drops.
func decodeStringField(raw json.RawMessage, field string) (string, error) { // retained legacy tolerance for null/absent plus the plan's strictness for anything that is actually supplied.
	if len(raw) == 0 || isJSONNull(raw) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s has the wrong type (want JSON string)", ErrProtocol, field) // wraps nothing further; the stdlib cause here is not actionable by callers above this boundary.
	}
	return value, nil
}

// decodeObjectField is the object-typed twin of decodeStringField: absent and null both pass (absence is never an error), a present non-null JSON object passes, and any other present non-null type fails with ErrProtocol naming the field. It checks shape only — no subfield typing or output policy lives here.
func decodeObjectField(raw json.RawMessage, field string) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var probe map[string]json.RawMessage // a scalar, string, array, or wrong-typed value fails this unmarshal as the wrong-typed canonical field it is.
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%w: %s has the wrong type (want JSON object)", ErrProtocol, field)
	}
	return nil
}

// parseContentInto normalizes delta.content into positioned content fragments under its two OpenAI wire forms: a JSON string becomes exactly one text fragment at position zero (string-form wire content), and an array of part objects uses each element's array index as its position with per-part strict typing. Absent or null content produces no fragments; any other top-level value type is a protocol error, not silently dropped data.
func parseContentInto(delta map[string]json.RawMessage, out *StreamDelta) error { // the two forms share one entry point so their strictness rules cannot drift apart in later edits.
	raw := delta["content"] // optional field: absent and null both mean no fragment data at all for this chunk.
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}

	switch bytes.TrimSpace(raw)[0] { // the first significant byte selects the wire form; json.Valid already proved the whole payload well-formed, so a quoted value here always decodes below — the error branch guards against future edits changing that assumption silently.
	case '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return fmt.Errorf("%w: stream event delta.content is not a JSON string", ErrProtocol)
		}
		out.ContentFragments = append(out.ContentFragments, ContentFragment{Position: 0, Kind: PartText, Text: text}) // one positioned fragment; accumulation across deltas happens downstream by position.

	case '[':
		var rawParts []map[string]json.RawMessage // array-form wire content uses its array index as position; every element must be a part object (a scalar or nested-array element is not OpenAI-compatible and fails here).
		if err := json.Unmarshal(raw, &rawParts); err != nil {
			return fmt.Errorf("%w: stream event delta.content is not an array of JSON objects", ErrProtocol)
		}
		for pos, rawPart := range rawParts { // positions are exactly the wire indices in order.
			frag, err := parseContentFragment(rawPart, pos)
			if err != nil {
				return fmt.Errorf("stream event delta.content part at position %d: %w", pos, err)
			}
			out.ContentFragments = append(out.ContentFragments, frag)
		}

	default: // number/boolean/object content is neither of the two OpenAI-compatible forms.
		return fmt.Errorf("%w: stream event delta.content has a wrong type (want JSON string or array of part objects)", ErrProtocol)
	}
	return nil
}

// parseContentFragment normalizes one raw content-part object into its positioned fragment under strict typing at every level: the structural wire type must be present and non-null as a JSON string when supplied, text/image_url parts carry only their kind-specific value plus non-canonical extras (the retained denylist is exactly type/text/image_url), unknown types become opaque fragments carrying that original wire type structurally with every other field preserved as part-scope extras — the same reconstruction basis the encoder uses for opaque replay.
func parseContentFragment(rawPart map[string]json.RawMessage, position int) (ContentFragment, error) { // one fragment per array element; its Position is fixed by the caller's index and never re-derived here.
	frag := ContentFragment{Position: position}

	wireType, err := decodeStringField(rawPart["type"], "content part field \"type\"") // the helper's error already names this exact field under ErrProtocol.
	if err != nil {
		return ContentFragment{}, err
	}

	switch wireType {
	case "text":
		frag.Kind = PartText
		text, textErr := decodeStringField(rawPart["text"], "content part field \"text\"") // a present non-null text must be its OpenAI-compatible JSON-string type.
		if textErr != nil {
			return ContentFragment{}, textErr
		}
		frag.Text = text

	case "image_url":
		frag.Kind = PartImageURL
		rawImg := rawPart["image_url"] // OpenAI shape: {"url":"..."} (optionally with detail) — url is the only subfield this parser consumes; absent/null image_url leaves an empty URL fragment rather than inventing one.
		if len(rawImg) > 0 && !isJSONNull(rawImg) {
			var img struct {
				URL string `json:"url"`
			} // strict object typing: a wrong-typed image_url (string/number/...) fails here as the canonical field it is; url itself must be absent/null/string.
			if err := json.Unmarshal(rawImg, &img); err != nil {
				return ContentFragment{}, fmt.Errorf("%w: content part field \"image_url\" has a wrong type (want JSON object)", ErrProtocol)
			}
			frag.URL = img.URL // an absent/null url decodes to "" through the struct default — same retained tolerance as every other optional string here.
		}

	default: // every other wire value (thinking, reasoning, ...) is provider-specific opaque data reconstructed structurally downstream; a missing or null type on any part is likewise unusable shape for this parser and fails below rather than guessing a kind.
		if wireType == "" {
			return ContentFragment{}, fmt.Errorf("%w: content part carries no usable wire type (field \"type\" must be present as a JSON string)", ErrProtocol) // the closed fragment kinds require classification; unlike legacy pass-through, an unclassifiable part is malformed input here.
		}
		frag.Kind = PartOpaque
		frag.OpaqueWireType = wireType
		for key, value := range rawPart { // opaque scope retains EVERY field except the structural wire type itself byte-for-byte as a part-scope extra (the opaque reconstruction basis): unlike canonical kinds that consume text/image_url into struct values, an unclassifiable kind has no consuming fields, so keys with those names must survive for replay rather than be dropped. Extra starts nil per package convention and materializes lazily below.
			if key == "type" {
				continue
			}
			if frag.Extra == nil { // first retained key: initialize only now so parts without extras keep their canonical nil representation rather than an empty map diverging from Clone's convention.
				frag.Extra = Extra{}
			}
			frag.Extra[key] = cloneRaw(value)
		}
		return frag, nil // opaque parts keep their full raw field set; no further canonical fields apply to them.
	}

	for key, value := range rawPart { // text/image_url scopes: non-canonical part keys become extras byte-for-byte (the retained producer's denylist at this scope). Same lazy nil-to-materialized initialization as the opaque branch above.
		if key == "type" || key == "text" || key == "image_url" {
			continue
		}
		if frag.Extra == nil { // first retained key: initialize only now so parts without extras keep their canonical nil representation rather than an empty map diverging from Clone's convention.
			frag.Extra = Extra{}
		}
		frag.Extra[key] = cloneRaw(value)
	}
	return frag, nil
}

// parseToolFragments normalizes one event's raw tool_calls array into positioned fragments: omitted indexes receive the lowest unused index in that event through the retained ensure-index rule before any fragment leaves this function (supplied values must be non-negative integer literals — wrong types fail), id/type/function subfields follow their OpenAI-compatible types with nulls staying absent, supplied function names and argument strings pass through verbatim without JSON validation of arguments, and every other per-call field stays a retained extra at tool-call scope under the retained denylist (id/type/function/index).
func parseToolFragments(delta map[string]json.RawMessage) ([]ToolCallFragment, error) { // one fragment per raw call in wire order; positions on returned fragments are always concrete non-negative values — omission is resolved here before return.
	raw := delta["tool_calls"] // optional field: absent and null both mean no tool-call data at all for this chunk.
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}

	var rawCalls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawCalls); err != nil { // the array must hold call objects; scalars or nested arrays are not OpenAI-compatible.
		return nil, fmt.Errorf("%w: stream event delta.tool_calls is not an array of JSON objects", ErrProtocol)
	}

	supplied := make([]int, len(rawCalls)) // normalized index per raw call once known.
	present := make([]bool, len(rawCalls)) // whether that call supplied its own (non-null) wire index at all.
	used := map[int]bool{}                 // every index claimed so far in this event — both explicitly and by the fill rule below — exactly like the retained producer's used set.
	for i, rawCall := range rawCalls {     // pass one: read supplied indexes strictly; duplicates are tolerated (the assembler correlates them) but each still claims its slot for filling purposes.
		idxRaw, ok := rawCall["index"] // absent or null index means omitted on the wire — normalization happens in pass two below.
		if !ok || isJSONNull(idxRaw) {
			continue
		}
		if idxRaw[0] != '-' && (idxRaw[0] < '0' || idxRaw[0] > '9') { // quoted spellings like "0" are JSON strings, not number literals — reject on the wire type itself before any numeric interpretation can absorb them through json.Number's string backing.
			return nil, fmt.Errorf("%w: stream event delta.tool_calls[%d].index has the wrong type (want non-negative integer)", ErrProtocol, i) // same typed message as every other index failure so callers classify all of these identically regardless of which spelling tripped it.
		}
		var num json.Number // strict integer typing: strings/objects/fractional values all fail here as wrong-typed canonical fields rather than being silently coerced or dropped.
		if err := json.Unmarshal(idxRaw, &num); err != nil || !isIntegerLiteral(num) {
			return nil, fmt.Errorf("%w: stream event delta.tool_calls[%d].index has the wrong type (want non-negative integer)", ErrProtocol, i)
		}
		value, perr := num.Int64() // Int64 rejects any remaining non-integer spellings of a number.
		if perr != nil || value < 0 {
			return nil, fmt.Errorf("%w: stream event delta.tool_calls[%d].index has the wrong type (want non-negative integer)", ErrProtocol, i)
		}
		supplied[i] = int(value) // cast is safe on every platform this project builds for; absurdly large indexes are malformed input that would fail long before assembly.
		present[i] = true
		used[supplied[i]] = true
	}

	next := 0 // lowest unused slot cursor for the retained fill rule (advances past claimed slots, exactly like ensureToolCallIndexes).
	fragments := make([]ToolCallFragment, 0, len(rawCalls))
	for i, rawCall := range rawCalls { // pass two: emit one fragment per call in wire order with its normalized position resolved.
		pos := supplied[i] // explicit indexes keep their own value; omitted ones claim the next free slot below before any other later fill can take it.
		if !present[i] {
			for used[next] { // retained rule: skipped forward past every already-claimed index in this event (supplied or filled).
				next++
			}
			pos = next       // the lowest unused index at that point is what omission normalizes to.
			used[pos] = true // claimed slots stay off-limits to later fills within this same event — duplicates of supplied indexes were already marked in pass one.
			next++           // advance past the slot just claimed, matching the retained producer's cursor exactly.
		}

		frag := ToolCallFragment{} // one fragment per raw call; every field below follows the fixed parser rules at tool-call scope.
		position := pos            // normalized concrete position — omission is resolved before any delta leaves this function.
		frag.Position = &position  // pointer form retained by the value contract so direct effect implementations may still express true absence; parsed fragments never do.

		id, idErr := decodeStringField(rawCall["id"], fmt.Sprintf("stream event delta.tool_calls[%d].id", i)) // stable call identity when supplied; empty means not yet reported for this fragment (assembly correlates by index/ID later).
		if idErr != nil {
			return nil, idErr
		}
		frag.ID = id

		wireType, typeErr := decodeStringField(rawCall["type"], fmt.Sprintf("stream event delta.tool_calls[%d].type", i)) // raw supplied value retained verbatim; empty means absent (the assembler applies the function default later — never this parser).
		if typeErr != nil {
			return nil, typeErr
		}
		frag.WireType = wireType

		rawFunction := rawCall["function"] // OpenAI shape: {"name":"...","arguments":"..."} with both strings; absent/null leaves an empty name and argument fragment.
		if len(rawFunction) > 0 && !isJSONNull(rawFunction) {
			var fn struct { // strict object typing at the canonical field level — a wrong type here fails before any subfield is read (string-typed subfields decode through their own types inside).
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(rawFunction, &fn); err != nil {
				return nil, fmt.Errorf("%w: stream event delta.tool_calls[%d].function has a wrong type (want JSON object)", ErrProtocol, i) // e.g. function as string/number/array; null was already handled above as absent.
			}
			frag.Name = fn.Name                  // name fragment passes through verbatim — empty pieces are legal fragments that assembly concatenates later.
			frag.ArgumentFragment = fn.Arguments // argument piece preserved byte-for-byte as a string with no JSON validation at any point on this path (the retained producer's contract).
		}

		for key, value := range rawCall { // every per-call field outside the retained tool-call denylist becomes a fragment extra byte-for-byte. Extra starts nil per package convention and materializes lazily below like its content-part siblings.
			if isCanonicalToolCallDeltaField(key) {
				continue
			}
			if frag.Extra == nil { // first retained key: initialize only now so calls without extras keep their canonical nil representation rather than an empty map diverging from Clone's convention.
				frag.Extra = Extra{}
			}
			frag.Extra[key] = cloneRaw(value)
		}

		fragments = append(fragments, frag) // wire order preserved end to end — assembly owns any reordering by normalized position later.
	}

	return fragments, nil // one fragment per started call; denial/error continuation and correlation are assembler concerns consumed from these values.
}

// isCanonicalMessageDeltaField reports whether a delta-object key belongs to the retained producer's message-delta canonical set: those keys never become MessageExtra entries even though some of them (name/tool_call_id/function_call) carry no StreamDelta field at all — name/tool_call_id are type-checked as strings and function_call as an object just before exclusion, then all three stay excluded exactly like their encoding-side denylist siblings.
func isCanonicalMessageDeltaField(key string) bool { // ported verbatim from the retained producer's parser scope list; do not widen it without a contract change since every member here suppresses one extra key on real streams.
	switch key {
	case "role", "content", "name", "tool_calls", "tool_call_id", "refusal", "function_call":
		return true
	default:
		return false
	}
}

// isCanonicalToolCallDeltaField reports whether a raw tool-call object's key belongs to the retained producer's call-scope canonical set (id/type/function/index): those keys never become fragment extras, and index in particular participates only in position normalization above it.
func isCanonicalToolCallDeltaField(key string) bool { // ported verbatim from the retained producer; its encoding-side sibling additionally drops nothing else at this scope.
	switch key {
	case "id", "type", "function", "index":
		return true
	default:
		return false
	}
}

// isIntegerLiteral reports whether a decoded JSON number literal spells an integer (no fraction or exponent spelling), which is what OpenAI-compatible index fields require before any range interpretation happens at all.
func isIntegerLiteral(num json.Number) bool { // spelled forms like "3", "-2" pass; "1e1", "0.5", "+3" do not — the retained producer's indexes are plain integers on every observed wire shape.
	s := num.String()
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	} // any other character (fraction dot, exponent e/E, sign) disqualifies the literal.
	return true
}

// unmarshalField extracts one JSON field name from an encoding/json error when present so protocol-error diagnostics can point at the offending wire key without re-parsing anything else of the payload.
func unmarshalField(err error) string { // best-effort diagnostic context only — callers still carry the full underlying cause text after it.
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return typeErr.Field
	}
	return "payload" // no field name recoverable (e.g. top-level shape mismatch): say so plainly instead of guessing at a key that was never named by the decoder.
}
