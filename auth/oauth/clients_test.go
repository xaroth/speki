package oauth_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A metadata document that couldn't be fetched isn't fetched again for a while: a minute when it's
// missing or wrong, a few seconds when its host had a blip.
func TestMetadataFailuresAreRemembered(t *testing.T) {
	t.Parallel()
	const doc = "https://claude.ai/oauth/other.json"
	testCases := []struct {
		name    string
		serve   http.HandlerFunc
		wantFor time.Duration
	}{
		{name: "wrong document", serve: document(claudeClient, claudeCallback), wantFor: time.Minute},
		{name: "missing document", serve: status(http.StatusNotFound), wantFor: time.Minute},
		{name: "host failing", serve: status(http.StatusServiceUnavailable), wantFor: 10 * time.Second},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.docs.serve(doc, testCase.serve)
				authorize := authorizeURL(doc, claudeCallback, nil)
				steps := []struct {
					name        string
					sleep       time.Duration
					wantStatus  int
					wantFetches int
				}{
					{name: "first", wantStatus: http.StatusBadRequest, wantFetches: 1},
					{name: "just before the failure runs out", sleep: testCase.wantFor - time.Second, wantStatus: http.StatusBadRequest, wantFetches: 1},
					{name: "once it ran out", sleep: time.Second, wantStatus: http.StatusOK, wantFetches: 2},
				}
				for _, step := range steps {
					if step.name == "just before the failure runs out" {
						e.docs.serve(doc, document(doc, claudeCallback))
					}
					time.Sleep(step.sleep)
					if got := e.do(t, authorize, nil).Code; got != step.wantStatus {
						t.Errorf("%s: got status %d, want %d", step.name, got, step.wantStatus)
					}
					if got := e.docs.count(doc); got != step.wantFetches {
						t.Errorf("%s: got %d fetches, want %d", step.name, got, step.wantFetches)
					}
				}
			})
		})
	}
}

// A document fetched before is refetched in the background, on fetch slots of its own, and is still used
// for an hour past its time while its host can't be reached. A document that's gone isn't used any more.
func TestKnownMetadataDocument(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		good := e.docs.docs[claudeClient]
		hang := make(chan struct{})
		var wg sync.WaitGroup
		defer func() {
			close(hang)
			wg.Wait()
		}()
		takeSlots := func() {
			for i := range 4 {
				u := fmt.Sprintf("https://claude.ai/hang-%d.json", i)
				e.docs.serve(u, func(http.ResponseWriter, *http.Request) { <-hang })
				wg.Go(func() { e.do(t, authorizeURL(u, claudeCallback, nil), nil) })
			}
		}
		steps := []struct {
			name        string
			before      func()
			serve       http.HandlerFunc
			clientID    string
			sleep       time.Duration
			wantStatus  int
			wantWait    time.Duration
			wantFetches int // of the known document
		}{
			{name: "first sign-in", wantStatus: http.StatusOK, wantFetches: 1},
			{name: "unknown document while every slot is taken", before: takeSlots, clientID: "https://claude.ai/new.json",
				wantStatus: http.StatusBadRequest, wantWait: 2 * time.Second, wantFetches: 1},
			{name: "expired, refetched in the background on a slot of its own", sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 2},
			{name: "refetched", wantStatus: http.StatusOK, wantFetches: 2},
			{name: "host failing, old document holds", serve: status(http.StatusServiceUnavailable), sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 3},
			{name: "failure remembered, old document holds", wantStatus: http.StatusOK, wantFetches: 3},
			{name: "an hour past its time", sleep: time.Hour, wantStatus: http.StatusBadRequest, wantFetches: 4},
			{name: "host back", serve: good, sleep: 10 * time.Second, wantStatus: http.StatusOK, wantFetches: 5},
			{name: "gone, old document used while refetching", serve: status(http.StatusNotFound), sleep: 10 * time.Minute, wantStatus: http.StatusOK, wantFetches: 6},
			{name: "gone", wantStatus: http.StatusBadRequest, wantFetches: 6},
		}
		for _, step := range steps {
			if step.before != nil {
				step.before()
				synctest.Wait()
			}
			if step.serve != nil {
				e.docs.serve(claudeClient, step.serve)
			}
			time.Sleep(step.sleep)
			clientID := step.clientID
			if clientID == "" {
				clientID = claudeClient
			}
			start := time.Now()
			got := e.do(t, authorizeURL(clientID, claudeCallback, nil), nil).Code
			if waited := time.Since(start); waited != step.wantWait {
				t.Errorf("%s: got an answer after %v, want %v", step.name, waited, step.wantWait)
			}
			synctest.Wait() // for the background refetch
			if got != step.wantStatus {
				t.Errorf("%s: got status %d, want %d", step.name, got, step.wantStatus)
			}
			if n := e.docs.count(claudeClient); n != step.wantFetches {
				t.Errorf("%s: got %d fetches, want %d", step.name, n, step.wantFetches)
			}
		}
	})
}

// Requests for one document share a fetch. One that leaves stops waiting, without failing the fetch for
// the others.
func TestMetadataFetchesAreShared(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		const doc = "https://claude.ai/shared.json"
		release := make(chan struct{})
		serve := document(doc, claudeCallback)
		e.docs.serve(doc, func(w http.ResponseWriter, r *http.Request) {
			<-release
			serve(w, r)
		})
		authorize := authorizeURL(doc, claudeCallback, nil)
		var wg sync.WaitGroup
		statuses := make(chan int, 8)
		for range 8 {
			wg.Go(func() { statuses <- e.do(t, authorize, nil).Code })
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if got := e.doCtx(t, ctx, authorize, nil).Code; got != http.StatusBadRequest {
			t.Errorf("request that left: got %d, want 400", got)
		}
		close(release)
		wg.Wait()
		close(statuses)
		for got := range statuses {
			if got != http.StatusOK {
				t.Errorf("got status %d, want 200", got)
			}
		}
		if n := e.docs.count(doc); n != 1 {
			t.Errorf("got %d fetches, want 1", n)
		}
	})
}
