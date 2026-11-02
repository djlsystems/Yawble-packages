package main

import (
	"fmt"
	"net/http"
	"strings"
)

// containerapp secret set puts secrets from the team's connections into an app that already exists -
// one Bicep deployed, say - and points environment variables at them, changing nothing else.
//
// THE WRITE IS A PATCH OF configuration.secrets AND template.containers ONLY. Azure merges a PATCH
// into the app, so scale (min and max replicas, rules), ingress, registries, identity, Dapr and the
// rest of the configuration are not in the body and stay as they are. The two lists it does send
// replace the app's whole lists, so both go back complete:
//   - every container as Azure answered it - image, probes, resources, volume mounts - with only the
//     env entries this command names added or changed;
//   - every secret: the ones named here with their new values, a Key Vault one kept as its address
//     and identity, and a value one kept with its value. Azure never answers a value on a GET, and
//     refuses a secret that has a name and neither a value nor a Key Vault address, so the values of
//     the secrets kept are read with the app's own listSecrets, in apply mode only, held in memory,
//     hidden from every output and sent back only to the same app in the same PATCH.

func init() {
	register(command{words: []string{"containerapp", "secret", "set"}, usage: "<app> secret=<NAME>=<connection>.<field>... [container=<name>]", run: (*plugin).appSecretSet})
}

const secretSetUsage = "Refused: containerapp secret set takes <app>, then one or more secret=<NAME>=<connection>.<field>, the field of a connection bound to this member's appSecrets slot, such as secret=SMTP_PASSWORD=mail.password, and optionally container=<name> when the app runs more than one container."

