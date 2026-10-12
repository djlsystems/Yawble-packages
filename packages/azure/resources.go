package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// findByName finds a resource of one type, such as Microsoft.Storage/storageAccounts, by its name
// anywhere in the subscription. what names the type in a refusal.
func (p *plugin) findByName(typ, name, what string) (resource, error) {
	var found resource
	err := p.az.list("/subscriptions/"+p.cfg.Subscription+"/providers/"+typ+"?api-version="+versionOf(typ), func(raw json.RawMessage) {
		var r resource
		if json.Unmarshal(raw, &r) == nil && strings.EqualFold(r.Name, name) {
			found = r
		}
	})
	if err != nil {
		return resource{}, err
	}
	if found.ID == "" {
		return resource{}, refusef("Refused: there is no %s %s in the subscription, or this sign-in cannot see it.", what, name)
	}
	return found, nil
}

// versionOf answers the api-version of a type the plugin creates.
func versionOf(typ string) string {
	v, _ := knownVersion(typ)
	return v
}

// rgLocation answers the region of a resource group, which the resources the plugin creates in it
// share.
func (p *plugin) rgLocation(rg string) (string, error) {
	var g struct {
		Location string `json:"location"`
	}
	found, err := p.az.get(rgPath(p.cfg.Subscription, rg)+"?api-version="+rgAPIVersion, &g)
	if err != nil {
		return "", err
	}
	if !found {
		return "", refusef("Refused: resource group %s does not exist in the subscription; create it with rg create first.", rg)
	}
	return g.Location, nil
}

// put creates or changes one Azure Resource Manager resource, follows it to the end, and answers the
// resource as Azure then reads it.
func (p *plugin) put(id string, body any, what string) (map[string]any, error) {
	r, _ := parseID(id)
	path := id + "?api-version=" + versionOf(r.typ())
	ans, err := p.az.arm(http.MethodPut, path, body, nil)
	if err != nil {
		return nil, err
	}
	if _, err := p.az.wait(ans, what); err != nil {
		return nil, err
	}
	p.forget(id)
	var out map[string]any
	if _, err := p.az.get(path, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// props reads a resource's properties object.
func props(r map[string]any) map[string]any {
	m, _ := r["properties"].(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// signedIn reads who signed in from the Azure management token's claims: the object id, the
// tenant and the sign-in name. Only these three are read; the token itself is never written out.
func (p *plugin) signedIn() (oid, tenant, name string, err error) {
	parts := strings.Split(p.az.tokens[apiARM], ".")
	if len(parts) == 3 {
		if raw, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); e == nil {
			var c struct {
				OID  string `json:"oid"`
				TID  string `json:"tid"`
				UPN  string `json:"upn"`
				Name string `json:"unique_name"`
				Pref string `json:"preferred_username"`
			}
			if json.Unmarshal(raw, &c) == nil && c.OID != "" && c.TID != "" {
				name = c.UPN
				if name == "" {
					name = c.Pref
				}
				if name == "" {
					name = c.Name
				}
				if name == "" {
					name = p.account
				}
				return c.OID, c.TID, name, nil
			}
		}
	}
	return "", "", "", refusal("Refused: the Microsoft sign-in does not say which account and directory it is. A person reconnects this member's microsoft connection slot, then tries again.")
}
