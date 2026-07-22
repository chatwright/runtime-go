package campaign

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrUnknownBundleSchemaVersion means ReadBundle decoded a Bundle whose
// SchemaVersion is not BundleSchemaVersion — older, newer, or simply
// unrecognised. ReadBundle returns it wrapped with the version actually
// found (see ReadBundle), rather than unmarshalling the rest of a shape this
// package's current Bundle type might not agree with.
var ErrUnknownBundleSchemaVersion = errors.New("campaign: unknown bundle schema version")

// WriteBundle writes b to w as indented, human-readable JSON, terminated by
// a trailing newline — the same style actor.Cassette.Save uses for its own
// checked-in JSON files, so a Bundle is reviewable in a PR diff and
// inspectable by hand, not just by a player.
//
// Output is deterministic: encoding/json always renders a struct's fields in
// their declared order (see Bundle's own doc comment for that order) and
// always sorts a map's keys, so two calls encoding equal Bundle values
// produce byte-identical output — see TestBundleRoundTripIsDeterministic.
// WriteBundle performs no reordering or canonicalisation of its own beyond
// what json.MarshalIndent already guarantees; a Bundle's slice fields carry
// whatever order the caller assembled them in (each field's own doc comment
// states what that order is expected to be).
func WriteBundle(w io.Writer, b Bundle) error {
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("campaign: encode bundle: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := w.Write(encoded); err != nil {
		return fmt.Errorf("campaign: write bundle: %w", err)
	}
	return nil
}

// ReadBundle reads a Bundle from r. It checks SchemaVersion before trusting
// the rest of the shape: a SchemaVersion other than BundleSchemaVersion
// returns an error wrapping ErrUnknownBundleSchemaVersion (naming the
// version actually found), rather than silently unmarshalling an old or
// newer schema's fields under today's meanings — a newer SchemaVersion is
// exactly as rejected as an older one, since this package has no way to
// know a future shape is backward-compatible.
func ReadBundle(r io.Reader) (Bundle, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Bundle{}, fmt.Errorf("campaign: read bundle: %w", err)
	}

	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Bundle{}, fmt.Errorf("campaign: decode bundle: %w", err)
	}
	if probe.SchemaVersion != BundleSchemaVersion {
		return Bundle{}, fmt.Errorf("%w: found %d, want %d", ErrUnknownBundleSchemaVersion, probe.SchemaVersion, BundleSchemaVersion)
	}

	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("campaign: decode bundle: %w", err)
	}
	return b, nil
}
