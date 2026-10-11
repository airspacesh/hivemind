package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// fakeClock moves time only when the code under test sleeps.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

var (
	errNoConnection = &connectionError{server: "http://pond",
		err: errors.New("connection refused")}
	newMessages = api.ChatPage{Messages: []api.Message{{Number: 4}},
		Cursor: 4}
)

// polls answers each poll in turn with the next outcome, an error or a
// page, and then with empty pages.
func polls(outcomes ...any) func() (api.ChatPage, pollOutcome, error) {
	return func() (api.ChatPage, pollOutcome, error) {
		if len(outcomes) == 0 {
			return api.ChatPage{Cursor: 3}, nothingYet, nil
		}
		next := outcomes[0]
		outcomes = outcomes[1:]
		switch next := next.(type) {
		case error:
			return api.ChatPage{}, nothingYet, next
		case pollOutcome:
			return api.ChatPage{Cursor: 3}, next, nil
		}
		page := next.(api.ChatPage)
		if len(page.Messages) > 0 {
			return page, found, nil
		}
		return page, nothingYet, nil
	}
}

func seconds(values ...int) string {
	var durations []time.Duration
	for _, value := range values {
		durations = append(durations, time.Duration(value)*time.Second)
	}
	return fmt.Sprint(durations)
}

func TestWaitPollsEverySecondThenEveryFive(t *testing.T) {
	clk := &fakeClock{}
	page, err := waitFor(polls(), 22*time.Second, clk)
	if err != nil || len(page.Messages) != 0 || page.Cursor != 3 {
		t.Fatalf("got %+v, %v, want the empty page at the timeout", page, err)
	}
	want := seconds(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 5, 5, 2)
	if got := fmt.Sprint(clk.sleeps); got != want {
		t.Fatalf("slept %s, want %s", got, want)
	}
}

func TestWaitReturnsAsSoonAsMessagesArrive(t *testing.T) {
	clk := &fakeClock{}
	empty := api.ChatPage{Cursor: 3}
	page, err := waitFor(polls(empty, empty, newMessages),
		time.Minute, clk)
	if err != nil || page.Cursor != 4 || len(clk.sleeps) != 2 {
		t.Fatalf("got %+v, %v after %v", page, err, clk.sleeps)
	}
}

// A poll that left changes unchecked polls again at once, and a poll
// that found nothing still sleeps.
func TestWaitPollsAgainAtOnceWhileMoreIsToCheck(t *testing.T) {
	clk := &fakeClock{}
	page, err := waitFor(polls(moreToCheck, moreToCheck, nothingYet,
		moreToCheck, newMessages), time.Minute, clk)
	if err != nil || page.Cursor != 4 {
		t.Fatalf("got %+v, %v", page, err)
	}
	if got, want := fmt.Sprint(clk.sleeps), seconds(1); got != want {
		t.Fatalf("slept %s, want %s", got, want)
	}
}

func TestWaitBacksOffThenGivesUp(t *testing.T) {
	failures := make([]any, 7)
	for i := range failures {
		failures[i] = errNoConnection
	}
	clk := &fakeClock{}
	_, err := waitFor(polls(failures...), time.Hour, clk)
	if err == nil || !strings.Contains(err.Error(), "gave up waiting after "+
		"6 retries") || !errors.Is(err, errNoConnection) {
		t.Fatalf("got %v", err)
	}
	if got, want := fmt.Sprint(clk.sleeps), seconds(1, 2, 4, 8, 16, 30); got != want {
		t.Fatalf("slept %s, want %s", got, want)
	}
}

func TestWaitResetsBackoffAfterAnAnswer(t *testing.T) {
	empty := api.ChatPage{Cursor: 3}
	clk := &fakeClock{}
	page, err := waitFor(polls(errNoConnection, errNoConnection,
		empty, errNoConnection, newMessages), time.Hour, clk)
	if err != nil || page.Cursor != 4 {
		t.Fatalf("got %+v, %v", page, err)
	}
	if got, want := fmt.Sprint(clk.sleeps), seconds(1, 2, 1, 1); got != want {
		t.Fatalf("slept %s, want %s", got, want)
	}
}

func TestWaitStopsAtOnceOnARefusal(t *testing.T) {
	refused := &serverError{server: "http://pond", status: 404,
		code: api.CodePostNotFound, message: "entry not found: e3"}
	clk := &fakeClock{}
	_, err := waitFor(polls(refused), time.Hour, clk)
	if !errors.Is(err, refused) || len(clk.sleeps) != 0 {
		t.Fatalf("got %v after %v", err, clk.sleeps)
	}
}

func TestWaitReportsAnOutageAtItsTimeout(t *testing.T) {
	clk := &fakeClock{}
	_, err := waitFor(polls(errNoConnection, errNoConnection,
		errNoConnection), 2*time.Second, clk)
	if err == nil || !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("got %v after %v", err, clk.sleeps)
	}
}
