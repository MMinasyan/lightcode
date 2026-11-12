package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// DecodeDocument decodes exactly one JSON document into dst with the native
// strict rules — UseNumber, DisallowUnknownFields, and a second decode that
// must report EOF — so unknown members and concatenated documents fail
// uniformly. It performs no presence, null or enum validation; DecodeValue and
// the accepting caller's own rules own those.
func DecodeDocument(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("input must contain exactly one JSON document")
	}
	return nil
}

// DecodeValue decodes one generated request value and applies the generated
// value rules: required members are present, present declared members are
// non-null, generated enums hold declared values, and the one generated union
// selects exactly its discriminator variant. The accepting caller wraps the
// returned error in its own invalid-input class.
func DecodeValue(data []byte, dst any) error {
	if err := DecodeDocument(data, dst); err != nil {
		return err
	}
	return ValidateValue(data, dst)
}

// ValidateValue applies the generated value rules to one already-decoded
// value against its raw JSON bytes.
func ValidateValue(raw []byte, dst any) error {
	if dst == nil {
		return nil
	}
	value := reflect.ValueOf(dst)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	return validateDecodedValue(raw, value, "body")
}

// the generated enum types publish one Valid method over their closed set.
type enumValue interface {
	Valid() bool
}

var contentPartType = reflect.TypeOf(ContentPart{})

// rawMessageType is the one generated alias whose bytes are the opaque JSON
// value itself: the schema still requires an object, but the transport must
// never re-interpret or renumber its members.
var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// validateDecodedValue checks the decoded value against its raw JSON bytes:
// every required member (no omitempty tag) is present, every present declared
// member is non-null, every generated enum holds a declared value, and the one
// request union selects exactly its discriminator variant with the same
// closed-member validation applied to its raw bytes. Recursion covers structs,
// pointers, slices, and typed maps; arbitrary interface and raw-message map
// values stay opaque, including null and exact numbers.
func validateDecodedValue(raw []byte, v reflect.Value, path string) error {
	if !v.IsValid() || !v.CanInterface() {
		return nil
	}
	t := v.Type()
	switch {
	case t == rawMessageType:
		return nil
	case t == contentPartType:
		return validateContentPart(raw, v.Interface().(ContentPart), path)
	case t.Kind() == reflect.String:
		if enum, ok := v.Interface().(enumValue); ok && !enum.Valid() {
			return fmt.Errorf("%s %q is not a declared enum value", path, v.String())
		}
		return nil
	case t.Kind() == reflect.Ptr:
		return validateDecodedValue(raw, v.Elem(), path)
	case t.Kind() == reflect.Slice:
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return fmt.Errorf("%s must be a JSON array: %v", path, err)
		}
		for i, element := range elements {
			if isJSONNull(element) {
				return fmt.Errorf("%s[%d] must not be null", path, i)
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
			return fmt.Errorf("%s must be a JSON object: %v", path, err)
		}
		for name, member := range members {
			if isJSONNull(member) {
				return fmt.Errorf("%s[%q] must not be null", path, name)
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
		return fmt.Errorf("%s must not be null", path)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return fmt.Errorf("%s must be a JSON object: %v", path, err)
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
				return fmt.Errorf("%s is missing required member %q", path, name)
			}
			continue
		}
		if isJSONNull(member) {
			// An optional opaque raw JSON member (a pointer to the raw value
			// type) carries the null literal. A non-pointer raw member keeps
			// the ordinary non-null rule its declared schema carries.
			fieldType := v.Field(i).Type()
			if fieldType.Kind() == reflect.Ptr && fieldType.Elem() == rawMessageType {
				continue
			}
			return fmt.Errorf("%s member %q must not be null", path, name)
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
func validateContentPart(raw []byte, part ContentPart, path string) error {
	kind, err := part.Discriminator()
	if err != nil {
		return fmt.Errorf("%s is not a decodable content part: %v", path, err)
	}
	var variant any
	switch kind {
	case "text":
		var value TextPart
		err = DecodeDocument(raw, &value)
		if err == nil {
			variant = value
		}
	case "image_url":
		var value ImageURLPart
		err = DecodeDocument(raw, &value)
		if err == nil {
			variant = value
		}
	case "opaque":
		var value OpaquePart
		err = DecodeDocument(raw, &value)
		if err == nil {
			variant = value
		}
	default:
		return fmt.Errorf("%s content part kind %q is not one of text, image_url or opaque", path, kind)
	}
	if err != nil {
		return fmt.Errorf("%s %s part: %v", path, kind, err)
	}
	// The variant holds the decoded member values, including enum fields;
	// the raw bytes drive the presence and null rules.
	return validateDecodedValue(raw, reflect.ValueOf(variant), path)
}

// isJSONNull reports whether one raw JSON value is null, after whitespace.
func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
