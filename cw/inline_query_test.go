package cw_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"chatwright.dev/runtime/cw"
	"github.com/bots-go-framework/bots-api-telegram/tgbotapi"
)

func TestInlinePhotoInvitationScenario(t *testing.T) {
	world := cw.New(t)
	world.ServeWebhook(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode update: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if update.InlineQuery == nil {
			t.Errorf("update has no inline_query: %+v", update)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method":          "answerInlineQuery",
			"inline_query_id": update.InlineQuery.ID,
			"is_personal":     true,
			"results": []map[string]any{{
				"type":      "photo",
				"id":        "invite-42",
				"photo_url": "https://sneat.games/preferans-invite.jpg",
				"caption":   "Alice invited you · 🪙 50 · ⏱ 60 seconds",
				"reply_markup": map[string]any{"inline_keyboard": [][]map[string]any{{
					{"text": "🃏 Join table", "url": "https://t.me/SneatBot?start=pref_42"},
				}}},
			}},
		})
	}))

	answer := world.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"}).
		SendInlineQuery("preferans:invite:game-42").
		ResultCount(1).
		Snapshot()
	result := answer.Results[0]
	if result.Type != "photo" ||
		result.PhotoURL != "https://sneat.games/preferans-invite.jpg" ||
		result.Actions[0][0].Label != "🃏 Join table" {
		t.Fatalf("inline invitation = %+v", result)
	}
}

func TestUserLanguageCodeReachesTelegramUpdate(t *testing.T) {
	world := cw.New(t)
	var received tgbotapi.Update
	world.ServeWebhook(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode update: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	world.PrivateChat(cw.User{
		ID:           "alisa",
		FirstName:    "Алиса",
		LanguageCode: "ru",
	}).SendText("/pref")

	if received.Message == nil {
		t.Fatal("update has no message")
	}
	if got := received.Message.From.LanguageCode; got != "ru" {
		t.Fatalf("message.from.language_code = %q, want ru", got)
	}
}

func TestSelectedInlineInvitationCanBeEditedWithoutChatID(t *testing.T) {
	world := cw.New(t)
	world.ServeWebhook(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode update: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case update.InlineQuery != nil:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"method":          "answerInlineQuery",
				"inline_query_id": update.InlineQuery.ID,
				"results": []map[string]any{{
					"type":      "photo",
					"id":        "pref-invite-invite1",
					"photo_url": "https://sneat.games/preferans-invite.jpg",
					"caption":   "Alice invited you",
					"reply_markup": map[string]any{"inline_keyboard": [][]map[string]any{{
						{"text": "🃏 Join table", "url": "https://t.me/SneatBot?start=pref_invite1_token"},
					}}},
				}},
			})
		case update.ChosenInlineResult != nil:
			if update.ChosenInlineResult.Query != "pref?i=invite1" {
				t.Errorf("chosen query = %q", update.ChosenInlineResult.Query)
			}
			if update.ChosenInlineResult.InlineMessageID == "" {
				t.Error("chosen result has no inline_message_id")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"method":            "editMessageCaption",
				"inline_message_id": update.ChosenInlineResult.InlineMessageID,
				"caption":           "✅ Table full · joining is closed",
				"reply_markup":      map[string]any{"inline_keyboard": [][]map[string]any{}},
			})
		default:
			t.Errorf("unexpected update: %+v", update)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))

	query := world.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"}).
		SendInlineQuery("pref?i=invite1").
		ResultCount(1)
	selected := query.Select(0)
	if selected.InlineMessageID() == "" {
		t.Fatal("selected result has no opaque inline ID")
	}
	edited := selected.WaitForEdit()
	if edited.Caption != "✅ Table full · joining is closed" {
		t.Fatalf("edited caption = %q", edited.Caption)
	}
	if len(edited.Actions) != 0 {
		t.Fatalf("terminal invitation still has actions: %#v", edited.Actions)
	}
}
