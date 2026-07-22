package chatwright_test

import (
	"net"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/whatsapp"
)

// freeAddr reserves a free local TCP address by binding to port 0, reading
// back what the OS assigned, then releasing it — the same "know the address
// before anything is listening on it" trick WithListenAddr exists to support
// for an externally-started bot process (see examples/pybot).
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return addr
}

// TestWithListenAddr_BindsRequestedAddress proves the emulator ends up
// listening exactly where the caller asked, and behaves like any other
// Chatwright harness once it does.
func TestWithListenAddr_BindsRequestedAddress(t *testing.T) {
	addr := freeAddr(t)

	cw := chatwright.New(t, chatwright.WithListenAddr(addr))
	if got, want := cw.BotAPIURL(), "http://"+addr; got != want {
		t.Fatalf("BotAPIURL() = %q, want %q", got, want)
	}

	cw.ServeWebhook(tgGreeter(cw.BotAPIURL(), "TEST:TOKEN"))
	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	chat.ExpectBotMessage().Within(time.Second).Text("Howdy stranger")
}

// TestWithListenAddr_UnsupportedPlatform_FailsClearly proves New fails the
// test, rather than silently ignoring the request or panicking, when a fixed
// listen address is requested for a platform that doesn't support one.
func TestWithListenAddr_UnsupportedPlatform_FailsClearly(t *testing.T) {
	addr := freeAddr(t)

	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		chatwright.New(tb, chatwright.OnPlatform(whatsapp.Platform()), chatwright.WithListenAddr(addr))
	})
	if !failed {
		t.Fatalf("expected New to fail the test for a platform without AddrPlatform support")
	}
	if !anyContains(logs, "does not support a fixed listen address") {
		t.Fatalf("expected a clear unsupported-platform message, got: %v", logs)
	}
}

// TestWithListenAddr_AddressInUse_FailsClearly proves a bind failure (the
// address already occupied) surfaces as a clear test failure instead of a
// panic.
func TestWithListenAddr_AddressInUse_FailsClearly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		chatwright.New(tb, chatwright.WithListenAddr(addr))
	})
	if !failed {
		t.Fatalf("expected New to fail the test when the requested address is already in use")
	}
	if !anyContains(logs, "listen on") {
		t.Fatalf("expected a clear listen-failure message, got: %v", logs)
	}
}
