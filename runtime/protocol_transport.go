package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the one transport boundary of the mounted protocol server:
// request decoding, request-shape validation, response writing, error
// classification, and response revision qualification. Everything reuses the
// generated protocol types' own JSON tags, pointer/omitempty shape, and
// generated enum validators; no handwritten request DTO or validation
// dependency exists, and no credential, request body, or header value is ever
// rendered into an error or a log.

// errUnauthorized is the one authentication failure class: the credential
// never rides the message, and no presented header value is echoed.
var errUnauthorized = errors.New("missing or invalid bearer credential")

// decodeRequestBody reads one uncapped request body as exactly one JSON
// document and decodes it natively into the generated request type:
// json.Number preserves exact numbers, DisallowUnknownFields rejects unknown
// members, and a second decode must report EOF so concatenated documents
// fail. The decoded value is then validated for required-member presence,
// non-null declared members, closed-union shape, and generated enum values.
// Every failure wraps the shared invalid-input sentinel.
func decodeRequestBody(r *http.Request, dst any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return invalidRequest("read request body: %v", err)
	}
	if err := decodeOneDocument(data, dst); err != nil {
		return invalidRequest("%v", err)
	}
	return validateDecodedValue(data, reflect.ValueOf(dst).Elem(), "body")
}

// decodeOneDocument decodes one complete JSON document into dst with the
// native strict rules — UseNumber, DisallowUnknownFields, and a second
// decode that must report EOF — so unknown members and concatenated
// documents fail uniformly. It performs no presence or enum validation;
// validateDecodedValue owns those.
func decodeOneDocument(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("input must contain exactly one JSON document")
	}
	return nil
}

// decodeEmptyBody decodes the one request shape whose members are all
// forbidden: the generated empty-map alias. The body must be one JSON object
// carrying no members; null and any member are invalid.
func decodeEmptyBody(r *http.Request, dst *map[string]interface{}) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return invalidRequest("read request body: %v", err)
	}
	if isJSONNull(data) {
		return invalidRequest("request body must be a JSON object, not null")
	}
	if err := decodeOneDocument(data, dst); err != nil {
		return invalidRequest("%v", err)
	}
	if len(*dst) != 0 {
		return invalidRequest("request body must be an empty object")
	}
	return nil
}

// invalidRequest wraps one transport-shape rejection in the shared invalid
// input sentinel, so the error mapper classifies every decode and validation
// failure as one class.
func invalidRequest(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), harness.ErrInvalid)
}

// isJSONNull reports whether one raw JSON value is null, after whitespace.
func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// The generated enum types publish one Valid method over their closed set.
type enumValue interface {
	Valid() bool
}

var contentPartType = reflect.TypeOf(protocol.ContentPart{})

