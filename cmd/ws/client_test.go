package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

var (
	errUnsent = &connectionError{server: "http://pond",
		err: errors.New("connection refused"), unsent: true}
	errBusy = &serverError{server: "http://pond", status: 503,
		code: api.CodeUnavailable, message: "server is busy"}
)

// sends answers each send in turn with the next error, and then with
// the last one.
func sends(errs ...error) (func() error, *int) {
	count := 0
	return func() error {
		count++
		return errs[min(count, len(errs))-1]
	}, &count
}

func TestWriteBacksOffThenGivesUpAtItsBudget(t *testing.T) {
	clk := &fakeClock{}
	send, count := sends(errNoConnection)
	err := sendRetrying(send, clk)
	want := seconds(1, 2, 4, 8, 8, 8, 8, 8, 8)
	if got := fmt.Sprint(clk.sleeps); got != want {
		t.Fatalf("slept %s, want %s", got, want)
	}
	if *count != 10 || !mayHaveApplied(err) || !strings.Contains(
		err.Error(), "gave up after 10 tries in 55s") {
		t.Fatalf("after %d sends got %v", *count, err)
	}
}

func TestWriteSendsAgainUntilItLands(t *testing.T) {
	clk := &fakeClock{}
	send, count := sends(errNoConnection, errBusy, nil)
	if err := sendRetrying(send, clk); err != nil || *count != 3 {
		t.Fatalf("after %d sends got %v", *count, err)
	}
}

// A first send that never left the client is certain, so it is not
// repeated, but once a send may have reached the server, the write
// stays uncertain and is repeated whatever the later sends meet.
func TestWriteRepeatsOnlyOnceASendMayHaveApplied(t *testing.T) {
	send, count := sends(errUnsent)
	err := sendRetrying(send, &fakeClock{})
	if err != errUnsent || *count != 1 {
		t.Fatalf("after %d sends got %v", *count, err)
	}
	send, count = sends(errNoConnection, errUnsent)
	err = sendRetrying(send, &fakeClock{})
	if *count != 10 || !mayHaveApplied(err) || !errors.Is(err, errUnsent) {
		t.Fatalf("after %d sends got %v", *count, err)
	}
}

func TestWriteStopsAtOnceOnARefusal(t *testing.T) {
	refused := &serverError{server: "http://pond", status: 404,
		code: api.CodePostNotFound, message: "post not found: e3"}
	clk := &fakeClock{}
	send, _ := sends(errNoConnection, refused)
	err := sendRetrying(send, clk)
	if err != refused || mayHaveApplied(err) ||
		fmt.Sprint(clk.sleeps) != seconds(1) {
		t.Fatalf("got %v after %v", err, clk.sleeps)
	}
}
