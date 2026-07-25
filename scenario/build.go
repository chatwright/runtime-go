package scenario

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/anthropic"
	"chatwright.dev/runtime/actor/openai"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
	"chatwright.dev/runtime/telegram"
	"chatwright.dev/sdk"
)

// BuildOptions configures Build — every seam it needs beyond doc itself.
// The zero value is a usable default for an exampleBot document that
// declares no secrets (exactly the worked-example greetbot document).
type BuildOptions struct {
	// Now supplies the built run's clock — see run.Environment.Now. Nil
	// uses time.Now.
	Now func() time.Time
	// Secrets resolves a declared secret's value — see SecretResolver. Nil
	// uses EnvOnlyResolver{}; a document declaring no secrets never
	// consults it either way.
	Secrets SecretResolver
	// ExampleBots is the registry Build boots a Bot.ExampleBot from. Nil
	// uses DefaultExampleBots().
	ExampleBots ExampleBotRegistry
	// BindAddr, when set, binds the emulator to this exact address
	// (telegram.NewEmulatorAt) instead of a random local port. Needed for
	// "http"+"polling" delivery: an already-running external bot must be
	// pre-configured with the emulator's address before Build even runs —
	// see the README's "the operator points the bot's Bot API base URL at
	// the emulator". Ignored for an exampleBot document (the README: "there
	// is no authorable URL").
	BindAddr string
	// HTTPClient is the client the emulator uses to push webhook updates,
	// for "http"+"webhook" delivery. Nil uses http.DefaultClient. Ignored
	// otherwise.
	HTTPClient *http.Client
}

// Built is everything Build produced from a validated Document: a
// ready-to-execute run.Run, the roster a caller needs to assemble a run
// bundle afterwards, the doc-local-to-platform chat-id mapping, this
// document's compiled VerifySpec (nil when it declares none), its resolved
// fidelity, and a Close tearing down whatever Build itself started (an
// example bot's own server; never anything an operator started out of
// band, e.g. a real bot process polling for "http"+"polling" delivery).
type Built struct {
	Run        run.Run
	Actors     []sdk.Actor
	ChatIDs    map[string]int64 // doc-local chat id -> platform chat id
	VerifySpec *VerifySpec
	Fidelity   ResolvedFidelity
	Close      func()
}

// Build is the package's separate, explicit resolution step (see the
// package doc comment): it resolves every declared secret doc's bot
// endpoint and cast providers need, boots an example bot or wires an HTTP
// bot transport, loads any referenced cassette file, and assembles a
// run.Run ready for Run.Execute. doc must already have passed Parse's
// validation — Build does not re-validate structure, only resolves it.
//
// Build is where every side effect this format's parsing stage explicitly
// forbids is, deliberately, allowed to happen: reading an env var or
// consulting a credential store (via opts.Secrets), starting an example
// bot, reading a cassette file from disk. A caller that only wants to
// validate a document, never run it, should call Parse (or a
// ScenarioProvider's Load) and stop there.
func Build(_ context.Context, doc *Document, opts BuildOptions) (built *Built, err error) {
	now := opts.Now
	if now == nil {
		now = defaultClock(doc)
	}
	secrets := opts.Secrets
	if secrets == nil {
		secrets = EnvOnlyResolver{}
	}
	exampleBots := opts.ExampleBots
	if exampleBots == nil {
		exampleBots = DefaultExampleBots()
	}

	emu, botActor, closeBot, err := buildBotEndpoint(doc, opts, secrets, exampleBots)
	if err != nil {
		return nil, err
	}
	// Disarmed by the final `return built, nil`; every other return in this
	// function is an error path that must tear down whatever Build already
	// started.
	cleanup := closeBot
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	chatIDs := make(map[string]int64, len(doc.Chats))
	envChatIDs := make([]int64, 0, len(doc.Chats))
	for _, c := range doc.Chats {
		chatIDs[c.ID] = c.PlatformChatID
		envChatIDs = append(envChatIDs, c.PlatformChatID)
	}

	actors := []sdk.Actor{botActor}
	castProviders := make(map[string]actor.Provider, len(doc.Cast))
	for _, c := range doc.Cast {
		a, provider, err := buildCastActor(doc, c, secrets)
		if err != nil {
			return nil, err
		}
		actors = append(actors, a)
		if provider != nil {
			castProviders[c.ID] = provider
		}
	}

	parts := make([]run.Part, 0, len(doc.Parts))
	for _, p := range doc.Parts {
		part, err := buildPart(doc, p, chatIDs, castProviders)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}

	ceiling := ceilingFromDocument(doc)

	var verifySpec *VerifySpec
	if doc.Verify != nil {
		verifySpec, err = CompileVerify(doc)
		if err != nil {
			return nil, err
		}
	}

	r := run.Run{
		ID:          doc.ID,
		Environment: run.Environment{Emulator: emu, ChatIDs: envChatIDs, Now: now},
		Parts:       parts,
		Ceiling:     ceiling,
	}

	built = &Built{
		Run: r, Actors: actors, ChatIDs: chatIDs,
		VerifySpec: verifySpec, Fidelity: ResolveFidelity(doc), Close: closeBot,
	}
	cleanup = nil // success: the caller now owns Close.
	return built, nil
}

