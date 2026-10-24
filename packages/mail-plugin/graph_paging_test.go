package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// folderListCalls are the requests for one folder list: its first page and each nextLink.
func folderListCalls(f *fakeGraph, path string) []call {
	var out []call
	for _, c := range f.called("GET", path) {
		if c.Path == "/v1.0"+path {
			out = append(out, c)
		}
	}
	return out
}

func TestGraphFindsAFolderOnTheThirdPageOfFolders(t *testing.T) {
	f := newFakeGraph(t)
	f.folders = map[string]string{"inbox": "Inbox"}
	for i := 1; i < 250; i++ {
		f.folders[fmt.Sprintf("AQMk-p%04d", i)] = fmt.Sprintf("Project %03d", i)
	}
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	// In id order Project 230 is the 230th of the 250 folders, so on the third page of 100.
	mustOK(t, runIt(t, "move \"Project 230\" "+a, graphOpts(f, map[string]any{"moveTo": []string{"Project 230"}})))
	if got := f.msgs[0].Folder; got != "AQMk-p0230" {
		t.Fatalf("want it moved to Project 230, got %q", got)
	}
	if n := len(folderListCalls(f, "/me/mailFolders")); n != 3 {
		t.Fatalf("want the three pages of folders read, got %d requests", n)
	}
	if len(graphCreates(f)) != 0 {
		t.Fatal("the folder exists, so nothing is created")
	}
}

func TestGraphResolvesAPathThroughTheSecondPageOfChildren(t *testing.T) {
	f := newFakeGraph(t)
	f.folders["AQMk-clients"] = "Clients"
	f.parent = map[string]string{"AQMk-clients": "inbox"}
	for i := 1; i <= 150; i++ {
		id := fmt.Sprintf("AQMk-c%04d", i)
		f.folders[id], f.parent[id] = fmt.Sprintf("Client %03d", i), "AQMk-clients"
	}
	f.folders["AQMk-c0140-acme"], f.parent["AQMk-c0140-acme"] = "Acme", "AQMk-c0140"
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	b := f.add("b@example.org", "B", "b", time.Now(), false)
	cfg := map[string]any{"moveTo": []string{"Inbox/Clients/Client 140", "Inbox/Clients/Client 140/Acme"}}
	mustOK(t, runIt(t, "move \"Inbox/Clients/Client 140\" "+a, graphOpts(f, cfg)))
	mustOK(t, runIt(t, "move \"Inbox/Clients/Client 140/Acme\" "+b, graphOpts(f, cfg)))
	if f.msgs[0].Folder != "AQMk-c0140" || f.msgs[1].Folder != "AQMk-c0140-acme" {
		t.Fatalf("want them in Client 140 and its Acme, got %q and %q", f.msgs[0].Folder, f.msgs[1].Folder)
	}
	second := false
	for _, c := range folderListCalls(f, "/me/mailFolders/AQMk-clients/childFolders") {
		second = second || strings.Contains(c.Query, "%24skip=100")
	}
	if !second {
		t.Fatal("want the second page of Clients' 150 children read")
	}
}

func TestGraphStopsAtTheBoundAndSaysHowFarItRead(t *testing.T) {
	f := newFakeGraph(t)
	f.endlessFolders = true
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	mustFail(t, runIt(t, "move Somewhere "+a, graphOpts(f, map[string]any{"moveTo": []string{"Somewhere"}})),
		"Microsoft Graph still had more folders after 50 pages and 5000 entries, more than Mailer reads in one go (50 pages or 5000 entries), so it stopped there; nothing was changed.")
	if n := len(folderListCalls(f, "/me/mailFolders")); n != graphMaxPages {
		t.Fatalf("want it to stop after %d pages, got %d requests", graphMaxPages, n)
	}
	if len(graphCreates(f)) != 0 || f.msgs[0].Folder != "inbox" {
		t.Fatal("the list was not read to its end, so nothing is created or moved")
	}
	o := runIt(t, "folders", graphOpts(f, nil))
	mustFail(t, o, "after 50 pages and 5000 entries")
	if strings.Contains(o.output, "Endless 0") {
		t.Fatalf("a list cut short is never shown:\n%s", o.output)
	}
}
