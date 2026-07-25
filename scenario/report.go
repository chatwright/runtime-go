package scenario

import "strings"

// Severity classifies one Issue — see Issue.
type Severity string

// Severities. See Severity.
const (
	// SeverityError means the document is rejected: Parse returns a nil
	// Document and a non-nil error the moment any Issue in the Report is
	// SeverityError.
	SeverityError Severity = "error"
	// SeverityWarning means the document is accepted but the condition is
	// reported rather than silently accepted — e.g. noRunCeiling,
	// noIndependentVerification, a declared-but-unused secret.
	SeverityWarning Severity = "warning"
)

// Issue is one machine-readable validation finding: a rule id, the JSON
// pointer into the document it applies to, a human-readable message that
// never echoes the offending value, and a Severity. See the format's own
// "a rejection names the JSON pointer and the rule, and never echoes the
// value" requirement — Message is written by this package's own code at
// each call site, never built by interpolating a document-supplied value,
// so that requirement holds by construction, not by scrubbing after the
// fact.
type Issue struct {
	// Code is a short, stable, machine-readable rule id, e.g.
	// "inline-secret", "unsupported-capability", "ai-goal-budgets-required".
	Code string `json:"code"`
	// Pointer is an RFC 6901 JSON pointer into the document (e.g.
	// "/cast/0/provider/apiKey"), or "" for a document-level issue with no
	// single location.
	Pointer  string   `json:"pointer"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
}

// Report is every Issue Parse (and the validation it runs) found, in the
// order they were discovered.
type Report struct {
	Issues []Issue `json:"issues"`
}

// add appends issue to r.
func (r *Report) add(issue Issue) {
	r.Issues = append(r.Issues, issue)
}

// Errors returns every SeverityError Issue in r, in order.
func (r Report) Errors() []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			out = append(out, i)
		}
	}
	return out
}

// Warnings returns every SeverityWarning Issue in r, in order.
func (r Report) Warnings() []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == SeverityWarning {
			out = append(out, i)
		}
	}
	return out
}

// HasErrors reports whether r carries at least one SeverityError Issue —
// exactly the condition under which Parse rejects the whole document.
func (r Report) HasErrors() bool {
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// RejectionError is the error Parse returns for a Report with at least one
// SeverityError Issue: every error-severity Issue's pointer and code,
// joined — never the Report's Warnings, and never any document-supplied
// value (see Issue's own doc comment on why that holds by construction).
type RejectionError struct {
	Report Report
}

// Error renders every error-severity Issue as "<pointer>: <code>: <message>",
// one per line.
func (e *RejectionError) Error() string {
	errs := e.Report.Errors()
	lines := make([]string, len(errs))
	for i, issue := range errs {
		pointer := issue.Pointer
		if pointer == "" {
			pointer = "(document)"
		}
		lines[i] = pointer + ": " + issue.Code + ": " + issue.Message
	}
	return "scenario: document rejected:\n" + strings.Join(lines, "\n")
}

// asError returns a *RejectionError wrapping r when r.HasErrors(), nil
// otherwise.
func (r Report) asError() error {
	if !r.HasErrors() {
		return nil
	}
	return &RejectionError{Report: r}
}

// errorIssue builds a SeverityError Issue.
func errorIssue(code, pointer, message string) Issue {
	return Issue{Code: code, Pointer: pointer, Message: message, Severity: SeverityError}
}

// warningIssue builds a SeverityWarning Issue.
func warningIssue(code, pointer, message string) Issue {
	return Issue{Code: code, Pointer: pointer, Message: message, Severity: SeverityWarning}
}
