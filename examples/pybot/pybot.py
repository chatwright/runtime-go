#!/usr/bin/env python3
"""pybot is a minimal Telegram bot proving Chatwright's "any language" claim.

It is written with nothing but the Python standard library — http.server for
its webhook, urllib for calling back into the Bot API — so it runs anywhere
Python 3 is installed, with no `pip install` step. Chatwright never cares
what a bot-under-test is written in: it only ever speaks HTTP, both to
deliver updates to the bot's webhook and to receive the bot's outbound Bot
API calls.

Configuration is two environment variables, read once at start-up:

  TELEGRAM_API_ROOT  Base URL of the Telegram Bot API to call back into
                      (Chatwright's emulator in tests; https://api.telegram.org
                      in production). Required.
  PORT                Local TCP port this bot's webhook listens on. Required.

Behaviour mirrors examples/greetbot (the Go example), kept intentionally
smaller:

  - any text other than "/start" -> replies "Howdy stranger"
  - "/start"                     -> sends a message with one inline button
  - clicking that button         -> edits the message in place

See examples/pybot/pybot_e2e_test.go for the Chatwright scenario that drives
this bot as a real subprocess, and the repository README for the env-var
contract this relies on.
"""

import json
import os
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

# The emulator (like the real Bot API) doesn't care what the token is; any
# non-empty value that round-trips through the URL works.
TOKEN = "TEST:TOKEN"

BUTTON_TEXT = "Click me"
BUTTON_DATA = "clicked"


def api_root() -> str:
    root = os.environ.get("TELEGRAM_API_ROOT")
    if not root:
        sys.exit("pybot: TELEGRAM_API_ROOT is not set")
    return root.rstrip("/")


def call(method: str, payload: dict) -> None:
    """POSTs payload as JSON to the Bot API method, Telegram-style: the path
    carries the bot token, exactly as a real Telegram bot's HTTP client
    would build it."""
    url = f"{api_root()}/bot{TOKEN}/{method}"
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as resp:
            resp.read()
    except urllib.error.HTTPError as e:
        # Surface the emulator's error body (e.g. a validation failure) on
        # stderr instead of failing silently — makes a broken scenario
        # diagnosable from the bot's own log output.
        sys.stderr.write(f"pybot: {method} failed: {e.code} {e.read().decode('utf-8', 'replace')}\n")


def send_message(chat_id, text: str, reply_markup: dict | None = None) -> None:
    payload = {"chat_id": chat_id, "text": text}
    if reply_markup is not None:
        payload["reply_markup"] = reply_markup
    call("sendMessage", payload)


def edit_message(chat_id, message_id, text: str) -> None:
    call("editMessageText", {"chat_id": chat_id, "message_id": message_id, "text": text})


class Handler(BaseHTTPRequestHandler):
    # Quiet by default: BaseHTTPRequestHandler logs every request to stderr,
    # which just adds noise to a passing test run. A failing scenario shows
    # the failure through Chatwright's own assertions/transcript, not this
    # log.
    def log_message(self, fmt, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(length) if length else b"{}"
        try:
            update = json.loads(body or b"{}")
        except json.JSONDecodeError as e:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(str(e).encode("utf-8"))
            return

        callback = update.get("callback_query")
        message = update.get("message")

        if callback is not None:
            self._handle_callback(callback)
        elif message is not None:
            self._handle_message(message)

        self.send_response(200)
        self.end_headers()

    def _handle_message(self, message: dict) -> None:
        chat = message.get("chat") or {}
        chat_id = chat.get("id")
        if chat_id is None:
            return
        text = message.get("text") or ""
        if text == "/start":
            markup = {"inline_keyboard": [[{"text": BUTTON_TEXT, "callback_data": BUTTON_DATA}]]}
            send_message(chat_id, "Choose an action", markup)
        else:
            send_message(chat_id, "Howdy stranger")

    def _handle_callback(self, callback: dict) -> None:
        if callback.get("data") != BUTTON_DATA:
            return
        cb_message = callback.get("message") or {}
        chat = cb_message.get("chat") or {}
        chat_id = chat.get("id")
        message_id = cb_message.get("message_id")
        if chat_id is None or message_id is None:
            return
        edit_message(chat_id, message_id, "You clicked it!")


def main() -> None:
    api_root()  # fail fast if misconfigured, before binding a port
    port = os.environ.get("PORT")
    if not port:
        sys.exit("pybot: PORT is not set")
    server = HTTPServer(("127.0.0.1", int(port)), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
