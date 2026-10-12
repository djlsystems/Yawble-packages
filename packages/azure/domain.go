package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// domain bind puts a custom domain on a container app with a free managed certificate. Azure gives
// the certificate only once the domain's records point at the app: a TXT record asuid.<host> holding
// the app's verification id, and a CNAME to the app's address (an A record to the environment's
// address for a domain's apex). In a DNS zone the team owns the plugin sets them; elsewhere it says
// what to set. Once they resolve, the next domain bind creates the certificate and binds it.

// How names are looked up. Only tests change them.
var (
	lookupTXT   = net.LookupTXT
	lookupCNAME = net.LookupCNAME
	lookupHost  = net.LookupHost
)

func init() {
	register(command{words: []string{"domain", "bind"}, usage: "<app> <hostname>", run: (*plugin).domainBind})
}

// record is one DNS record a domain needs: its full name, type and value.
type record struct{ name, typ, value string }

func (p *plugin) domainBind(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "domain bind", "<app> <hostname>"); err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSuffix(args[1], "."))
	if !zoneForm.MatchString(host) {
		return "", refusef("Refused: %q is not a host name such as www.example.com.", args[1])
	}
	r, app, err := p.containerApp(args[0])
	if err != nil {
		return "", err
	}
	if err := p.mustOwn(change{id: r.ID, kind: changeModify}); err != nil {
		return "", err
	}
	pr := props(app)
	cfg, _ := pr["configuration"].(map[string]any)
	ingress, _ := cfg["ingress"].(map[string]any)
	fqdn := str(ingress["fqdn"])
	if fqdn == "" || ingress["external"] != true {
		return "", refusef("Refused: container app %s has no public ingress, so a domain cannot reach it. Create it with port=<n>.", r.Name)
	}
	for _, d := range anyList(ingress["customDomains"]) {
		if strings.EqualFold(str(d["name"]), host) && str(d["bindingType"]) == "SniEnabled" {
			return fmt.Sprintf("%s is already bound to %s with a certificate. Nothing was changed.", host, r.Name), nil
		}
	}
	verification := str(pr["customDomainVerificationId"])
	envID := str(pr["managedEnvironmentId"])
	var env struct {
		Location   string `json:"location"`
		Properties struct {
			StaticIP string `json:"staticIp"`
		} `json:"properties"`
	}
	if _, err := p.az.get(envID+"?api-version="+versionOf("Microsoft.App/managedEnvironments"), &env); err != nil {
		return "", err
	}
	if verification == "" || env.Properties.StaticIP == "" {
		return "", fmt.Errorf("Azure did not give container app %s's verification id or address; try again in a minute", r.Name)
	}

	// THE RECORDS, and the zone they go in when the team owns one.
	zoneID, zone, err := p.zoneFor(host)
	if err != nil {
		return "", err
	}
	apex := host == zone || (zone == "" && strings.Count(host, ".") == 1)
	need := []record{{"asuid." + host, "TXT", verification}}
	if apex {
		need = append(need, record{host, "A", env.Properties.StaticIP})
	} else {
		need = append(need, record{host, "CNAME", fqdn})
	}
	ownedZone := false
	if zoneID != "" {
		why, err := p.guard(change{id: zoneID, kind: changeModify})
		if err != nil {
			return "", err
		}
		ownedZone = why == ""
	}
	var lines []string
	for _, rec := range need {
		lines = append(lines, fmt.Sprintf("- %s %s %s", rec.typ, rec.name, rec.value))
	}
	records := strings.Join(lines, "\n")

	if !resolves(need) {
		if ownedZone {
			what := fmt.Sprintf("set the records %s needs in DNS zone %s:\n%s", host, zone, records)
			if !p.cfg.apply() {
				p.planned(plan{command: "domain bind", target: r.ID, summary: what, counts: map[string]int{"modify": len(need)}})
				return "whatif: would " + what + "\nOnce they resolve, domain bind creates the managed certificate and binds it. Nothing was changed; a person sets mode to apply in this member's settings to make the change.", nil
			}
			var ids []string
			for _, rec := range need {
				id, err := p.setZoneRecord(zoneID, zone, rec)
				if err != nil {
					return "", err
				}
				ids = append(ids, id)
			}
			p.applied("domain bind", r.ID, "set the records for "+host+" in "+zone, ids)
			if !resolves(need) {
				return fmt.Sprintf("Set the records %s needs in DNS zone %s:\n%s\nThey resolve within minutes when the domain's registrar points at the zone's name servers. Run domain bind %s %s again then, and it creates the certificate and binds it.", host, zone, records, r.Name, host), nil
			}
		} else {
			where := "at the DNS host of " + host
			if zoneID != "" {
				where = "in DNS zone " + zone + ", which this team does not own,"
			}
			p.planned(plan{command: "domain bind", target: r.ID, summary: "records " + host + " needs before binding", counts: map[string]int{}})
			return fmt.Sprintf("%s does not point at %s yet. A person sets these records %s:\n%s\nOnce they resolve (minutes to a day), run domain bind %s %s again and it creates the certificate and binds it. Nothing was changed.", host, r.Name, where, records, r.Name, host), nil
		}
	}

	// THE CERTIFICATE AND THE BINDING.
	er, _ := parseID(envID)
	certName := "mc-" + strings.ReplaceAll(host, ".", "-")
	if len(certName) > 60 {
		certName = certName[:60]
	}
	certName = strings.TrimRight(certName, "-")
	certID := er.raw + "/managedCertificates/" + certName
	if err := p.mustOwn(change{id: certID, kind: changeCreate, tags: p.ownTags()}); err != nil {
		return "", err
	}
	validation := "CNAME"
	if apex {
		validation = "HTTP"
	}
	what := fmt.Sprintf("add %s to container app %s, create a managed certificate for it in environment %s, and bind it", host, r.Name, er.name())
	if !p.cfg.apply() {
		return p.whatif("domain bind", r.ID, what, map[string]int{"create": 1, "modify": 1}), nil
	}
	appVersion := versionOf("Microsoft.App/containerApps")
	patch := func(binding map[string]any) error {
		domains := []map[string]any{}
		for _, d := range anyList(ingress["customDomains"]) {
			if !strings.EqualFold(str(d["name"]), host) {
				domains = append(domains, d)
			}
		}
		ingress["customDomains"] = append(domains, binding)
		ans, err := p.az.arm(http.MethodPatch, r.ID+"?api-version="+appVersion, map[string]any{
			"properties": map[string]any{"configuration": map[string]any{"ingress": ingress}},
		}, nil)
		if err == nil {
			_, err = p.az.wait(ans, "the container app "+r.Name)
		}
		return err
	}
	p.progress("azure: adding " + host + " to " + r.Name)
	if err := patch(map[string]any{"name": host, "bindingType": "Disabled"}); err != nil {
		return "", err
	}
	p.progress("azure: creating the managed certificate for " + host + " (a few minutes)")
	ans, err := p.az.arm(http.MethodPut, certID+"?api-version="+versionOf("Microsoft.App/managedEnvironments/managedCertificates"), map[string]any{
		"location": env.Location, "tags": p.ownTags(),
		"properties": map[string]any{"subjectName": host, "domainControlValidation": validation},
	}, nil)
	if err == nil {
		_, err = p.az.wait(ans, "the certificate for "+host)
	}
	if err != nil {
		p.applied("domain bind", r.ID, "added "+host+" to "+r.Name+" without a certificate", []string{r.ID})
		return "", err
	}
	if err := p.waitCertificate(certID, host); err != nil {
		p.applied("domain bind", r.ID, "added "+host+" to "+r.Name+"; its certificate is not issued", []string{r.ID, certID})
		return "", err
	}
	p.progress("azure: binding the certificate to " + host)
	if err := patch(map[string]any{"name": host, "bindingType": "SniEnabled", "certificateId": certID}); err != nil {
		p.applied("domain bind", r.ID, "created the certificate for "+host+"; it is not bound", []string{r.ID, certID})
		return "", err
	}
	p.applied("domain bind", r.ID, what, []string{r.ID, certID})
	return fmt.Sprintf("Bound %s to container app %s with a managed certificate, which Azure renews by itself. It answers at https://%s.", host, r.Name, host), nil
}

