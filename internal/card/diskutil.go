package card

import (
	"encoding/json"
	"fmt"
)

// The parsing of diskutil's output lives here, apart from the exec calls in
// list_darwin.go, so that it is testable on any machine — which matters more
// than usual, since CI has no diskutil at all and the only way to be wrong
// about this is silently.
//
// Everything is read out of a generic map rather than a struct. diskutil's
// keys have changed spelling across macOS releases (a disk has been
// "removable" under three different names), and a key that has moved must
// cost a picker row at worst, never the whole listing.

// wholeDisks returns the whole-disk identifiers in a `diskutil list -plist`
// document: disk4, not disk4s1.
func wholeDisks(raw []byte) ([]string, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing diskutil list: %w", err)
	}
	list, _ := doc["WholeDisks"].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if id, ok := v.(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// device turns a `diskutil info -plist <id>` document into a Device, and
// reports whether it may be offered at all. A disk that is internal, not a
// whole disk, or disk0 by name is never offered: this command erases what it
// is pointed at, and the operator's own system disk must not be one keystroke
// away in a list.
func device(raw []byte) (Device, bool) {
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		return Device{}, false
	}
	id := stringAt(info, "DeviceIdentifier")
	node := stringAt(info, "DeviceNode")
	if node == "" && id != "" {
		node = "/dev/" + id
	}
	switch {
	case node == "" || id == "disk0":
		return Device{}, false
	case boolAt(info, "Internal"):
		return Device{}, false
	case !boolAt(info, "WholeDisk"):
		return Device{}, false
	case stringAt(info, "VirtualOrPhysical") == "Virtual":
		return Device{}, false
	}
	return Device{
		Path:      node,
		RawPath:   RawPath(node),
		Size:      intAt(info, "TotalSize", "Size", "IOKitSize"),
		Name:      stringAt(info, "MediaName"),
		Removable: boolAt(info, "RemovableMedia", "RemovableMediaOrExternalDevice", "Ejectable"),
	}, true
}

func stringAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// boolAt is true when any of the keys is. Several spellings are passed where
// diskutil has renamed one.
func boolAt(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		switch v := m[k].(type) {
		case bool:
			if v {
				return true
			}
		case string:
			// Older plists spell some of these "Yes"/"No".
			if v == "Yes" || v == "true" {
				return true
			}
		}
	}
	return false
}

// intAt takes the first key that carries a number; JSON from plutil has them
// as float64, and a card's size is far inside what that represents exactly.
func intAt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok && v > 0 {
			return int64(v)
		}
	}
	return 0
}
