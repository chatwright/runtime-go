package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// forbiddenMembers is the closed set of member names that make a document
// carry an executable string — see the README's "Parsing is inert, and
// refusal is explicit" section and the executable-string-members-are-
// rejected acceptance criterion. Checked at every depth, anywhere in the
// document, before any other validation runs.
var forbiddenMembers = map[string]bool{
	"command":    true,
	"script":     true,
	"shell":      true,
	"entrypoint": true,
}

// Parse validates data as a scenario-document/v1 document. It is pure: no
// file is opened beyond decoding data itself, no environment variable is
// read, no credential store is consulted, no network call is made, and no
// bot — example or otherwise — is started. See the package doc comment's
// "Parse" stage.
//
// The returned Report carries every Issue found, error and warning alike.
// The returned error is non-nil exactly when the Report has at least one
// SeverityError Issue, in which case the returned *Document is nil: no part
// of a rejected document is resolved, and the error never echoes an
// offending value (see Issue's own doc comment). A non-nil Document may
// still carry Report warnings (e.g. noRunCeiling) worth surfacing to a
// caller even though the document is perfectly valid.
func Parse(data []byte) (*Document, Report, error) {
	var report Report

	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		report.add(errorIssue("invalid-json", "", "document is not valid JSON"))
		return nil, report, report.asError()
	}

	root, ok := generic.(map[string]any)
	if !ok {
		report.add(errorIssue("invalid-shape", "", "document root must be a JSON object"))
		return nil, report, report.asError()
	}

	// Security-critical scans run first, over the generic tree, before any
	// struct decode: this is what lets a rejection name a precise JSON
	// pointer without ever depending on the strict Document shape having
	// decoded successfully, and what keeps every message built from this
	// package's own literals rather than from document-supplied values.
	scanForbiddenMembers(generic, "", &report)
	declaredSecrets := scanSecretsArray(root, &report)
	usedSecrets := scanSecretBearingFields(root, &report)
	crossCheckSecretRefs(declaredSecrets, usedSecrets, &report)
	scanCredentialBearingURLs(root, &report)

	if report.HasErrors() {
		return nil, report, report.asError()
	}

	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(data)))
	// DisallowUnknownFields: an unrecognised member is a rejection, not a
	// silent no-op — the format's own "no member is silently ignored"
	// rule (see the README's "Parsing is inert, and refusal is explicit"
	// section) applies just as much to a typo'd or made-up member as it
	// does to a whole unsupported capability. encoding/json's own error
	// for this ("json: unknown field \"x\"") names the field but not a
	// full JSON pointer to it — a known, stdlib-imposed limitation; see
	// unknownFieldName.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		if field, ok := unknownFieldName(err); ok {
			report.add(errorIssue("unknown-member", "", fmt.Sprintf("unrecognised member %q is not part of the scenario-document/v1 shape", field)))
		} else {
			report.add(errorIssue("invalid-shape", "", "document does not match the scenario-document/v1 shape"))
		}
		return nil, report, report.asError()
	}

	validate(&doc, &report)
	if report.HasErrors() {
		return nil, report, report.asError()
	}
	return &doc, report, nil
}

// scanForbiddenMembers walks v (the document's generic JSON tree)
// recursively, reporting a SeverityError Issue for every object member
// whose key is in forbiddenMembers, at any depth — see forbiddenMembers.
func scanForbiddenMembers(v any, pointer string, report *Report) {
	switch val := v.(type) {
	case map[string]any:
		for _, key := range sortedKeys(val) {
			childPointer := pointer + "/" + escapePointerToken(key)
			if forbiddenMembers[key] {
				report.add(errorIssue("executable-string-member", childPointer,
					fmt.Sprintf("member %q is never permitted in a scenario document — the parser accepts no executable strings", key)))
			}
			scanForbiddenMembers(val[key], childPointer, report)
		}
	case []any:
		for i, child := range val {
			scanForbiddenMembers(child, fmt.Sprintf("%s/%d", pointer, i), report)
		}
	}
}

// sortedKeys returns m's keys sorted ascending, so every tree walk in this
// package visits (and therefore reports) object members in a fixed,
// reproducible order regardless of Go's randomised map iteration.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// escapePointerToken escapes s per RFC 6901 for use as one JSON pointer
// path segment.
func escapePointerToken(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// unknownFieldNamePattern matches encoding/json's own DisallowUnknownFields
// error text — `json: unknown field "x"` — the only way to recover the
// offending member name, since *json.Decoder exposes no structured error
// type for it.
var unknownFieldNamePattern = regexp.MustCompile(`^json: unknown field "(.+)"$`)

// unknownFieldName extracts the offending member name from a
// DisallowUnknownFields decode error, or ("", false) for any other decode
// error (a genuine syntax/type error, reported generically instead).
func unknownFieldName(err error) (string, bool) {
	m := unknownFieldNamePattern.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	return m[1], true
}