// validateDecodedValue checks the decoded request value against its raw JSON
// bytes: every required member (no omitempty tag) is present, every present
// declared member is non-null (no request member is nullable), every
// generated enum holds a declared value, and the one request union selects
// exactly its discriminator variant with the same closed-member validation
// applied to its raw bytes. Recursion covers structs, pointers, slices, and
// typed maps; arbitrary interface and raw-message map values stay opaque,
// including null and exact numbers.
func validateDecodedValue(raw []byte, v reflect.Value, path string) error {
	if !v.IsValid() || !v.CanInterface() {
		return nil
	}
	t := v.Type()
	switch {
	case t == contentPartType:
		return validateContentPart(raw, v.Interface().(protocol.ContentPart), path)
	case t.Kind() == reflect.String:
		if enum, ok := v.Interface().(enumValue); ok && !enum.Valid() {
			return invalidRequest("%s %q is not a declared enum value", path, v.String())
		}
		return nil
	case t.Kind() == reflect.Ptr:
		return validateDecodedValue(raw, v.Elem(), path)
	case t.Kind() == reflect.Slice:
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return invalidRequest("%s must be a JSON array: %v", path, err)
		}
		for i, element := range elements {
			if isJSONNull(element) {
				return invalidRequest("%s[%d] must not be null", path, i)
			}
			if err := validateDecodedValue(element, v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case t.Kind() == reflect.Map:
		elem := t.Elem()
		if elem.Kind() == reflect.Interface || (elem.Kind() == reflect.Slice && elem.Elem().Kind() == reflect.Uint8) {
			return nil // opaque map values, null and exact numbers included
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return invalidRequest("%s must be a JSON object: %v", path, err)
		}
		for name, member := range members {
			if isJSONNull(member) {
				return invalidRequest("%s[%q] must not be null", path, name)
			}
			if err := validateDecodedValue(member, v.MapIndex(reflect.ValueOf(name)), fmt.Sprintf("%s[%q]", path, name)); err != nil {
				return err
			}
		}
		return nil
	case t.Kind() == reflect.Struct:
		return validateStructMembers(raw, v, path)
	default:
		return nil // scalars are shape-checked by the native decode
	}
}

// validateStructMembers applies the member rules of one decoded struct: the
// raw value must be a JSON object, required members (fields without the
// omitempty tag) must be present, present members must be non-null, and each
// member recurses with its decoded field value.
func validateStructMembers(raw []byte, v reflect.Value, path string) error {
	if isJSONNull(raw) {
		return invalidRequest("%s must not be null", path)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return invalidRequest("%s must be a JSON object: %v", path, err)
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() || field.Tag.Get("json") == "" || field.Tag.Get("json") == "-" {
			continue
		}
		tag := field.Tag.Get("json")
		name, rest, hasOptions := strings.Cut(tag, ",")
		omitEmpty := hasOptions && rest == "omitempty"
		member, present := members[name]
		if !present {
			if !omitEmpty {
				return invalidRequest("%s is missing required member %q", path, name)
			}
			continue
		}
		if isJSONNull(member) {
			return invalidRequest("%s member %q must not be null", path, name)
		}
		if err := validateDecodedValue(member, v.Field(i), path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

// validateContentPart selects the generated variant of the one request union
// by its discriminator and applies the same closed-member and presence
// validation to the part's raw bytes: the strict decode rejects unknown
// members, and the recursion validates the selected variant's own members.
func validateContentPart(raw []byte, part protocol.ContentPart, path string) error {
	kind, err := part.Discriminator()
	if err != nil {
		return invalidRequest("%s is not a decodable content part: %v", path, err)
	}
	var variant any
	switch kind {
	case "text":
		var value protocol.TextPart
		err = decodeOneDocument(raw, &value)
		if err == nil {
			variant = value
		}
	case "image_url":
		var value protocol.ImageURLPart
		err = decodeOneDocument(raw, &value)
		if err == nil {
			variant = value
		}
	case "opaque":
		var value protocol.OpaquePart
		err = decodeOneDocument(raw, &value)
		if err == nil {
			variant = value
		}
	default:
		return invalidRequest("%s content part kind %q is not one of text, image_url or opaque", path, kind)
	}
	if err != nil {
		return invalidRequest("%s %s part: %v", path, kind, err)
	}
	// The variant holds the decoded member values, including enum fields;
	// the raw bytes drive the presence and null rules.
	return validateDecodedValue(raw, reflect.ValueOf(variant), path)
}

// classifyProtocolError maps one error to its schema error code and HTTP
// status by errors.Is/As over the existing typed classes, never rendered
// text. Caller-input invalidity outranks the complete-candidate configuration
// failure that may wrap it; the closed class is the Runtime's own closure
// sentinel alone, and every unclassified error — plain caller cancellation
// and deadline errors included — is internal.
func classifyProtocolError(err error) (protocol.ErrorCode, int) {
	switch {
	case errors.Is(err, errUnauthorized):
		return protocol.Unauthorized, http.StatusUnauthorized
	case errors.Is(err, harness.ErrInvalid):
		return protocol.Invalid, http.StatusBadRequest
	case errors.Is(err, harness.ErrNotFound), errors.Is(err, catalog.ErrUnknownProvider):
		return protocol.NotFound, http.StatusNotFound
	case errors.Is(err, harness.ErrConflict):
		return protocol.Conflict, http.StatusConflict
	case errors.Is(err, ErrClosed):
		return protocol.Closed, http.StatusGone
	case errors.Is(err, harness.ErrCorrupt):
		return protocol.Corrupt, http.StatusUnprocessableEntity
	case errors.Is(err, ErrConfiguration):
		return protocol.Configuration, http.StatusUnprocessableEntity
	case errors.Is(err, harness.ErrStorage):
		return protocol.Storage, http.StatusInternalServerError
	default:
		return protocol.Internal, http.StatusInternalServerError
	}
}

// writeProtocolError writes one schema-defined typed error body. The
// message is the error's own text — never a credential, header value, or
// request body — and a corrupt Session's typed identity rides the optional
// session_id member.
func writeProtocolError(w http.ResponseWriter, err error) {
	code, status := classifyProtocolError(err)
	body := protocol.Error{Code: code, Message: err.Error()}
	var corrupt *harness.CorruptionError
	if errors.As(err, &corrupt) && corrupt.SessionID != "" {
		session := corrupt.SessionID
		body.SessionId = &session
	}
	data, _ := json.Marshal(body) // plain string members; the marshal cannot fail
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeQualifiedJSON stamps the owning server's instance identity into every
// revision of the response and writes the JSON body with its status. body
// must be a pointer to the response value so qualification can address the
// struct members it traverses.
func (rt *Runtime) writeQualifiedJSON(w http.ResponseWriter, status int, body any) {
	qualifyRevisions(reflect.ValueOf(body).Elem(), rt.protocolInstance())
	data, err := json.Marshal(body)
	if err != nil {
		writeProtocolError(w, fmt.Errorf("marshal response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// The one qualification traversal may match only the three generated revision
// types; every other composite stays exactly as its producer built it.
var (
	sessionRevisionType       = reflect.TypeOf(protocol.SessionRevision{})
	configurationRevisionType = reflect.TypeOf(protocol.ConfigurationRevision{})
	warningsRevisionType      = reflect.TypeOf(protocol.WarningsRevision{})
)

// qualifyRevisions stamps the Runtime instance identity into every generated
// session, configuration, and warnings revision reachable through struct
// fields, pointers, and slices. Arbitrary maps, interfaces, and raw JSON —
// including any opaque member spelled instance_id — are never traversed or
// rewritten.
func qualifyRevisions(rv reflect.Value, instance string) {
	switch rv.Kind() {
	case reflect.Struct:
		switch rv.Type() {
		case sessionRevisionType, configurationRevisionType, warningsRevisionType:
			if field := rv.FieldByName("InstanceId"); field.CanSet() {
				field.SetString(instance)
			}
		default:
			for i := 0; i < rv.NumField(); i++ {
				if rv.Type().Field(i).IsExported() {
					qualifyRevisions(rv.Field(i), instance)
				}
			}
		}
	case reflect.Ptr:
		if !rv.IsNil() {
			qualifyRevisions(rv.Elem(), instance)
		}
	case reflect.Slice:
		for i := 0; i < rv.Len(); i++ {
			qualifyRevisions(rv.Index(i), instance)
		}
	}
}

// qualifyEvent stamps the instance identity into the revision of the three
// revisioned event variants through their generated accessors and
// constructors; every transient event is returned unchanged and inherits
// the connection's identity implicitly.
func qualifyEvent(event Event, instance string) Event {
	kind, err := event.Discriminator()
	if err != nil {
		return event
	}
	switch kind {
	case "configuration_changed":
		value, err := event.AsConfigurationChangedEvent()
		if err != nil {
			return event
		}
		value.ConfigurationRevision.InstanceId = instance
		_ = event.FromConfigurationChangedEvent(value)
	case "session_changed":
		value, err := event.AsSessionChangedEvent()
		if err != nil {
			return event
		}
		value.SessionRevision.InstanceId = instance
		_ = event.FromSessionChangedEvent(value)
	case "warning_changed":
		value, err := event.AsWarningChangedEvent()
		if err != nil {
			return event
		}
		value.WarningsRevision.InstanceId = instance
		_ = event.FromWarningChangedEvent(value)
	}
	return event
}
