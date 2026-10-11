package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A server that answers with more than the watch asked for must not make one check list more
// than maxResults; what is left over is listed by the next checks, and nothing is lost.
func TestWatchListsNoMoreThanMaxResultsWhenTheServerSendsMore(t *testing.T) {
	cases := map[string]func(t *testing.T) watchBox{
		"graph ignoring maxpagesize": func(t *testing.T) watchBox {
			f := newFakeGraph(t)
			f.ignoreLimits = true
			return graphWatchBox{f}
		},
		"gmail history in one record": func(t *testing.T) watchBox {
			g := newGmailBox(t)
			g.oneRecord = true
			return gmailWatchBox{g}
		},
	}
	for name, newBox := range cases {
		t.Run(name, func(t *testing.T) {
			box := newBox(t)
			st := newSiteStore()
			o := watchOpts(box, nil)
			o.maxResults = 2
			mustOK(t, st.run(t, "watch", o))
			var ids []string
			for i := range 5 {
				ids = append(ids, box.arrive(t, "a@example.org", fmt.Sprintf("Burst %d", i), time.Now().Add(time.Duration(i)*time.Second), false))
			}
			got := eventIDs(t, event(t, st.run(t, "watch", o)))
			sameIDs(t, got, ids[0], ids[1])
			mustOK(t, st.run(t, "ack "+strings.Join(got, ","), o))
			got = eventIDs(t, event(t, st.run(t, "watch", o)))
			sameIDs(t, got, ids[2], ids[3])
			mustOK(t, st.run(t, "ack "+strings.Join(got, ","), o))
			sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", o))), ids[4])
		})
	}
}

// A server that answers a list with more ids than were asked for must not make list or search
// fetch more than maxResults messages.
func TestGmailListFetchesNoMoreThanMaxResultsWhenTheServerSendsMore(t *testing.T) {
	g := newGmailBox(t)
	g.ignoreMax = true
	for i := range 10 {
		g.add("a@example.org", fmt.Sprintf("Message %d", i), "Body", time.Now().Add(time.Duration(i)*time.Second), false)
	}
	for _, instr := range []string{"list", "search subject:Message"} {
		before := len(g.called("GET", "/messages/"))
		mustOK(t, runIt(t, instr, opts{conn: g.conn(), maxResults: 3}))
		if n := len(g.called("GET", "/messages/")) - before; n != 3 {
			t.Errorf("%s with maxResults 3 fetched %d messages", instr, n)
		}
	}
}
