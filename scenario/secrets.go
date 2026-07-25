package scenario

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// defaultEnvLookup is EnvOnlyResolver's default Lookup — the one place in
// this package that reads the process environment, and only ever reached
// from Build (never from Parse — see the package doc comment).
func defaultEnvLookup(varName string) (string, bool) {
	return os.LookupEnv(varName)
}

// credentialQueryParamNames is the case/underscore-insensitive set of query
// parameter names that make a URL credential-bearing — see the README's
// secrets rule 4. normalizeParamName folds a name down to this set's own
// canonical form before comparing.
var credentialQueryParamNames = map[string]bool{
	"token":       true,
	"apikey":      true,
	"accesstoken": true,
	"key":         true,
	"password":    true,
	"secret":      true,
	"signature":   true,
}

// normalizeParamName folds name for comparison against
// credentialQueryParamNames: lower-cased, underscores removed, so "api_key",
// "apiKey" and "APIKEY" all normalize identically.
func normalizeParamName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "")
}

// scanSecretsArray validates root's top-level "secrets" member — each
// entry's name and its "from" source (exactly one of {"env"} or
// {"credential"}, no other member — see SecretSource) — and returns the set
// of declared secret names. A malformed entry is reported against its own
// pointer; scanning continues past one bad entry.
func scanSecretsArray(root map[string]any, report *Report) map[string]bool {
	declared := map[string]bool{}
	raw, ok := root["secrets"]
	if !ok {
		return declared
	}
	arr, ok := raw.([]any)
	if !ok {
		report.add(errorIssue("invalid-shape", "/secrets", "secrets must be an array"))
		return declared
	}

	seen := map[string]bool{}
	for i, item := range arr {
		pointer := fmt.Sprintf("/secrets/%d", i)
		obj, ok := item.(map[string]any)
		if !ok {
			report.add(errorIssue("invalid-shape", pointer, "each secret must be an object"))
			continue
		}

		name, _ := obj["name"].(string)
		if name == "" {
			report.add(errorIssue("invalid-shape", pointer+"/name", "secret name is required and must be a non-empty string"))
		} else {
			if seen[name] {
				report.add(errorIssue("duplicate-id", pointer+"/name", "duplicate secret name"))
			}
			seen[name] = true
			declared[name] = true
		}

		validateSecretSource(obj["from"], pointer+"/from", report)
	}
	return declared
}

// validateSecretSource enforces SecretSource's shape: exactly one of "env"
// or "credential", each a non-empty string, and no other member (no
// "value", "default" or "fallback" — see the README's secrets rule 2).
func validateSecretSource(raw any, pointer string, report *Report) {
	if raw == nil {
		report.add(errorIssue("secret-source-shape", pointer, `"from" is required`))
		return
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		report.add(errorIssue("secret-source-shape", pointer, `"from" must be an object`))
		return
	}

	envV, hasEnv := obj["env"]
	credV, hasCred := obj["credential"]
	if len(obj) != 1 || hasEnv == hasCred {
		report.add(errorIssue("secret-source-shape", pointer,
			`must be exactly one of {"env": "VAR"} or {"credential": "name"}, with no other member (no "value", "default" or "fallback")`))
		return
	}
	if hasEnv {
		if s, ok := envV.(string); !ok || s == "" {
			report.add(errorIssue("secret-source-shape", pointer+"/env", "env must be a non-empty string"))
		}
	}
	if hasCred {
		if s, ok := credV.(string); !ok || s == "" {
			report.add(errorIssue("secret-source-shape", pointer+"/credential", "credential must be a non-empty string"))
		}
	}
}

