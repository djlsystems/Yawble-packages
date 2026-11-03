package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// deploy param=<NAME>=<connection>.<field>: a template's secure parameter filled from a connection
// bound to appSecrets, so a redeploy of an app reading its secrets from such parameters keeps them.

const secureTemplate = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": { "name": { "type": "string" }, "smtpPassword": { "type": "securestring" }, "plainSetting": { "type": "string" } },
  "resources": [
    { "type": "Microsoft.ManagedIdentity/userAssignedIdentities", "apiVersion": "2023-01-31", "name": "[parameters('name')]", "location": "westeurope" }
  ]
}`

func TestDeployFillsASecureParameterFromAConnectionField(t *testing.T) {
	f := newFake(t)
	docFile(t, "app.json", secureTemplate)
	params := docFile(t, "app.parameters.json", `{"parameters": {"name": {"value": "app-id"}}}`)
	id := inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id")
	f.whatIf = []map[string]any{wiChange("Modify", id, nil)}
	line := "deploy " + ownedRG + " app.json " + params + " param=SMTPPASSWORD=mail.password"
	o := runOpts{bound: setBound()}

	r := runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoSetValue(t, r.raw)
	if len(f.writes()) != 0 {
		t.Fatal("whatif deployed")
	}
	whatifName := f.callsTo(http.MethodPost, "/whatIf")[0].Path

	o.config = apply(nil)
	r = runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoSetValue(t, r.raw)
	puts := f.callsTo(http.MethodPut, "")
	if len(puts) != 1 {
		t.Fatalf("%d deployments", len(puts))
	}
	var deployed struct {
		Properties struct {
			Parameters map[string]struct {
				Value string `json:"value"`
			} `json:"parameters"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(puts[0].Body, &deployed); err != nil {
		t.Fatal(err)
	}
	if deployed.Properties.Parameters["smtpPassword"].Value != secretValue || deployed.Properties.Parameters["name"].Value != "app-id" {
		t.Fatal("the deployment's parameters are not the file's and the connection's")
	}
	// The deployment's name comes from the reference, not the value: a changed password deploys
	// under the same name.
	if wi := f.callsTo(http.MethodPost, "/whatIf"); len(wi) != 2 || wi[1].Path != whatifName {
		t.Fatal("the deployment's name depends on the connection's value")
	}
	for _, c := range f.calls {
		if strings.Contains(string(c.Body), secretValue) && !strings.Contains(c.Path, "/providers/Microsoft.Resources/deployments/") {
			t.Fatalf("the value went to %s %s", c.Method, c.Path)
		}
	}
}

func TestDeployParamRefusals(t *testing.T) {
	for line, want := range map[string]string{
		"param=plainSetting=mail.password":    "the parameter plainSetting of app.json is not secure",
		"param=nothere=mail.password":         "app.json declares no parameter nothere",
		"param=smtpPassword=mailbox.password": "the field password of the connection mailbox, which is not bound to this member's appSecrets slot (the ones bound are mail, stripe live)",
		"param=smtpPassword=mail.apiKey":      "the field apiKey of the connection mail, which that connection does not have",
		"param=smtpPassword=" + secretValue:   "param=smtpPassword takes <connection>.<field>",
	} {
		f := newFake(t)
		docFile(t, "app.json", secureTemplate)
		r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "deploy "+ownedRG+" app.json "+line)
		if r.ok() {
			t.Fatalf("%s was accepted", line)
		}
		mustContain(t, r.text(), want)
		assertNoSetValue(t, r.raw)
		if len(f.calls) != 0 {
			t.Fatalf("%s: Azure was asked before the refusal", line)
		}
	}
	f := newFake(t)
	docFile(t, "app.json", secureTemplate)
	params := docFile(t, "app.parameters.json", `{"parameters": {"name": {"value": "app-id"}, "smtpPassword": {"reference": {"keyVault": {"id": "x"}, "secretName": "y"}}}}`)
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "deploy "+ownedRG+" app.json "+params+" param=smtpPassword=mail.password")
	if r.ok() || !strings.Contains(r.text(), "the parameters file already gives smtpPassword") || len(f.calls) != 0 {
		t.Fatalf("a parameter given twice was not refused: %s", r.text())
	}
}
