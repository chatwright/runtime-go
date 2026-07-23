package cw_test

import (
	"fmt"
	"runtime"
	"testing"
)

// fakeTB is a minimal testing.TB double used to test Chatwright's own failure
// paths (Fatalf/Errorf/Cleanup) without the surrounding real *testing.T also
// being marked failed — a failing subtest run via t.Run unconditionally fails
// every enclosing test, which makes that approach unusable for "assert this
// scenario correctly fails" tests. Embedding a nil testing.TB satisfies the
// interface (which has an unexported method); only the methods Chatwright
// actually calls (Helper, Cleanup, Errorf, Fatalf) are overridden below —
// anything else would nil-panic, which run() also catches and reports.
type fakeTB struct {
	testing.TB

	mu       chan struct{} // binary mutex (buffered chan of size 1)
	failed   bool
	logs     []string
	cleanups []func()
}

func newFakeTB() *fakeTB {
	f := &fakeTB{mu: make(chan struct{}, 1)}
	f.mu <- struct{}{}
	return f
}

func (f *fakeTB) lock()   { <-f.mu }
func (f *fakeTB) unlock() { f.mu <- struct{}{} }

func (f *fakeTB) Helper() {}

func (f *fakeTB) Cleanup(fn func()) {
	f.lock()
	f.cleanups = append(f.cleanups, fn)
	f.unlock()
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.lock()
	f.failed = true
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
	f.unlock()
}

// Fatalf mirrors real testing.T: it records the failure and stops the calling
// goroutine, so callers relying on Fatalf never returning (as chatwright's own
// code does) behave the same way here as under a real test.
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

// run executes fn as a fake test: in its own goroutine (so a Fatalf inside it
// only unwinds that goroutine, matching how the testing package runs each
// test/subtest), then runs registered cleanups in LIFO order, matching
// testing.T.Cleanup semantics. It reports whether the fake test failed and its
// captured Errorf/Fatalf messages.
func (f *fakeTB) run(fn func(tb testing.TB)) (failed bool, logs []string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				f.lock()
				f.failed = true
				f.logs = append(f.logs, fmt.Sprintf("panic: %v", r))
				f.unlock()
			}
		}()
		fn(f)
	}()
	<-done

	f.lock()
	cleanups := f.cleanups
	f.cleanups = nil
	f.unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanup := cleanups[i]
		cdone := make(chan struct{})
		go func() {
			defer close(cdone)
			cleanup()
		}()
		<-cdone
	}

	f.lock()
	defer f.unlock()
	return f.failed, append([]string(nil), f.logs...)
}
