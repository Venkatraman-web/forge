package workerruntime

import (
	"bytes"
	"encoding"
	"encoding/json"
	"reflect"
	"unicode/utf8"
)

// encoding/json does not reject invalid UTF-8 in a Go string: it silently
// writes U+FFFD in its place, so the result Forge stores would differ from the
// one the handler returned. These helpers detect that before the value is
// serialized, without calling any user-defined marshaler a second time.
//
// How, by kind of value:
//
//   - Plain values (strings, string map keys, and strings inside structs,
//     pointers, interfaces, arrays, slices and maps) are checked by walking the
//     value with reflection before encoding. Struct fields that encoding/json
//     would not write (unexported, or tagged json:"-") are skipped.
//   - A type with its own MarshalJSON (including json.RawMessage) is opaque to
//     the walk and called exactly once, by the encoder. encoding/json copies
//     its output through unchanged, so invalid bytes it emits survive into the
//     encoded result and are caught by utf8.Valid on the final bytes.
//   - A type that only implements encoding.TextMarshaler is also opaque (calling
//     it here would run it twice). Its text is written as a JSON string, so
//     invalid bytes in it appear as the six-character escape \ufffd, which
//     encoding/json produces for nothing else: valid text, including a real
//     U+FFFD, is written as-is, and a literal backslash is written as \\. That
//     escape in the output is therefore the signature of invalid UTF-8.
//
// Fail-closed rule for the one ambiguous combination: if the value contains
// BOTH a type with its own MarshalJSON (whose output may legitimately contain a
// \ufffd escape) AND a type serialized through MarshalText (whose invalid
// output would produce that same escape), and the encoded result contains the
// escape, the encoder cannot tell the two apart without calling a marshaler a
// second time. It then REJECTS the result instead of guessing. This also
// rejects a result whose escape was legitimate; such a result is not supported.
// If the value has no text-marshaled part, or no JSON-marshaled part, or the
// output has no escape, the outcome is exact and nothing valid is rejected.
//
// Not covered, by design: a MarshalJSON method that itself calls json.Marshal
// on data containing invalid UTF-8 returns already-normalized bytes. Its output
// is trusted to be the JSON the method means to produce.

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// maxWalkDepth stops the walk on a cyclic value; encoding/json then reports the
// cycle itself.
const maxWalkDepth = 1000

// utf8Scan walks a value looking for invalid UTF-8 in strings and keys.
type utf8Scan struct {
	sawJSONMarshaler bool // an opaque MarshalJSON value was met
	sawTextMarshaler bool // a value serialized through MarshalText was met (value or map key)
}

// opaque reports whether encoding/json would hand this value to a marshaler
// method rather than encode it structurally.
func (s *utf8Scan) opaque(v reflect.Value) bool {
	t := v.Type()
	if t.Implements(jsonMarshalerType) || (v.CanAddr() && reflect.PointerTo(t).Implements(jsonMarshalerType)) {
		s.sawJSONMarshaler = true
		return true
	}
	if t.Implements(textMarshalerType) || (v.CanAddr() && reflect.PointerTo(t).Implements(textMarshalerType)) {
		s.sawTextMarshaler = true
		return true
	}
	return false
}

// invalid reports whether v holds a string or map key that is not valid UTF-8.
func (s *utf8Scan) invalid(v reflect.Value, depth int) bool {
	if depth > maxWalkDepth || !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
	}
	if s.opaque(v) {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return !utf8.ValidString(v.String())
	case reflect.Pointer, reflect.Interface:
		return s.invalid(v.Elem(), depth+1)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // []byte becomes base64
			return false
		}
		fallthrough
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if s.invalid(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			if k.Kind() == reflect.String {
				if !utf8.ValidString(k.String()) {
					return true
				}
			} else if k.Type().Implements(textMarshalerType) {
				s.sawTextMarshaler = true // written through MarshalText
			}
			if s.invalid(iter.Value(), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" && !(f.Anonymous && f.Type.Kind() == reflect.Struct) {
				continue // unexported: not written
			}
			if f.Tag.Get("json") == "-" {
				continue
			}
			if s.invalid(v.Field(i), depth+1) {
				return true
			}
		}
	}
	return false
}

// hasFFFDEscape reports whether the encoded JSON contains a real \ufffd escape
// (an odd run of backslashes before "ufffd"; an even run is escaped backslashes
// followed by ordinary text).
func hasFFFDEscape(raw []byte) bool {
	esc := []byte(`\ufffd`)
	for off := 0; ; {
		i := bytes.Index(raw[off:], esc)
		if i < 0 {
			return false
		}
		i += off
		n := 0
		for j := i - 1; j >= 0 && raw[j] == '\\'; j-- {
			n++
		}
		if n%2 == 0 { // the backslash that starts the match is itself unescaped
			return true
		}
		off = i + 1
	}
}
