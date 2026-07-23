package arena

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeJSONBody decodes r's JSON body into out, failing t on error.
func decodeJSONBody(t *testing.T, r *http.Request, out any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

func TestLMStudioLoaderDegradesWhenBinaryAbsent(t *testing.T) {
	l := LMStudioLoader{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Exec: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("Exec should not be called when the lms binary is absent")
			return nil, nil
		},
	}
	result, err := l.Load(context.Background(), ProviderSpec{Model: "gemma-4-e4b"})
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (absence degrades, never fails)", err)
	}
	if result.Performed {
		t.Errorf("result.Performed = true, want false when lms is absent")
	}
	if result.Note == "" {
		t.Error("result.Note is empty, want an explanation")
	}
}

func TestLMStudioLoaderDegradesWhenUnloadFails(t *testing.T) {
	l := LMStudioLoader{
		LookPath: func(string) (string, error) { return "/usr/local/bin/lms", nil },
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "lms" || len(args) < 2 || args[0] != "unload" {
				t.Fatalf("unexpected exec: %s %v", name, args)
			}
			return []byte("some lms error"), errors.New("exit status 1")
		},
	}
	result, err := l.Load(context.Background(), ProviderSpec{Model: "gemma-4-e4b"})
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if result.Performed {
		t.Error("result.Performed = true, want false when unload fails")
	}
}

func TestLMStudioLoaderPerformsEvictAndLoad(t *testing.T) {
	var calls [][]string
	l := LMStudioLoader{
		LookPath: func(string) (string, error) { return "/usr/local/bin/lms", nil },
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return []byte("ok"), nil
		},
	}
	result, err := l.Load(context.Background(), ProviderSpec{Model: "google/gemma-4-e4b", ContextLength: 4096})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !result.Performed {
		t.Fatalf("result.Performed = false, want true; note=%q", result.Note)
	}
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2 (unload, load); calls=%v", len(calls), calls)
	}
	if calls[0][1] != "unload" {
		t.Errorf("calls[0] = %v, want an unload call first (the evict-others hook)", calls[0])
	}
	if calls[1][1] != "load" {
		t.Errorf("calls[1] = %v, want a load call second", calls[1])
	}
	foundContextFlag := false
	for i, a := range calls[1] {
		if a == "--context-length" && i+1 < len(calls[1]) && calls[1][i+1] == "4096" {
			foundContextFlag = true
		}
	}
	if !foundContextFlag {
		t.Errorf("load args = %v, want --context-length 4096 (the spec's right-sizing rule)", calls[1])
	}
}

func TestOllamaLoaderPreLoadsWithNumCtx(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeJSONBody(t, r, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := OllamaLoader{}
	result, err := l.Load(context.Background(), ProviderSpec{
		Model: "qwen3.6:latest", ContextLength: 4096, BaseURL: srv.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !result.Performed {
		t.Fatalf("result.Performed = false, want true; note=%q", result.Note)
	}
	if gotPath != "/api/generate" {
		t.Errorf("request path = %q, want /api/generate (Ollama's native API, not the OpenAI-compat /v1 path)", gotPath)
	}
	if gotBody["model"] != "qwen3.6:latest" {
		t.Errorf("body[model] = %v, want qwen3.6:latest", gotBody["model"])
	}
	options, ok := gotBody["options"].(map[string]any)
	if !ok {
		t.Fatalf("body[options] = %v, want a map carrying num_ctx", gotBody["options"])
	}
	if options["num_ctx"] != float64(4096) {
		t.Errorf("body[options][num_ctx] = %v, want 4096", options["num_ctx"])
	}
}

func TestOllamaLoaderDegradesWhenUnreachable(t *testing.T) {
	l := OllamaLoader{}
	result, err := l.Load(context.Background(), ProviderSpec{
		Model: "qwen3.6:latest", BaseURL: "http://127.0.0.1:1/v1", // nothing listens here
	})
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (unreachable degrades, never fails the run)", err)
	}
	if result.Performed {
		t.Error("result.Performed = true, want false when the server is unreachable")
	}
}

func TestOllamaLoaderDegradesOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	l := OllamaLoader{}
	result, err := l.Load(context.Background(), ProviderSpec{Model: "qwen3.6:latest", BaseURL: srv.URL + "/v1"})
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if result.Performed {
		t.Error("result.Performed = true, want false on a non-200 response")
	}
}

func TestOllamaNativeBaseURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434/v1":  "http://localhost:11434",
		"http://localhost:11434/v1/": "http://localhost:11434",
		"http://localhost:11434":     "http://localhost:11434",
	}
	for in, want := range cases {
		if got := ollamaNativeBaseURL(in); got != want {
			t.Errorf("ollamaNativeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoopLoaderNeverPerforms(t *testing.T) {
	result, err := NoopLoader{}.Load(context.Background(), ProviderSpec{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if result.Performed {
		t.Error("NoopLoader.Load().Performed = true, want false")
	}
}

func TestDefaultLoadersHasEntriesForOllamaAndLMStudio(t *testing.T) {
	loaders := DefaultLoaders()
	if _, ok := loaders[KindOllama]; !ok {
		t.Error("DefaultLoaders() has no entry for KindOllama")
	}
	if _, ok := loaders[KindLMStudio]; !ok {
		t.Error("DefaultLoaders() has no entry for KindLMStudio")
	}
	if _, ok := loaders[KindOpenAICompat]; ok {
		t.Error("DefaultLoaders() has an entry for KindOpenAICompat, want none (falls back to NoopLoader)")
	}
}
