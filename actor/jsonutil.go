package actor

import (
	"encoding/json"
	"fmt"
)

// stringer is the subset of fmt.Stringer the small enum types in this
// package (ProposalKind, ...) satisfy, used to give them a shared,
// human-readable JSON encoding for cassette files instead of a bare int.
type stringer interface{ String() string }

// marshalStringer JSON-encodes s as its String() text.
func marshalStringer(s stringer) ([]byte, error) { return json.Marshal(s.String()) }

// unmarshalString decodes a JSON string value.
func unmarshalString(data []byte) (string, error) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	return s, nil
}

// fmtUnknownEnum formats a consistent error for an enum's UnmarshalJSON
// rejecting a value that matches none of its known String() forms.
func fmtUnknownEnum(prefix, got string) error { return fmt.Errorf("%s: %q", prefix, got) }