// buildPart maps one authored Part (guaranteed kind:"ai-goal" — Parse
// rejects any other kind) to a run.Part via run.NewAIGoalPart, member for
// member against run.AIGoalPartInput — see the README's "Parts, budgets and
// ceilings" section.
func buildPart(doc *Document, p Part, chatIDs map[string]int64, castProviders map[string]actor.Provider) (run.Part, error) {
	chatID, ok := chatIDs[p.Chat]
	if !ok {
		return run.Part{}, fmt.Errorf("scenario: part %q: chat %q is not declared", p.ID, p.Chat)
	}
	provider, ok := castProviders[p.ActorID]
	if !ok {
		return run.Part{}, fmt.Errorf("scenario: part %q: actor %q has no ai-agent provider", p.ID, p.ActorID)
	}
	castMember := findCast(doc, p.ActorID)
	if castMember == nil {
		return run.Part{}, fmt.Errorf("scenario: part %q: actor %q is not declared", p.ID, p.ActorID)
	}
	g, err := toGoalGoal(*p.Goal)
	if err != nil {
		return run.Part{}, fmt.Errorf("scenario: part %q: %w", p.ID, err)
	}

	cfg := actor.Config{
		ChatID: chatID,
		User: platform.User{
			ID:        castMember.PlatformIdentity.UserID,
			FirstName: castMember.PlatformIdentity.FirstName,
		},
	}
	applyLoopConfig(&cfg, p.Loop)

	return run.NewAIGoalPart(p.ID, p.Title, run.FailurePolicy(p.FailurePolicy), run.AIGoalPartInput{
		ActorID: p.ActorID, Goal: g, Provider: provider, Config: cfg,
	}), nil
}

// applyLoopConfig copies an authored LoopConfig onto cfg. A nil lc, or any
// individual zero-valued field within it, leaves the matching Config field
// at its own zero value — which actor.Config.withDefaults then turns into
// exactly the defaults the README documents (HistoryWindow 10,
// NonProgressLimit 3, ActWaitTimeout 5s), identically whether lc is nil or
// simply omits that one field.
func applyLoopConfig(cfg *actor.Config, lc *LoopConfig) {
	if lc == nil {
		return
	}
	cfg.HistoryWindow = lc.HistoryWindow
	cfg.NonProgressLimit = lc.NonProgressLimit
	cfg.ActWaitTimeout = secondsToDuration(lc.ActWaitTimeoutSeconds)
	if lc.RetainObservations != nil && !*lc.RetainObservations {
		cfg.DisableObservationRetention = true
	}
	if lc.OvershootProbe != nil && !*lc.OvershootProbe {
		cfg.DisableOvershootProbe = true
	}
}

// frozenClockEpoch is the fixed instant defaultClock returns for a
// cassette-only document — see defaultClock's own doc comment.
var frozenClockEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// defaultClock picks Build's own clock default when BuildOptions.Now is
// nil: a frozen instant for a document whose every ai-agent cast member is
// a cassette provider in replay mode, real wall-clock time.Now otherwise.
//
// This is not cosmetic. actor.CassetteProvider keys each cache entry on a
// hash of the whole actor.Prompt, which — from a task's second step
// onward — includes Prompt.History, and each carried LoopEvent.At is
// stamped from this run's own clock (see actor/loop_event.go: "so a run's
// timeline is reproducible"). A cassette recorded once and replayed later
// under a different, real wall-clock time therefore produces a different
// History hash for every step after the first — a guaranteed cache miss —
// unless the replaying run's clock reproduces the exact instants the
// cassette was recorded with. Freezing the clock (an unchanging instant,
// so every LoopEvent.At in a run is identical) is the only clock any two
// separate cassette-replay runs of the same document can agree on without
// the document itself somehow declaring a recording timestamp — which
// would defeat the point of a zero-network, zero-cost, checked-in fixture.
// A document with a live "model" provider (or a cassette in "record" mode,
// which wraps one) gets real time instead, because its own budgets and a
// run bundle's own timestamps should reflect wall-clock reality.
func defaultClock(doc *Document) func() time.Time {
	if cassetteOnly(doc) {
		return func() time.Time { return frozenClockEpoch }
	}
	return time.Now
}