// waitCertificate follows a managed certificate until Azure has issued it.
func (p *plugin) waitCertificate(id, host string) error {
	deadline := time.Now().Add(pollFor)
	for {
		var cert struct {
			Properties struct {
				State string `json:"provisioningState"`
				Error string `json:"error"`
			} `json:"properties"`
		}
		if _, err := p.az.get(id+"?api-version="+versionOf("Microsoft.App/managedEnvironments/managedCertificates"), &cert); err != nil {
			return err
		}
		switch strings.ToLower(cert.Properties.State) {
		case "succeeded":
			return nil
		case "failed", "canceled", "deletefailed":
			return fmt.Errorf("Azure could not issue the certificate for %s: %s. Check the records, then run domain bind again", host, cert.Properties.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the certificate for %s was not issued within %d minutes; run domain bind again later", host, int(pollFor.Minutes()))
		}
		p.progress("azure: waiting for the certificate for " + host)
		sleep(pollEvery)
	}
}

// zoneFor finds the Azure DNS zone in the subscription that holds a host name, the longest one
// when several do; "" when none does.
func (p *plugin) zoneFor(host string) (id, zone string, err error) {
	err = p.az.list("/subscriptions/"+p.cfg.Subscription+"/providers/Microsoft.Network/dnszones?api-version="+dnsAPIVersion, func(raw json.RawMessage) {
		var r resource
		if json.Unmarshal(raw, &r) != nil {
			return
		}
		name := strings.ToLower(r.Name)
		if (host == name || strings.HasSuffix(host, "."+name)) && len(name) > len(zone) {
			id, zone = r.ID, name
		}
	})
	return id, zone, err
}

// setZoneRecord sets one record in a zone the team owns, and answers its record set's id.
func (p *plugin) setZoneRecord(zoneID, zone string, rec record) (string, error) {
	name := strings.TrimSuffix(rec.name, "."+zone)
	if rec.name == zone {
		name = "@"
	}
	id := zoneID + "/" + rec.typ + "/" + name
	if err := p.mustOwn(change{id: id, kind: changeModify}); err != nil {
		return "", err
	}
	body, err := recordProps(rec.typ, []string{rec.value}, 3600)
	if err != nil {
		return "", err
	}
	if _, err := p.az.arm(http.MethodPut, id+"?api-version="+dnsAPIVersion, map[string]any{"properties": body}, nil); err != nil {
		return "", err
	}
	return id, nil
}

// resolves reports whether every record already answers as needed.
func resolves(need []record) bool {
	for _, rec := range need {
		switch rec.typ {
		case "TXT":
			values, err := lookupTXT(rec.name)
			if err != nil || !contains(values, rec.value) {
				return false
			}
		case "CNAME":
			target, err := lookupCNAME(rec.name)
			if err != nil || !strings.EqualFold(strings.TrimSuffix(target, "."), rec.value) {
				return false
			}
		case "A":
			addrs, err := lookupHost(rec.name)
			if err != nil || !contains(addrs, rec.value) {
				return false
			}
		}
	}
	return true
}

// anyList reads a JSON list of objects.
func anyList(v any) []map[string]any {
	var out []map[string]any
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
