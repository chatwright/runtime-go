package cw_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"chatwright.dev/runtime/cw"
)

func TestChatSubmitCallbackDoesNotRequireAdvertisedAction(t *testing.T) {
	world := cw.New(t)
	world.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"}).
		SubmitCallback(99, "pref?g=old&a=x&x=forged")

	response, err := http.Get(world.BotAPIURL() + "/botTEST/getUpdates")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	var envelope struct {
		OK     bool `json:"ok"`
		Result []struct {
			CallbackQuery *struct {
				Data    string `json:"data"`
				Message struct {
					MessageID int `json:"message_id"`
				} `json:"message"`
			} `json:"callback_query"`
		} `json:"result"`
	}
	if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || len(envelope.Result) != 1 || envelope.Result[0].CallbackQuery == nil {
		t.Fatalf("getUpdates = %+v", envelope)
	}
	callback := envelope.Result[0].CallbackQuery
	if callback.Data != "pref?g=old&a=x&x=forged" || callback.Message.MessageID != 99 {
		t.Fatalf("callback = %+v", callback)
	}
}
