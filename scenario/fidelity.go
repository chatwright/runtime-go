package scenario

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// validateFidelity enforces the README's "Declared fidelity and
// environment" rules that are checkable from the document alone (endpoint
// profile/environment/data-sensitivity vocabulary, and the
// real-subject-requires-a-redaction-policy rule). The environment-default
// heuristic (an unrecognised host resolving to "unknown", production
// defaulting sensitivity to real-subject) is applied later, by
// ResolveFidelity — see its own doc comment on why that step is not part of
// Validate.
func validateFidelity(doc *Document, report *Report) {
	f := doc.Fidelity
	switch f.EndpointProfile {
	case EndpointProfilePlatformEmulated, EndpointProfileHeadlessEngine:
	default:
		report.add(errorIssue("invalid-shape", "/fidelity/endpointProfile", fmt.Sprintf("unrecognised endpointProfile %q", f.EndpointProfile)))
	}

	switch f.Environment {
	case "", EnvironmentDev, EnvironmentTest, EnvironmentProduction, EnvironmentUnknown:
	default:
		report.add(errorIssue("invalid-shape", "/fidelity/environment", fmt.Sprintf("unrecognised environment %q", f.Environment)))
	}

	switch f.DataSensitivity {
	case "", DataSensitivitySynthetic, DataSensitivityRealSubject:
	default:
		report.add(errorIssue("invalid-shape", "/fidelity/dataSensitivity", fmt.Sprintf("unrecognised dataSensitivity %q", f.DataSensitivity)))
	}

	if f.DataSensitivity == DataSensitivityRealSubject && strings.TrimSpace(f.RedactionPolicy) == "" {
		report.add(errorIssue("redaction-policy-required", "/fidelity/redactionPolicy",
			"dataSensitivity \"real-subject\" requires a declared redactionPolicy"))
	}
	// A document declaring real-subject on a non-production environment is
	// accepted as-is (never overridden back to synthetic) — see the
	// README: "real-subject may be declared on a test endpoint, which is
	// the case that leaks in silence otherwise".
}

// ResolvedFidelity is doc.Fidelity with every default the README's
// "Declared fidelity and environment" section describes actually applied —
// what a caller (Build, a report) should show as "what this run's fidelity
// actually is", as opposed to Document.Fidelity, which shows only what the
// author explicitly wrote.
type ResolvedFidelity struct {
	EndpointProfile string
	// Environment is doc.Fidelity.Environment when declared; otherwise the
	// configured host map (localHostEnvironments, the only one this
	// runtime ships) when the bot's own host is unambiguous; otherwise
	// EnvironmentUnknown — never guessed beyond that.
	Environment string
	// DataSensitivity is doc.Fidelity.DataSensitivity when declared;
	// otherwise DataSensitivityRealSubject when Environment is
	// EnvironmentProduction; otherwise DataSensitivitySynthetic.
	DataSensitivity string
}

// ResolveFidelity applies the README's declared-then-configured-then-
// heuristic resolution order for environment, and the environment-defaults-
// sensitivity rule, to doc — a pure function (net/url parsing only; no
// network access) kept separate from Validate because it needs doc.Bot.URL,
// which is meaningless to resolve for an exampleBot document (there is no
// host to inspect) and because "what was declared" and "what is effective"
// are different questions a caller may want to show both answers to.
func ResolveFidelity(doc *Document) ResolvedFidelity {
	f := doc.Fidelity
	env := f.Environment
	if env == "" {
		env = environmentForHost(doc.Bot.URL)
	}

	sensitivity := f.DataSensitivity
	if sensitivity == "" {
		if env == EnvironmentProduction {
			sensitivity = DataSensitivityRealSubject
		} else {
			sensitivity = DataSensitivitySynthetic
		}
	}

	return ResolvedFidelity{EndpointProfile: f.EndpointProfile, Environment: env, DataSensitivity: sensitivity}
}

// environmentForHost applies the README's "unambiguous hosts" heuristic
// (localhost, 127.0.0.1, *.localhost -> dev) to rawURL's host, or
// EnvironmentUnknown for an empty/unparsable/unrecognised host — an
// exampleBot document (no bot.url at all) always resolves to
// EnvironmentUnknown here, which is why a worked-example greetbot document
// declares "environment": "dev" explicitly rather than relying on this
// heuristic.
func environmentForHost(rawURL string) string {
	if rawURL == "" {
		return EnvironmentUnknown
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return EnvironmentUnknown
	}
	host := u.Hostname()
	switch {
	case host == "localhost", host == "127.0.0.1", strings.HasSuffix(host, ".localhost"):
		return EnvironmentDev
	default:
		return EnvironmentUnknown
	}
}

// secondsToDuration converts an authored integer-seconds budget field to
// time.Duration, the wire/runtime unit — see Budgets' own doc comment on
// why documents author whole seconds rather than nanoseconds.
func secondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// jsonUnmarshalString decodes raw as a bare JSON string into out — the
// shape a regex Condition.Value must have.
func jsonUnmarshalString(raw json.RawMessage, out *string) error {
	return json.Unmarshal(raw, out)
}
