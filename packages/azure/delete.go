package main

import (
	"fmt"
	"net/http"
	"strings"
)

func init() {
	register(command{words: []string{"delete"}, usage: "<resource id or resource group>", run: (*plugin).deleteResource})
}

func (p *plugin) deleteResource(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "delete", "<resource id or resource group>"); err != nil {
		return "", err
	}
	// OFF MEANS OFF, in either mode, before anything is read.
	if !p.cfg.AllowDelete {
		return "", refusal("Refused: allowDelete is off, so this member deletes nothing, in whatif or apply. A person turns allowDelete on in this member's settings.")
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	id := strings.TrimSuffix(args[0], "/")
	if !strings.HasPrefix(id, "/") {
		id = rgPath(p.cfg.Subscription, id)
	}
	r, ok := parseID(id)
	if !ok {
		return "", refusef("Refused: %q is not a resource group name or an Azure resource id.", args[0])
	}
	tags, err := p.tagsOf(id)
	if err != nil {
		return "", err
	}
	if tags == nil {
		return "", refusef("Refused: %s does not exist, or this sign-in cannot see it. Nothing was deleted.", id)
	}
	if err := p.mustOwn(change{id: id, kind: changeDelete}); err != nil {
		return "", err
	}
	what := "delete " + id
	if r.ns == "" {
		what = "delete resource group " + r.rg + " and everything in it"
	}
	if !p.cfg.apply() {
		return p.whatif("delete", id, what, map[string]int{"delete": 1}), nil
	}
	version, err := p.apiVersionFor(r)
	if err != nil {
		return "", err
	}
	ans, err := p.az.arm(http.MethodDelete, id+"?api-version="+version, nil, nil)
	if isNotFound(err) {
		return id + " was already gone.", nil
	}
	if err != nil {
		return "", err
	}
	p.forget(id)
	p.applied("delete", id, what, []string{id})
	if ans.status == http.StatusAccepted {
		return fmt.Sprintf("Azure accepted the deletion of %s; it finishes in the next minutes.", id), nil
	}
	return "Deleted " + id + ".", nil
}