// cassetteOnly reports whether every ai-agent cast member in doc uses a
// cassette provider in replay mode — see defaultClock.
func cassetteOnly(doc *Document) bool {
	sawCassette := false
	for _, c := range doc.Cast {
		if c.Type != CastTypeAIAgent || c.Provider == nil {
			continue
		}
		if c.Provider.Kind != ProviderKindCassette || c.Provider.Mode != CassetteModeReplay {
			return false
		}
		sawCassette = true
	}
	return sawCassette
}

// ceilingFromDocument maps doc.Ceiling to run.RunCeiling — the zero value
// (no limit) when doc declares none, exactly matching run.RunCeiling's own
// "zero means no run-level ceiling" convention.
func ceilingFromDocument(doc *Document) run.RunCeiling {
	if doc.Ceiling == nil {
		return run.RunCeiling{}
	}
	return run.RunCeiling{
		MaxSteps:    doc.Ceiling.MaxSteps,
		MaxCost:     doc.Ceiling.MaxCost,
		MaxDuration: secondsToDuration(doc.Ceiling.MaxDurationSeconds),
	}
}

func findCast(doc *Document, id string) *Cast {
	for i := range doc.Cast {
		if doc.Cast[i].ID == id {
			return &doc.Cast[i]
		}
	}
	return nil
}

// buildBotEndpoint boots doc.Bot — an example bot from registry, or an
// emulator wired for an HTTP transport — returning the platform.Emulator
// every Part will run against, that bot's roster sdk.Actor, and a Close
// tearing down whatever was actually started. See the README's "The bot
// endpoint" section: the bot's platform identity is always assigned by the
// emulator (telegram.EmulatedBotUserID), never authored in the document.
func buildBotEndpoint(doc *Document, opts BuildOptions, secrets SecretResolver, registry ExampleBotRegistry) (platform.Emulator, sdk.Actor, func(), error) {
	b := doc.Bot

	if b.ExampleBot != "" {
		session, err := registry.Boot(b.ExampleBot)
		if err != nil {
			return nil, sdk.Actor{}, nil, err
		}
		return session.Emulator, botRosterActor(doc, b), session.Close, nil
	}

	var emu *telegram.Emulator
	var err error
	if opts.BindAddr != "" {
		emu, err = telegram.NewEmulatorAt(opts.BindAddr)
	} else {
		emu = telegram.NewEmulator()
	}
	if err != nil {
		return nil, sdk.Actor{}, nil, fmt.Errorf("scenario: start emulator: %w", err)
	}

	if b.Transport == TransportHTTP && b.Delivery == DeliveryWebhook {
		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		if len(b.Headers) > 0 {
			headers, herr := resolveHeaders(doc, b.Headers, secrets)
			if herr != nil {
				emu.Close()
				return nil, sdk.Actor{}, nil, herr
			}
			client = &http.Client{
				Transport: headerRoundTripper{base: transportOf(client), headers: headers},
				Timeout:   client.Timeout,
			}
		}
		emu.SetWebhook(b.URL, client)
	}
	// delivery=="polling": nothing further to do here — the operator's own
	// bot process polls this emulator's BotAPIURL()/opts.BindAddr,
	// configured out of band (the README: "the operator points the bot's
	// Bot API base URL at the emulator ... documented rather than
	// automated").

	return emu, botRosterActor(doc, b), emu.Close, nil
}

func botRosterActor(doc *Document, b Bot) sdk.Actor {
	return sdk.Actor{
		ID: b.ID, Type: sdk.ActorBot, Name: b.Name,
		PlatformIdentities: map[string]sdk.PlatformIdentity{
			doc.Platform: {UserID: telegram.EmulatedBotUserID, FirstName: b.Name},
		},
	}
}

// headerRoundTripper adds a fixed set of resolved header values to every
// outbound request — how a webhook-delivery bot's declared, secret-backed
// headers (Bot.Headers) actually reach the wire, without this package ever
// needing to build request bytes itself.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	for k, v := range t.headers {
		cloned.Header.Set(k, v)
	}
	return t.base.RoundTrip(cloned)
}

func transportOf(client *http.Client) http.RoundTripper {
	if client.Transport != nil {
		return client.Transport
	}
	return http.DefaultTransport
}