// scanSecretBearingFields validates every field in the closed list of
// secret-bearing locations — bot.headers[*].value, cast[*].provider.apiKey
// and cast[*].provider.wraps.apiKey (the README's secrets rule 1) — and
// returns the secretRef names actually used, each mapped to every pointer
// that used it (so crossCheckSecretRefs can report every offending
// location, not just the first).
func scanSecretBearingFields(root map[string]any, report *Report) map[string][]string {
	used := map[string][]string{}

	if bot, ok := root["bot"].(map[string]any); ok {
		if headers, ok := bot["headers"].(map[string]any); ok {
			for _, key := range sortedKeys(headers) {
				pointer := "/bot/headers/" + escapePointerToken(key)
				checkSecretRefShape(headers[key], pointer, report, used)
			}
		}
	}

	if cast, ok := root["cast"].([]any); ok {
		for i, item := range cast {
			member, ok := item.(map[string]any)
			if !ok {
				continue
			}
			provider, ok := member["provider"].(map[string]any)
			if !ok {
				continue
			}
			base := fmt.Sprintf("/cast/%d/provider", i)
			if v, present := provider["apiKey"]; present {
				checkSecretRefShape(v, base+"/apiKey", report, used)
			}
			if wraps, ok := provider["wraps"].(map[string]any); ok {
				if v, present := wraps["apiKey"]; present {
					checkSecretRefShape(v, base+"/wraps/apiKey", report, used)
				}
			}
		}
	}

	return used
}

// checkSecretRefShape enforces that v is exactly {"secretRef": "<name>"} —
// no sibling member, no bare string literal, no other JSON type — the
// README's secrets rule 2. A literal string is reported as "inline-secret"
// specifically (a stronger, more actionable diagnosis than the generic
// shape-mismatch code); every other malformed shape is "secret-ref-shape".
// Neither message, nor any Issue this function ever adds, includes v's own
// value.
func checkSecretRefShape(v any, pointer string, report *Report, used map[string][]string) {
	switch val := v.(type) {
	case string:
		report.add(errorIssue("inline-secret", pointer,
			`a literal value is not permitted here — this field must be {"secretRef": "<name>"}, referencing a name declared in "secrets"`))
	case map[string]any:
		if len(val) != 1 {
			report.add(errorIssue("secret-ref-shape", pointer,
				`must be exactly {"secretRef": "<name>"} with no other member`))
			return
		}
		name, ok := val["secretRef"]
		if !ok {
			report.add(errorIssue("secret-ref-shape", pointer, `must be {"secretRef": "<name>"}`))
			return
		}
		s, ok := name.(string)
		if !ok || s == "" {
			report.add(errorIssue("secret-ref-shape", pointer, "secretRef must be a non-empty string naming a declared secret"))
			return
		}
		used[s] = append(used[s], pointer)
	default:
		report.add(errorIssue("secret-ref-shape", pointer, `must be {"secretRef": "<name>"}`))
	}
}

// crossCheckSecretRefs rejects a secretRef naming an undeclared secret (the
// README's secrets rule 3, first half) and reports — never rejects — a
// declared secret no field actually references (rule 3, second half).
func crossCheckSecretRefs(declared map[string]bool, used map[string][]string, report *Report) {
	for name, pointers := range used {
		if declared[name] {
			continue
		}
		for _, pointer := range pointers {
			report.add(errorIssue("secret-ref-undeclared", pointer, "secretRef names a secret not declared in \"secrets\""))
		}
	}
	for name := range declared {
		if _, ok := used[name]; !ok {
			report.add(warningIssue("secret-declared-unused", "", fmt.Sprintf("secret %q is declared but never referenced by a secretRef", name)))
		}
	}
}

// scanCredentialBearingURLs rejects a URL member (bot.url,
// cast[*].provider.baseUrl, cast[*].provider.wraps.baseUrl) that carries
// userinfo or a credential-shaped query parameter — the README's secrets
// rule 4. The offending URL is never echoed in the Issue message.
func scanCredentialBearingURLs(root map[string]any, report *Report) {
	if bot, ok := root["bot"].(map[string]any); ok {
		if u, ok := bot["url"].(string); ok && u != "" {
			checkURLForCredentials(u, "/bot/url", report)
		}
	}
	if cast, ok := root["cast"].([]any); ok {
		for i, item := range cast {
			member, ok := item.(map[string]any)
			if !ok {
				continue
			}
			provider, ok := member["provider"].(map[string]any)
			if !ok {
				continue
			}
			base := fmt.Sprintf("/cast/%d/provider", i)
			if u, ok := provider["baseUrl"].(string); ok && u != "" {
				checkURLForCredentials(u, base+"/baseUrl", report)
			}
			if wraps, ok := provider["wraps"].(map[string]any); ok {
				if u, ok := wraps["baseUrl"].(string); ok && u != "" {
					checkURLForCredentials(u, base+"/wraps/baseUrl", report)
				}
			}
		}
	}
}

