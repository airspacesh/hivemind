package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// changePolls answers each poll in turn with the next outcome, an
// error or a number of changes, and then with no changes.
func changePolls(outcomes ...any) func() (api.ChangesPage, bool, error) {
	return func() (api.ChangesPage, bool, error) {
		if len(outcomes) == 0 {
			return api.ChangesPage{}, false, nil
		}
		next := outcomes[0]
		outcomes = outcomes[1:]
		if err, ok := next.(error); ok {
			return api.ChangesPage{}, false, err
		}
		page := api.ChangesPage{Changes: make([]api.Change, next.(int))}
		return page, len(page.Changes) > 0, nil
	}
}

// followed runs follow on a fake clock, and returns the sizes of the
// pages it showed and the clock's sleeps.
func followed(
	t *testing.T, timeout time.Duration, outcomes ...any,
) ([]int, string) {
	t.Helper()
	clk := &fakeClock{}
	var shown []int
	show := func(page api.ChangesPage) {
		shown = append(shown, len(page.Changes))
	}
	if err := follow(changePolls(outcomes...), show, timeout,
		clk); err != nil {
		t.Fatal(err)
	}
	return shown, fmt.Sprint(clk.sleeps)
}

func TestFollowPollsEverySecondAgainAfterChanges(t *testing.T) {
	quiet := make([]any, 12)
	for i := range quiet {
		quiet[i] = 0
	}
	shown, sleeps := followed(t, 22*time.Second,
		append(quiet, 2, 0, 1)...)
	if fmt.Sprint(shown) != "[2 1]" {
		t.Fatalf("showed pages of %v, want [2 1]", shown)
	}
	want := seconds(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 5, 5, 1, 1)
	if sleeps != want {
		t.Fatalf("slept %s, want %s", sleeps, want)
	}
}

func TestFollowStopsAtItsTimeoutAcrossPages(t *testing.T) {
	shown, sleeps := followed(t, 3*time.Second, 0, 1, 0, 1, 0, 0, 0)
	if fmt.Sprint(shown) != "[1 1]" {
		t.Fatalf("showed pages of %v, want [1 1]", shown)
	}
	if want := seconds(1, 1, 1); sleeps != want {
		t.Fatalf("slept %s, want %s", sleeps, want)
	}
}
