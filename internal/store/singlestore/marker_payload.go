package singlestore

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// markerProjectionKeys maps each projected event kind to the payload keys
// every fold that reads the kind decodes (store.MarkerPayloadProjections).
var markerProjectionKeys = func() map[string][]string {
	out := map[string][]string{}
	for _, projection := range store.MarkerPayloadProjections {
		for _, kind := range projection.Kinds {
			out[kind] = projection.Keys
		}
	}
	return out
}()

// markerProjectPayload returns the payload the folds read: an object of the
// kind's fold keys with their raw values when a top-level key scan proves Go
// decodes exactly those values, and the compact payload otherwise. A kind
// without a projection, such as a claim, keeps its compact payload
// (component-web-task-surfaces invariant 19).
func markerProjectPayload(kind string, payload json.RawMessage) json.RawMessage {
	keys := markerProjectionKeys[kind]
	if keys == nil {
		return payload
	}
	values, ok := markerTopLevelValues(payload, keys)
	if !ok {
		return payload
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for _, key := range keys {
		value, found := values[key]
		if !found {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes()
}

// markerTopLevelValues scans the top-level members of a JSON object once and
// returns the raw value of each key in keys. It reports false, so the caller
// keeps the compact payload, when the payload is not one well-formed object,
// when a key in keys appears twice, or when any other member name could
// match a key the way Go's decoder matches field names: a name that equals a
// key under case folding, or any name with a non-ASCII rune, which Go's
// Unicode folding can map to an ASCII field name. Raw values keep their bytes
// and JSON types.
func markerTopLevelValues(payload json.RawMessage, keys []string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	values := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, false
		}
		name, ok := token.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, false
		}
		if !markerASCIIName(name) {
			return nil, false
		}
		for _, key := range keys {
			if name == key {
				if _, seen := values[key]; seen {
					return nil, false
				}
				values[key] = value
			} else if strings.EqualFold(name, key) {
				return nil, false
			}
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, false
	}
	return values, true
}

func markerASCIIName(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