// checkURLForCredentials reports raw as credential-bearing when it carries
// userinfo (//user:pass@) or a query parameter whose name normalizes to one
// of credentialQueryParamNames. An unparsable URL is reported separately
// (invalid-url) rather than silently passed through — it is still rejected,
// just under a different, more accurate code.
func checkURLForCredentials(raw, pointer string, report *Report) {
	u, err := url.Parse(raw)
	if err != nil {
		report.add(errorIssue("invalid-url", pointer, "value is not a valid URL"))
		return
	}
	if u.User != nil {
		report.add(errorIssue("credential-bearing-url", pointer,
			"URL carries userinfo (user:pass@) credentials, which a scenario document may never contain"))
		return
	}
	for param := range u.Query() {
		if credentialQueryParamNames[normalizeParamName(param)] {
			report.add(errorIssue("credential-bearing-url", pointer,
				"URL carries a credential-shaped query parameter, which a scenario document may never contain"))
			return
		}
	}
}

// SecretResolver resolves one declared Secret's actual value at Build time
// — the separate, explicit step Parse never reaches (see the package doc
// comment). Two methods, not one, so a caller can support one source
// without pretending to support the other: ResolveCredential returning
// ErrCredentialStoreUnavailable is a normal, expected outcome for a runner
// with no configured credential store, distinct from "this credential name
// does not exist in a store that does exist".
type SecretResolver interface {
	ResolveEnv(varName string) (string, error)
	ResolveCredential(credentialName string) (string, error)
}

// EnvOnlyResolver is the default SecretResolver: {"env": "VAR"} secrets
// resolve via os.Getenv (missing/empty is an error, never a silent empty
// string standing in for a credential), and {"credential": "name"} secrets
// always fail with ErrCredentialStoreUnavailable — no credential store
// exists yet in this runtime.
type EnvOnlyResolver struct {
	// Lookup overrides how an env var is read — defaults to os.LookupEnv.
	// Tests substitute a fake here so a resolver test never actually reads
	// the process environment.
	Lookup func(varName string) (string, bool)
}

// ErrCredentialStoreUnavailable is returned by EnvOnlyResolver.ResolveCredential
// for every credential name: this runtime has no credential store yet.
var ErrCredentialStoreUnavailable = fmt.Errorf("scenario: no credential store is configured in this runtime")

// ResolveEnv implements SecretResolver.
func (r EnvOnlyResolver) ResolveEnv(varName string) (string, error) {
	lookup := r.Lookup
	if lookup == nil {
		lookup = defaultEnvLookup
	}
	v, ok := lookup(varName)
	if !ok || v == "" {
		return "", fmt.Errorf("scenario: environment variable %s is not set", varName)
	}
	return v, nil
}

// ResolveCredential implements SecretResolver.
func (EnvOnlyResolver) ResolveCredential(credentialName string) (string, error) {
	return "", fmt.Errorf("%w: %s", ErrCredentialStoreUnavailable, credentialName)
}

// resolveSecret resolves one declared secret by name via resolver, given
// doc's own Secrets declarations — the shared lookup Build's header/apiKey
// resolution both use.
func resolveSecret(doc *Document, resolver SecretResolver, name string) (string, error) {
	for _, s := range doc.Secrets {
		if s.Name != name {
			continue
		}
		switch {
		case s.From.Env != "":
			return resolver.ResolveEnv(s.From.Env)
		case s.From.Credential != "":
			return resolver.ResolveCredential(s.From.Credential)
		default:
			return "", fmt.Errorf("scenario: secret %q declares neither env nor credential", name)
		}
	}
	return "", fmt.Errorf("scenario: secret %q is not declared", name)
}