func resolveHeaders(doc *Document, headers map[string]SecretRef, resolver SecretResolver) (map[string]string, error) {
	out := make(map[string]string, len(headers))
	for name, ref := range headers {
		v, err := resolveSecret(doc, resolver, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("scenario: header %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// buildCastActor converts one authored Cast member into its roster
// sdk.Actor and, for an ai-agent, the actor.Provider it drives its part
// with (nil for a human cast member — v1 authors nothing else, see the
// README's "scripted is deliberately not expressible").
func buildCastActor(doc *Document, c Cast, secrets SecretResolver) (sdk.Actor, actor.Provider, error) {
	identity := map[string]sdk.PlatformIdentity{
		doc.Platform: {UserID: c.PlatformIdentity.UserID, FirstName: c.PlatformIdentity.FirstName},
	}

	switch c.Type {
	case CastTypeHuman:
		return sdk.Actor{ID: c.ID, Type: sdk.ActorHuman, Name: c.Name, PlatformIdentities: identity}, nil, nil
	case CastTypeAIAgent:
		provider, providerName, modelIDs, err := buildProvider(doc, *c.Provider, secrets)
		if err != nil {
			return sdk.Actor{}, nil, fmt.Errorf("scenario: cast %q: %w", c.ID, err)
		}
		a := sdk.Actor{
			ID: c.ID, Type: sdk.ActorAIAgent, Name: c.Name, PlatformIdentities: identity,
			Provider: &sdk.ActorProvider{Name: providerName, ModelIDs: modelIDs},
		}
		return a, provider, nil
	default:
		return sdk.Actor{}, nil, fmt.Errorf("scenario: cast %q: unrecognised type %q", c.ID, c.Type)
	}
}

// buildProvider dispatches an authored Provider to buildModelProvider or
// buildCassetteProvider by Kind.
func buildProvider(doc *Document, p Provider, secrets SecretResolver) (actor.Provider, string, []string, error) {
	switch p.Kind {
	case ProviderKindModel:
		return buildModelProvider(doc, p, secrets)
	case ProviderKindCassette:
		return buildCassetteProvider(doc, p, secrets)
	default:
		return nil, "", nil, fmt.Errorf("unrecognised provider kind %q", p.Kind)
	}
}

// buildModelProvider constructs a live actor/openai.Provider or
// actor/anthropic.Provider from an authored model Provider, resolving its
// apiKey secretRef (if any) against doc.Secrets — the one place in this
// package that turns a resolved secret directly into an outbound-request
// credential, never into a returned or logged value.
func buildModelProvider(doc *Document, p Provider, secretsResolver SecretResolver) (actor.Provider, string, []string, error) {
	apiKey := ""
	if p.APIKey != nil {
		v, err := resolveSecret(doc, secretsResolver, p.APIKey.Name)
		if err != nil {
			return nil, "", nil, err
		}
		apiKey = v
	}

	switch p.ProviderID {
	case "anthropic":
		prov, err := anthropic.New(anthropic.Config{APIKey: apiKey, Model: p.Model, BaseURL: p.BaseURL})
		if err != nil {
			return nil, "", nil, fmt.Errorf("anthropic provider: %w", err)
		}
		return prov, p.ProviderID, []string{p.Model}, nil
	default:
		// Every other providerId (openai, ollama, lmstudio, openai-compat,
		// or an unrecognised value) speaks the OpenAI-compatible wire
		// format, matching arena.KindOpenAICompat's own fallback for any
		// server this runtime has no specific mechanism for.
		prov, err := openai.New(openai.Config{BaseURL: p.BaseURL, Model: p.Model, APIKey: apiKey})
		if err != nil {
			return nil, "", nil, fmt.Errorf("openai-compatible provider: %w", err)
		}
		return prov, p.ProviderID, []string{p.Model}, nil
	}
}

// buildCassetteProvider loads doc's cast-member cassette file (resolved
// relative to doc.BaseDir when the authored path is relative — see the
// parent feature's "references ... resolve relative to the source
// document" rule) and wraps it in a *actor.CassetteProvider. Loading a
// cassette file is I/O this package only ever performs here, in Build,
// never in Parse.
func buildCassetteProvider(doc *Document, p Provider, secrets SecretResolver) (actor.Provider, string, []string, error) {
	path := p.Cassette
	if !filepath.IsAbs(path) && doc.BaseDir != "" {
		path = filepath.Join(doc.BaseDir, path)
	}
	cassette, err := actor.LoadCassette(path)
	if err != nil {
		return nil, "", nil, fmt.Errorf("cassette provider: %w", err)
	}

	switch p.Mode {
	case CassetteModeReplay:
		cp, err := actor.NewCassetteProvider(actor.ModeReplay, nil, cassette)
		if err != nil {
			return nil, "", nil, err
		}
		return cp, "cassette", nil, nil
	case CassetteModeRecord:
		wrapped, name, modelIDs, err := buildModelProvider(doc, *p.Wraps, secrets)
		if err != nil {
			return nil, "", nil, err
		}
		cp, err := actor.NewCassetteProvider(actor.ModeRecord, wrapped, cassette)
		if err != nil {
			return nil, "", nil, err
		}
		return cp, name, modelIDs, nil
	default:
		return nil, "", nil, fmt.Errorf("unrecognised cassette mode %q", p.Mode)
	}
}
