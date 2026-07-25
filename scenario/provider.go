package scenario

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ScenarioProvider resolves a reference — a file path today; a store id or
// a Cloud reference tomorrow — into a loaded, validated Document. It is the
// only seam through which a Document ever enters this package's runner
// side (Build): the runner never learns where a Document came from, so a
// future store-backed or Cloud-backed provider is a drop-in replacement for
// FileScenarioProvider with no change to Build, run.Run or the CLI.
type ScenarioProvider interface {
	// Load resolves ref, then parses and validates the result exactly as
	// Parse does — see Parse's own doc comment for what "resolves" does
	// and does not do (reading ref's bytes is the only I/O Load performs;
	// nothing about the document's own content — a secret, an example bot,
	// a cassette file — is touched). A Report with at least one
	// SeverityError Issue means Load returns a non-nil error and a nil
	// Document.
	Load(ctx context.Context, ref string) (*Document, Report, error)
}

// FileScenarioProvider is the seam's only implementation today: ref is a
// local filesystem path.
type FileScenarioProvider struct {
	// Root is prepended to a relative ref. Empty resolves a relative ref
	// against the process's current working directory (os.ReadFile's own
	// behaviour).
	Root string
}

// Load implements ScenarioProvider.
func (p FileScenarioProvider) Load(_ context.Context, ref string) (*Document, Report, error) {
	path := ref
	if p.Root != "" && !filepath.IsAbs(ref) {
		path = filepath.Join(p.Root, ref)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, Report{}, fmt.Errorf("scenario: resolve %s: %w", ref, err)
	}
	data, err := os.ReadFile(abs) //nolint:gosec // caller-supplied scenario document path — the CLI's own explicit argument, not a document-declared value
	if err != nil {
		return nil, Report{}, fmt.Errorf("scenario: read %s: %w", ref, err)
	}
	doc, report, err := Parse(data)
	if err != nil {
		return nil, report, err
	}
	doc.SourceURL = "file://" + abs
	doc.BaseDir = filepath.Dir(abs)
	return doc, report, nil
}
