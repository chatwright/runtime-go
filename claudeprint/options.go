package claudeprint

import "time"

// defaultMaxTurns and defaultAllowedTools are applied when the corresponding
// Option is never called — a scenario that just wants "some working claude
// process" should not have to spell out every flag.
const (
	defaultMaxTurns     = 12
	defaultAllowedTools = "Bash,Skill,Read"
	defaultClaudeBinary = "claude"
	defaultTurnTimeout  = 5 * time.Minute
)

// Options configures the claude process claudeprint.New launches for every
// turn. Build it with the With* functions below rather than constructing it
// directly — New applies the defaults documented on each option.
type Options struct {
	// Model is passed as --model. Left empty, the flag is omitted and claude
	// falls back to its own default model.
	Model string

	// WorkDir is the working directory the claude process runs in — a
	// scratch directory outside any repository, so no ambient CLAUDE.md,
	// hooks or project settings interfere with the session under test.
	WorkDir string

	// PluginDirs is passed as one --plugin-dir flag per entry.
	PluginDirs []string

	// Env is applied on top of a copy of the current process environment
	// from which every CLAUDE*-prefixed variable has first been removed
	// (see sanitizedEnviron) — the same leak claudechannel guards against:
	// running inside a claude session ourselves must not make the child
	// look like a nested session.
	Env map[string]string

	// AllowedTools is passed as --allowedTools. Defaults to "Bash,Skill,Read".
	AllowedTools string

	// MaxTurns is passed as --max-turns. Defaults to 12.
	MaxTurns int

	// ExtraArgs are appended verbatim to the claude command line, after
	// every flag claudeprint itself sets.
	ExtraArgs []string

	// ClaudeBinary is the executable claudeprint runs. Defaults to "claude";
	// override with a fake binary in tests.
	ClaudeBinary string

	// TurnTimeout bounds a single `claude -p ...` turn. Defaults to 5
	// minutes.
	TurnTimeout time.Duration
}

// Option configures Options at claudeprint.New construction time.
type Option func(*Options)

// WithModel sets the --model flag passed to every turn.
func WithModel(model string) Option {
	return func(o *Options) { o.Model = model }
}

// WithWorkDir sets the directory the claude process runs in.
func WithWorkDir(dir string) Option {
	return func(o *Options) { o.WorkDir = dir }
}

// WithPluginDirs appends one --plugin-dir flag per directory given. Calling
// it more than once accumulates rather than replacing.
func WithPluginDirs(dirs ...string) Option {
	return func(o *Options) { o.PluginDirs = append(o.PluginDirs, dirs...) }
}

// WithEnv merges the given key/value pairs into the environment applied on
// top of the sanitized process environment. Calling it more than once
// merges rather than replacing.
func WithEnv(env map[string]string) Option {
	return func(o *Options) {
		if o.Env == nil {
			o.Env = make(map[string]string, len(env))
		}
		for k, v := range env {
			o.Env[k] = v
		}
	}
}

// WithAllowedTools overrides the --allowedTools flag (default
// "Bash,Skill,Read").
func WithAllowedTools(tools string) Option {
	return func(o *Options) { o.AllowedTools = tools }
}

// WithMaxTurns overrides the --max-turns flag (default 12).
func WithMaxTurns(n int) Option {
	return func(o *Options) { o.MaxTurns = n }
}

// WithExtraArgs appends arguments verbatim to the claude command line,
// after every flag claudeprint itself sets. Calling it more than once
// accumulates rather than replacing.
func WithExtraArgs(args ...string) Option {
	return func(o *Options) { o.ExtraArgs = append(o.ExtraArgs, args...) }
}

// WithClaudeBinary overrides the executable run in place of "claude" — the
// seam offline tests use to point at a fake stand-in script.
func WithClaudeBinary(path string) Option {
	return func(o *Options) { o.ClaudeBinary = path }
}

// WithTurnTimeout overrides the wall-clock ceiling a single turn (one
// `claude -p ...` invocation) is allowed to run before it is killed and
// reported as an error message (default 5 minutes).
func WithTurnTimeout(d time.Duration) Option {
	return func(o *Options) { o.TurnTimeout = d }
}

// defaultOptions returns Options pre-filled with every documented default.
func defaultOptions() Options {
	return Options{
		AllowedTools: defaultAllowedTools,
		MaxTurns:     defaultMaxTurns,
		ClaudeBinary: defaultClaudeBinary,
		TurnTimeout:  defaultTurnTimeout,
	}
}

// resolve applies opts on top of the documented defaults.
func resolve(opts []Option) Options {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