func (p *plugin) appSecretSet(args []string) (string, error) {
	const cmd = "containerapp secret set"
	if len(args) < 2 {
		return "", refusal(secretSetUsage)
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	name := args[0]
	if !appName.MatchString(name) {
		return "", refusef("Refused: %q is not a container app name.", name)
	}

	// EVERY ARGUMENT IS READ, AND EVERY CONNECTION FOUND, BEFORE AZURE IS ASKED ANYTHING.
	var secrets []appSecret
	container := ""
	seen, secretSeen := map[string]bool{}, map[string]bool{}
	for _, a := range args[1:] {
		key, value, _ := strings.Cut(a, "=")
		switch strings.ToLower(key) {
		case "container":
			if container != "" || !appName.MatchString(value) {
				return "", refusef("Refused: %s is not container=<name>, given once.", a)
			}
			container = value
		case "secret":
			v, val, ok := strings.Cut(value, "=")
			if !ok || !envVarName.MatchString(v) {
				return "", refusef("Refused: %s must be secret=<NAME>=<connection>.<field>, NAME being letters, digits and underscores.", a)
			}
			if seen[v] {
				return "", refusef("Refused: %s is given twice.", v)
			}
			seen[v] = true
			sname := appSecretName(v)
			if secretSeen[sname] {
				return "", refusef("Refused: %s would be the app's secret %s, which another secret= already names.", v, sname)
			}
			secretSeen[sname] = true
			s, err := p.secretArg(cmd, v, val, false)
			if err != nil {
				return "", err
			}
			secrets = append(secrets, s)
		default:
			return "", refusef("Refused: %s is not secret= or container=. %s", a, strings.TrimPrefix(secretSetUsage, "Refused: "))
		}
	}
	if len(secrets) == 0 {
		return "", refusal(secretSetUsage)
	}

	r, app, err := p.containerApp(name)
	if err != nil {
		return "", err
	}
	if err := p.mustOwn(change{id: r.ID, kind: changeModify}); err != nil {
		return "", err
	}
	pr := props(app)
	cfg, _ := pr["configuration"].(map[string]any)
	tpl, _ := pr["template"].(map[string]any)
	containers := anyList(tpl["containers"])
	target, err := pickContainer(r.Name, containers, container)
	if err != nil {
		return "", err
	}
	existing := anyList(cfg["secrets"])

	// The plan, in names only.
	named, byEnv := map[string]appSecret{}, map[string]appSecret{}
	for _, s := range secrets {
		named[s.name], byEnv[s.env] = s, s
	}
	var added, updated, keptVault, keptValue []string
	for _, s := range existing {
		n := str(s["name"])
		switch {
		case named[n].name != "":
			updated = append(updated, n)
		case str(s["keyVaultUrl"]) != "":
			keptVault = append(keptVault, n)
		default:
			keptValue = append(keptValue, n)
		}
	}
	has := map[string]bool{}
	for _, s := range existing {
		has[str(s["name"])] = true
	}
	for _, s := range secrets {
		if !has[s.name] {
			added = append(added, s.name)
		}
	}
	envOf := map[string]bool{}
	for _, e := range anyList(containers[target]["env"]) {
		envOf[str(e["name"])] = true
	}
	var envAdd, envSet []string
	newRef := false // an environment variable gets a secret reference it did not have, so a new revision starts
	for _, s := range secrets {
		if envOf[s.env] {
			envSet = append(envSet, s.env)
		} else {
			envAdd = append(envAdd, s.env)
		}
	}
	for _, e := range anyList(containers[target]["env"]) {
		if s, ok := byEnv[str(e["name"])]; ok && str(e["secretRef"]) != s.name {
			newRef = true
		}
	}
	newRef = newRef || len(envAdd) > 0

	var reads []string
	for _, s := range secrets {
		reads = append(reads, fmt.Sprintf("%s from %s as secret %s", s.env, s.field, s.name))
	}
	steps := []string{fmt.Sprintf("set on container app %s, container %s: %s", r.Name, str(containers[target]["name"]), strings.Join(reads, ", "))}
	if len(added) > 0 {
		steps = append(steps, "add secrets "+strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		steps = append(steps, "replace the values of secrets "+strings.Join(updated, ", "))
	}
	if len(envAdd) > 0 {
		steps = append(steps, "add environment variables "+strings.Join(envAdd, ", "))
	}
	if len(envSet) > 0 {
		steps = append(steps, "point environment variables "+strings.Join(envSet, ", ")+" at their secrets")
	}
	steps = append(steps, "keep "+keepsSaying(keptVault, keptValue))
	what := strings.Join(steps, "; ")
	if !p.cfg.apply() {
		return p.whatif(cmd, r.ID, what, map[string]int{"modify": 1}), nil
	}

	// APPLY. The kept value secrets' values, from the app itself; never written out.
	values := map[string]string{}
	if len(keptValue) > 0 {
		p.progress("azure: reading the secrets " + r.Name + " keeps, to send them back unchanged")
		var listed struct {
			Value []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"value"`
		}
		if _, err := p.az.arm(http.MethodPost, r.ID+"/listSecrets?api-version="+versionOf("Microsoft.App/containerApps"), nil, &listed); err != nil {
			return "", fmt.Errorf("the secrets container app %s keeps could not be read, so nothing was changed: %w", r.Name, err)
		}
		for _, s := range listed.Value {
			p.hide(s.Value)
			values[s.Name] = s.Value
		}
		for _, n := range keptValue {
			if values[n] == "" {
				return "", refusef("Refused: container app %s keeps its secret %s, but Azure did not answer its value to send back, and a write without it would remove it. Name it again with secret=<NAME>=<connection>.<field>. Nothing was changed.", r.Name, n)
			}
		}
	}
	var outSecrets []map[string]any
	for _, s := range existing {
		n := str(s["name"])
		if ns, ok := named[n]; ok {
			outSecrets = append(outSecrets, map[string]any{"name": n, "value": ns.value})
			continue
		}
		if url := str(s["keyVaultUrl"]); url != "" {
			ks := map[string]any{"name": n, "keyVaultUrl": url}
			if id := str(s["identity"]); id != "" {
				ks["identity"] = id
			}
			outSecrets = append(outSecrets, ks)
			continue
		}
		outSecrets = append(outSecrets, map[string]any{"name": n, "value": values[n]})
	}
	for _, s := range secrets {
		if !has[s.name] {
			outSecrets = append(outSecrets, map[string]any{"name": s.name, "value": s.value})
		}
	}
	outContainers := make([]map[string]any, len(containers))
	for i, c := range containers {
		outContainers[i] = c
		if i != target {
			continue
		}
		cc := map[string]any{}
		for k, v := range c {
			cc[k] = v
		}
		var env []map[string]any
		done := map[string]bool{}
		for _, e := range anyList(c["env"]) {
			en := str(e["name"])
			if s, ok := byEnv[en]; ok {
				env = append(env, map[string]any{"name": en, "secretRef": s.name})
				done[en] = true
				continue
			}
			env = append(env, e)
		}
		for _, s := range secrets {
			if !done[s.env] {
				env = append(env, map[string]any{"name": s.env, "secretRef": s.name})
			}
		}
		cc["env"] = env
		outContainers[i] = cc
	}
	body := map[string]any{"properties": map[string]any{
		"configuration": map[string]any{"secrets": outSecrets},
		"template":      map[string]any{"containers": outContainers},
	}}
	p.progress("azure: setting secrets on container app " + r.Name + " (a few minutes)")
	ans, err := p.az.arm(http.MethodPatch, r.ID+"?api-version="+versionOf("Microsoft.App/containerApps"), body, nil)
	if err == nil {
		_, err = p.az.wait(ans, "the container app "+r.Name)
	}
	if err != nil {
		return "", err
	}
	p.appliedAs(cmd, r.ID, what, []string{r.ID}, func(string) string { return actionChanged })
	answer := fmt.Sprintf("Container app %s now reads %s, from this member's connections. It keeps %s: scale, probes, ingress, image, registries and identity are as they were.", r.Name, strings.Join(reads, ", "), keepsSaying(keptVault, keptValue))
	if !newRef {
		answer += " Every variable already read its secret, so no new revision starts: run containerapp restart " + r.Name + " for the running revision to read the new values."
	}
	return answer, nil
}

// keepsSaying names, for the plan and the answer, the secrets an app keeps as they are.
func keepsSaying(vault, value []string) string {
	var parts []string
	if len(vault) > 0 {
		parts = append(parts, "Key Vault secrets "+strings.Join(vault, ", "))
	}
	if len(value) > 0 {
		parts = append(parts, "secrets "+strings.Join(value, ", "))
	}
	if len(parts) == 0 {
		return "every other setting as it is (it has no other secrets)"
	}
	return "its other " + strings.Join(parts, " and ") + " and every other setting as they are"
}

// pickContainer answers which of an app's containers gets the environment variables: the one
// named container= when given, else the one named as the app, else the only one.
func pickContainer(app string, containers []map[string]any, named string) (int, error) {
	var names []string
	for _, c := range containers {
		names = append(names, str(c["name"]))
	}
	if named != "" {
		for i, n := range names {
			if strings.EqualFold(n, named) {
				return i, nil
			}
		}
		return 0, refusef("Refused: container app %s has no container %s; its containers are %s. Nothing was changed.", app, named, strings.Join(names, ", "))
	}
	switch len(containers) {
	case 0:
		return 0, refusef("Refused: container app %s has no containers to set environment variables on. Nothing was changed.", app)
	case 1:
		return 0, nil
	}
	for i, n := range names {
		if strings.EqualFold(n, app) {
			return i, nil
		}
	}
	return 0, refusef("Refused: container app %s runs the containers %s, so say which one reads the secrets with container=<name>. Nothing was changed.", app, strings.Join(names, ", "))
}
