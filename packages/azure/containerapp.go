package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Container Apps: an environment whose logs go to its own Log Analytics workspace, and apps in it
// that pull their image with a managed identity the plugin creates for each, and read their secrets
// from Key Vault through that identity, or from this member's own keys (appSecret1 to appSecret8,
// which a person binds and sets on the screen). No registry password is ever part of an app, and a
// secret's value is put only in the app's configuration, never in what the plugin writes out.
// Logs are read through Azure management's query on the workspace resource.

const (
	diagAPIVersion  = "2021-05-01-preview"
	logsAPIVersion  = "2017-01-01-preview"
	maxLogLines     = 500
	appSecretFields = 8
)

var (
	appName    = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9]){0,29}[a-z0-9]?$`)
	envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	acrImage   = regexp.MustCompile(`^([a-z0-9]{5,50})\.azurecr\.io/(.+)$`)
	appSecretN = regexp.MustCompile(`(?i)^appsecret([0-9]+)$`)
)

// appSecretField answers the canonical field name (appSecret1 to appSecret8) of a value such as
// appSecret3, and whether it names one of the eight.
func appSecretField(v string) (string, bool) {
	m := appSecretN.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 || n > appSecretFields || strconv.Itoa(n) != m[1] {
		return "", false
	}
	return "appSecret" + m[1], true
}

func init() {
	register(
		command{words: []string{"containerapp", "env", "create"}, usage: "<resource group> <environment>", run: (*plugin).envCreate},
		command{words: []string{"containerapp", "create"}, usage: "<resource group> <app> <environment> <registry>.azurecr.io/<image:tag> [port=<n>] [cpu=<0.25 to 2>] [env=<NAME>=<value>]... [secret=<NAME>=<Key Vault secret address>|appSecret<1-8>]...", run: (*plugin).appCreate},
		command{words: []string{"containerapp", "show"}, usage: "<app>", run: (*plugin).appShow},
		command{words: []string{"containerapp", "revisions"}, usage: "<app>", run: (*plugin).appRevisions},
		command{words: []string{"containerapp", "restart"}, usage: "<app>", run: (*plugin).appRestart},
		command{words: []string{"containerapp", "logs"}, usage: "<app> [minutes]", run: (*plugin).appLogs},
	)
}

func (p *plugin) envCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "containerapp env create", "<resource group> <environment>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name := args[0], args[1]
	if !appName.MatchString(name) {
		return "", refusef("Refused: %q is not an environment name: 2 to 32 lower-case letters, digits and single hyphens, starting with a letter.", name)
	}
	base := rgPath(p.cfg.Subscription, rg)
	ws := base + "/providers/Microsoft.OperationalInsights/workspaces/" + name + "-logs"
	env := base + "/providers/Microsoft.App/managedEnvironments/" + name
	diag := env + "/providers/Microsoft.Insights/diagnosticSettings/yawble-logs"
	for _, c := range []change{{id: ws, kind: changeCreate}, {id: env, kind: changeCreate}, {id: diag, kind: changeModify}} {
		if err := p.mustOwn(c); err != nil {
			return "", err
		}
	}
	what := fmt.Sprintf("create Log Analytics workspace %s-logs and Container Apps environment %s in %s, the environment's logs going to the workspace, both tagged for this team", name, name, rg)
	if !p.cfg.apply() {
		return p.whatif("containerapp env create", env, what, map[string]int{"create": 3}), nil
	}
	location, err := p.rgLocation(rg)
	if err != nil {
		return "", err
	}
	p.progress("azure: creating workspace " + name + "-logs")
	if _, err := p.put(ws, map[string]any{
		"location": location, "tags": p.ownTags(),
		"properties": map[string]any{"sku": map[string]any{"name": "PerGB2018"}, "retentionInDays": 30},
	}, "the workspace "+name+"-logs"); err != nil {
		return "", err
	}
	p.progress("azure: creating environment " + name + " (a few minutes)")
	if _, err := p.put(env, map[string]any{
		"location": location, "tags": p.ownTags(),
		"properties": map[string]any{"appLogsConfiguration": map[string]any{"destination": "azure-monitor"}},
	}, "the environment "+name); err != nil {
		p.applied("containerapp env create", env, "created workspace "+name+"-logs", []string{ws})
		return "", err
	}
	if _, err := p.az.arm(http.MethodPut, diag+"?api-version="+diagAPIVersion, map[string]any{"properties": map[string]any{
		"workspaceId": ws,
		"logs":        []map[string]any{{"categoryGroup": "allLogs", "enabled": true}},
	}}, nil); err != nil {
		p.applied("containerapp env create", env, "created workspace "+name+"-logs and environment "+name, []string{ws, env})
		return "", fmt.Errorf("environment %s is ready, but its logs could not be sent to %s-logs: %w", name, name, err)
	}
	p.applied("containerapp env create", env, what, []string{ws, env, diag})
	return fmt.Sprintf("Container Apps environment %s is ready in %s; its apps' logs go to workspace %s-logs, which containerapp logs reads through Azure management.", name, rg, name), nil
}

// appSecret is one secret=NAME=... argument: an environment variable read from Key Vault (address,
// vault and secret set), or from one of this member's own keys (field set, value its value).
type appSecret struct {
	env, name, address, vault, secret string
	field, value                      string
}

// keyRefusal is the sentence for secret=NAME=appSecretN when the member has no value for the field.
// It names the key when the request names it, and never a value.
func (p *plugin) keyRefusal(env, field string) refusal {
	if key := p.appSecretKeys[field]; key != "" {
		return refusef("Refused: secret=%s=%s names this member's %s, which is bound to the key %s, and that key is not set. A person sets %s on the screen first (this member's settings), then runs containerapp create again. Nothing was changed.", env, field, field, key, key)
	}
	return refusef("Refused: secret=%s=%s names this member's %s, which this member does not bind to a key. A person binds %s to a key and sets it on the screen first (this member's settings), then runs containerapp create again. Nothing was changed.", env, field, field, field)
}

func (p *plugin) appCreate(args []string) (string, error) {
	if len(args) < 4 {
		return "", refusal("Refused: containerapp create takes <resource group> <app> <environment> <registry>.azurecr.io/<image:tag>, then optionally port=<n>, cpu=<0.25 to 2> (its memory is twice that in GiB; 0.5 by default), env=<NAME>=<value> and secret=<NAME>=https://<vault>.vault.azure.net/secrets/<name> or secret=<NAME>=appSecret<1-8>.")
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name, envName, image := args[0], args[1], args[2], args[3]
	if !appName.MatchString(name) {
		return "", refusef("Refused: %q is not an app name: 2 to 32 lower-case letters, digits and single hyphens, starting with a letter.", name)
	}
	m := acrImage.FindStringSubmatch(image)
	if m == nil || !imageForm.MatchString(m[2]) {
		return "", refusef("Refused: %q is not an image in an Azure container registry, such as <registry>.azurecr.io/my-app:1.0. This member runs images only from registries its team owns and those in sharedRegistries.", image)
	}
	port := 0
	cpu := 0.5
	var envs []map[string]any
	var secrets []appSecret
	seen, secretSeen := map[string]bool{}, map[string]bool{}
	for _, a := range args[4:] {
		key, value, _ := strings.Cut(a, "=")
		switch strings.ToLower(key) {
		case "port":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 {
				return "", refusef("Refused: %s is not a port.", a)
			}
			port = n
		case "cpu":
			// The sizes a Consumption environment runs: a quarter of a CPU at a time, with twice as
			// many GiB of memory.
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || n < 0.25 || n > 2 || n*4 != float64(int(n*4)) {
				return "", refusef("Refused: %s is not a size Azure runs: cpu takes 0.25, 0.5, 0.75 and so on up to 2, and the app gets twice that in GiB of memory.", a)
			}
			cpu = n
		case "env", "secret":
			v, val, ok := strings.Cut(value, "=")
			if !ok || !envVarName.MatchString(v) {
				return "", refusef("Refused: %s must be %s=<NAME>=<value>, NAME being letters, digits and underscores.", a, strings.ToLower(key))
			}
			if seen[v] {
				return "", refusef("Refused: %s is given twice.", v)
			}
			seen[v] = true
			if strings.EqualFold(key, "env") {
				if secretParam.MatchString(v) || credentialShaped[0].MatchString(val) || credentialShaped[1].MatchString(val) {
					return "", refusef("Refused: %s looks like a secret. App secrets go in Key Vault or in this member's own keys: pass it as secret=%s=https://<vault>.vault.azure.net/secrets/<name> for a Key Vault secret, set by a person or another plugin, or as secret=%s=appSecret<1-8> for one of this member's keys, which a person binds and sets on the screen (this member's settings).", v, v, v)
				}
				envs = append(envs, map[string]any{"name": v, "value": val})
				continue
			}
			sname := strings.ReplaceAll(strings.ToLower(v), "_", "-")
			if secretSeen[sname] {
				return "", refusef("Refused: %s would be the app's secret %s, which another secret= already names.", v, sname)
			}
			secretSeen[sname] = true
			if appSecretN.MatchString(val) {
				field, ok := appSecretField(val)
				if !ok {
					return "", refusef("Refused: secret=%s=%s names no field of this member; its own keys are appSecret1 to appSecret%d.", v, val, appSecretFields)
				}
				value := p.appSecrets[field]
				if value == "" {
					return "", p.keyRefusal(v, field)
				}
				secrets = append(secrets, appSecret{env: v, name: sname, field: field, value: value})
				continue
			}
			sm := secretRefForm.FindStringSubmatch(val)
			if sm == nil {
				return "", refusef("Refused: secret=%s takes a Key Vault secret's address, https://<vault>.vault.azure.net/secrets/<name>, or one of this member's own keys, appSecret1 to appSecret%d; never a value.", v, appSecretFields)
			}
			secrets = append(secrets, appSecret{env: v, name: sname, address: val, vault: sm[1], secret: sm[2]})
		default:
			return "", refusef("Refused: %s is not port=, cpu=, env= or secret=.", a)
		}
	}

	base := rgPath(p.cfg.Subscription, rg)
	appID := base + "/providers/Microsoft.App/containerApps/" + name
	identityID := base + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/" + name + "-id"
	env, err := p.findByName("Microsoft.App/managedEnvironments", envName, "Container Apps environment")
	if err != nil {
		return "", err
	}
	registry, err := p.findByName("Microsoft.ContainerRegistry/registries", m[1], "container registry")
	if err != nil {
		return "", err
	}
	for _, c := range []change{{id: appID, kind: changeCreate}, {id: identityID, kind: changeCreate}, {id: env.ID, kind: changeModify}} {
		if err := p.mustOwn(c); err != nil {
			return "", err
		}
	}
	if why, err := p.guard(change{id: registry.ID, kind: changeGrant, role: roleAcrPull}); err != nil {
		return "", err
	} else if why != "" {
		return "", refusef("Refused: %s. This member runs images only from registries its team owns and those in sharedRegistries. Nothing was changed.", why)
	}
	vaults := map[string]resource{}
	vaultSecrets, keySecrets := 0, 0
	for _, s := range secrets {
		if s.field != "" {
			keySecrets++
			continue
		}
		vaultSecrets++
		if _, ok := vaults[strings.ToLower(s.vault)]; ok {
			continue
		}
		v, err := p.findByName("Microsoft.KeyVault/vaults", s.vault, "Key Vault")
		if err != nil {
			return "", err
		}
		if why, err := p.guard(change{id: v.ID, kind: changeGrant, role: roleKeyVaultSecretsUser}); err != nil {
			return "", err
		} else if why != "" {
			return "", refusef("Refused: %s. An app reads secrets only from a Key Vault its team owns. Nothing was changed.", why)
		}
		names, err := p.secretNames(v)
		if err != nil {
			return "", err
		}
		vaults[strings.ToLower(s.vault)] = v
		for _, s2 := range secrets {
			if s2.field == "" && strings.EqualFold(s2.vault, s.vault) && !contains(names, s2.secret) {
				return "", refusef("Refused: Key Vault %s holds no secret %s. A person (or another plugin) sets it there first; this member never sets or reads a secret's value.", v.Name, s2.secret)
			}
		}
	}

	server := registryHost(registry.Name)
	steps := []string{
		"create managed identity " + name + "-id in " + rg,
		"grant it AcrPull on registry " + registry.Name,
	}
	for _, v := range vaults {
		steps = append(steps, "grant it Key Vault Secrets User on Key Vault "+v.Name)
	}
	ingress := "no ingress"
	if port > 0 {
		ingress = fmt.Sprintf("public HTTPS ingress to port %d", port)
	}
	steps = append(steps, fmt.Sprintf("create or update container app %s in environment %s running %s on %s, with %s, %d settings, %d Key Vault secrets and %d secrets from this member's keys", name, env.Name, image, appSize(cpu), ingress, len(envs), vaultSecrets, keySecrets))
	what := strings.Join(steps, "; ")
	if !p.cfg.apply() {
		return p.whatif("containerapp create", appID, what, map[string]int{"create": 2 + 1 + len(vaults)}), nil
	}

	location := env.Location
	if location == "" {
		if location, err = p.rgLocation(rg); err != nil {
			return "", err
		}
	}
	ids := []string{}
	done := func() { p.applied("containerapp create", appID, "partly: "+what, ids) }
	identity, err := p.put(identityID, map[string]any{"location": location, "tags": p.ownTags()}, "the identity "+name+"-id")
	if err != nil {
		return "", err
	}
	ids = append(ids, identityID)
	principal, _ := props(identity)["principalId"].(string)
	if principal == "" {
		done()
		return "", fmt.Errorf("the identity %s-id was created, but Azure gave no principal for it yet; run containerapp create again in a minute", name)
	}
	grants := []struct{ scope, role string }{{registry.ID, roleAcrPull}}
	for _, v := range vaults {
		grants = append(grants, struct{ scope, role string }{v.ID, roleKeyVaultSecretsUser})
	}
	for _, g := range grants {
		assignment, _, err := p.grantRole(g.scope, g.role, principal)
		if err != nil {
			done()
			return "", fmt.Errorf("%s could not be granted on %s: %w", grantable[g.role], g.scope, err)
		}
		ids = append(ids, assignment)
	}

	var appSecrets []map[string]any
	for _, s := range secrets {
		if s.field != "" {
			appSecrets = append(appSecrets, map[string]any{"name": s.name, "value": s.value})
		} else {
			appSecrets = append(appSecrets, map[string]any{"name": s.name, "keyVaultUrl": s.address, "identity": identityID})
		}
		envs = append(envs, map[string]any{"name": s.env, "secretRef": s.name})
	}
	sort.Slice(envs, func(i, j int) bool { return envs[i]["name"].(string) < envs[j]["name"].(string) })
	config := map[string]any{
		"activeRevisionsMode": "Single",
		"registries":          []map[string]any{{"server": server, "identity": identityID}},
	}
	if len(appSecrets) > 0 {
		config["secrets"] = appSecrets
	}
	if port > 0 {
		config["ingress"] = map[string]any{"external": true, "targetPort": port, "transport": "auto", "allowInsecure": false}
	}
	body := map[string]any{
		"location": location, "tags": p.ownTags(),
		"identity": map[string]any{"type": "UserAssigned", "userAssignedIdentities": map[string]any{identityID: map[string]any{}}},
		"properties": map[string]any{
			"managedEnvironmentId": env.ID,
			"configuration":        config,
			"template": map[string]any{
				"containers": []map[string]any{{"name": name, "image": image, "env": envs, "resources": map[string]any{"cpu": cpu, "memory": appMemory(cpu)}}},
				"scale":      map[string]any{"minReplicas": 0, "maxReplicas": 1},
			},
		},
	}
	p.progress("azure: creating container app " + name + " (a few minutes)")
	app, err := p.put(appID, body, "the container app "+name)
	if err != nil {
		done()
		return "", fmt.Errorf("%w. A new identity's pull right can take a few minutes to reach the registry: run containerapp create again then", err)
	}
	ids = append(ids, appID)
	p.applied("containerapp create", appID, what, ids)
	answer := fmt.Sprintf("Container app %s runs %s in environment %s, pulling with its own identity %s-id.", name, image, env.Name, name)
	if fqdn := appFQDN(app); fqdn != "" {
		answer += " It answers at https://" + fqdn + "."
	}
	return answer, nil
}

// appFQDN is the address an app with ingress answers at.
func appFQDN(app map[string]any) string {
	cfg, _ := props(app)["configuration"].(map[string]any)
	ing, _ := cfg["ingress"].(map[string]any)
	fqdn, _ := ing["fqdn"].(string)
	return fqdn
}

// containerApp finds an app by name and reads it.
func (p *plugin) containerApp(name string) (resource, map[string]any, error) {
	if err := p.needSubscription(); err != nil {
		return resource{}, nil, err
	}
	if !appName.MatchString(name) {
		return resource{}, nil, refusef("Refused: %q is not a container app name.", name)
	}
	r, err := p.findByName("Microsoft.App/containerApps", name, "container app")
	if err != nil {
		return resource{}, nil, err
	}
	var app map[string]any
	if _, err := p.az.get(r.ID+"?api-version="+versionOf("Microsoft.App/containerApps"), &app); err != nil {
		return resource{}, nil, err
	}
	return r, app, nil
}

func (p *plugin) appShow(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "containerapp show", "<app>"); err != nil {
		return "", err
	}
	r, app, err := p.containerApp(args[0])
	if err != nil {
		return "", err
	}
	pr := props(app)
	cfg, _ := pr["configuration"].(map[string]any)
	ing, _ := cfg["ingress"].(map[string]any)
	tpl, _ := pr["template"].(map[string]any)
	var images []string
	if cs, ok := tpl["containers"].([]any); ok {
		for _, c := range cs {
			if cm, ok := c.(map[string]any); ok {
				images = append(images, str(cm["image"]))
			}
		}
	}
	var domains []string
	if ds, ok := ing["customDomains"].([]any); ok {
		for _, d := range ds {
			if dm, ok := d.(map[string]any); ok {
				domains = append(domains, fmt.Sprintf("%s (%s)", str(dm["name"]), str(dm["bindingType"])))
			}
		}
	}
	tags := map[string]string{}
	for k, v := range stringMap(app["tags"]) {
		tags[k] = v
	}
	state := "not owned by this team (read only)"
	if p.ownedBy(tags) {
		state = "owned by this team"
	}
	or := func(s string) string {
		if s == "" {
			return "none"
		}
		return s
	}
	lines := []string{
		"- id: " + r.ID,
		"- " + state,
		"- state: " + or(str(pr["provisioningState"])) + ", running " + or(str(pr["runningStatus"])),
		"- address: " + or(str(ing["fqdn"])),
		"- images: " + or(strings.Join(images, ", ")),
		"- latest revision: " + or(str(pr["latestRevisionName"])) + ", ready: " + or(str(pr["latestReadyRevisionName"])),
		"- custom domains: " + or(strings.Join(domains, ", ")),
	}
	return "Container app " + r.Name + ":\n" + strings.Join(lines, "\n"), nil
}

func (p *plugin) appRevisions(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "containerapp revisions", "<app>"); err != nil {
		return "", err
	}
	r, _, err := p.containerApp(args[0])
	if err != nil {
		return "", err
	}
	var lines []string
	err = p.az.list(r.ID+"/revisions?api-version="+versionOf("Microsoft.App/containerApps"), func(raw json.RawMessage) {
		var rev struct {
			Name       string `json:"name"`
			Properties struct {
				Active        bool   `json:"active"`
				Created       string `json:"createdTime"`
				Replicas      int    `json:"replicas"`
				TrafficWeight int    `json:"trafficWeight"`
				Health        string `json:"healthState"`
				Running       string `json:"runningState"`
			} `json:"properties"`
		}
		if json.Unmarshal(raw, &rev) == nil {
			active := "inactive"
			if rev.Properties.Active {
				active = "active"
			}
			lines = append(lines, fmt.Sprintf("- %s: %s, %s, %s, %d replicas, %d%% of traffic, created %s", rev.Name, active,
				strings.ToLower(rev.Properties.Running), strings.ToLower(rev.Properties.Health), rev.Properties.Replicas, rev.Properties.TrafficWeight, rev.Properties.Created))
		}
	})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "Container app " + r.Name + " has no revisions.", nil
	}
	sort.Strings(lines)
	return fmt.Sprintf("Revisions of %s (%d):\n%s", r.Name, len(lines), strings.Join(lines, "\n")), nil
}

func (p *plugin) appRestart(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "containerapp restart", "<app>"); err != nil {
		return "", err
	}
	r, app, err := p.containerApp(args[0])
	if err != nil {
		return "", err
	}
	if err := p.mustOwn(change{id: r.ID, kind: changeModify}); err != nil {
		return "", err
	}
	pr := props(app)
	rev := str(pr["latestReadyRevisionName"])
	if rev == "" {
		rev = str(pr["latestRevisionName"])
	}
	if rev == "" {
		return "", refusef("Refused: container app %s has no revision to restart.", r.Name)
	}
	what := "restart revision " + rev + " of container app " + r.Name
	if !p.cfg.apply() {
		return p.whatif("containerapp restart", r.ID, what, map[string]int{"modify": 1}), nil
	}
	if _, err := p.az.arm(http.MethodPost, r.ID+"/revisions/"+rev+"/restart?api-version="+versionOf("Microsoft.App/containerApps"), nil, nil); err != nil {
		return "", err
	}
	p.applied("containerapp restart", r.ID, what, []string{r.ID})
	return "Restarted revision " + rev + " of " + r.Name + ".", nil
}

func (p *plugin) appLogs(args []string) (string, error) {
	if err := argsExactly(args, 1, 2, "containerapp logs", "<app> [minutes]"); err != nil {
		return "", err
	}
	minutes := 30
	if len(args) == 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > 1440 {
			return "", refusal("Refused: containerapp logs takes a number of minutes from 1 to 1440.")
		}
		minutes = n
	}
	r, app, err := p.containerApp(args[0])
	if err != nil {
		return "", err
	}
	workspace, err := p.logWorkspace(str(props(app)["managedEnvironmentId"]))
	if err != nil {
		return "", err
	}
	query := fmt.Sprintf(`union isfuzzy=true (ContainerAppConsoleLogs | where ContainerAppName == '%s' | project TimeGenerated, Log), (ContainerAppConsoleLogs_CL | where ContainerAppName_s == '%s' | project TimeGenerated, Log = Log_s) | top %d by TimeGenerated desc`, r.Name, r.Name, maxLogLines)
	// Azure management's query answers each column's name as ColumnName; name is read too.
	var out struct {
		Tables []struct {
			Columns []struct {
				ColumnName string `json:"ColumnName"`
				Name       string `json:"name"`
			} `json:"columns"`
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	body := map[string]any{"query": query, "timespan": fmt.Sprintf("PT%dM", minutes)}
	if _, err := p.az.arm(http.MethodPost, workspace+"/api/query?api-version="+logsAPIVersion, body, &out); err != nil {
		if isStatus(err, http.StatusForbidden) {
			return "", refusef("Refused: the signed-in account cannot query the logs of %s. It needs Log Analytics Reader (or Reader, or more) on the workspace %s; its owner gives it in the Azure portal (the workspace, Access control (IAM), Add role assignment).", r.Name, workspace[strings.LastIndex(workspace, "/")+1:])
		}
		return "", err
	}
	var lines []string
	if len(out.Tables) > 0 {
		t := out.Tables[0]
		ti, li := -1, -1
		for i, c := range t.Columns {
			col := c.ColumnName
			if col == "" {
				col = c.Name
			}
			switch col {
			case "TimeGenerated":
				ti = i
			case "Log":
				li = i
			}
		}
		for i := len(t.Rows) - 1; i >= 0; i-- { // oldest first
			row := t.Rows[i]
			if ti < 0 || li < 0 || ti >= len(row) || li >= len(row) {
				continue
			}
			lines = append(lines, str(row[ti])+"  "+strings.TrimRight(str(row[li]), "\n"))
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Container app %s wrote no log lines in the last %d minutes (they reach the workspace a few minutes late).", r.Name, minutes), nil
	}
	return fmt.Sprintf("Logs of %s, the last %d minutes (%d lines, at most %d, oldest first):\n%s", r.Name, minutes, len(lines), maxLogLines, strings.Join(lines, "\n")), nil
}

// logWorkspace answers the Azure Resource Manager id of the Log Analytics workspace an environment's
// logs go to: for an environment that names the workspace's customerId (not a secret), the
// workspace in the subscription with that customerId; otherwise the workspace its diagnostic
// settings name.
func (p *plugin) logWorkspace(envID string) (string, error) {
	if envID == "" {
		return "", refusal("Refused: the app names no environment.")
	}
	var env struct {
		Properties struct {
			Logs struct {
				LogAnalytics struct {
					CustomerID string `json:"customerId"`
				} `json:"logAnalyticsConfiguration"`
			} `json:"appLogsConfiguration"`
		} `json:"properties"`
	}
	if _, err := p.az.get(envID+"?api-version="+versionOf("Microsoft.App/managedEnvironments"), &env); err != nil {
		return "", err
	}
	if cid := env.Properties.Logs.LogAnalytics.CustomerID; cid != "" {
		found := ""
		err := p.az.list("/subscriptions/"+p.cfg.Subscription+"/providers/Microsoft.OperationalInsights/workspaces?api-version="+versionOf("Microsoft.OperationalInsights/workspaces"), func(raw json.RawMessage) {
			var ws struct {
				ID         string `json:"id"`
				Properties struct {
					CustomerID string `json:"customerId"`
				} `json:"properties"`
			}
			if json.Unmarshal(raw, &ws) == nil && found == "" && strings.EqualFold(ws.Properties.CustomerID, cid) {
				found = ws.ID
			}
		})
		if err != nil {
			return "", err
		}
		if id, ok := workspaceID(found); ok {
			return id, nil
		}
		return "", refusef("Refused: the app's environment sends its logs to the Log Analytics workspace %s, which this sign-in cannot see in the subscription. Its owner gives this account Log Analytics Reader (or Reader) on that workspace.", cid)
	}
	var diags struct {
		Value []struct {
			Properties struct {
				WorkspaceID string `json:"workspaceId"`
			} `json:"properties"`
		} `json:"value"`
	}
	if _, err := p.az.arm(http.MethodGet, envID+"/providers/Microsoft.Insights/diagnosticSettings?api-version="+diagAPIVersion, nil, &diags); err != nil {
		return "", err
	}
	for _, d := range diags.Value {
		if id, ok := workspaceID(d.Properties.WorkspaceID); ok {
			return id, nil
		}
	}
	return "", refusal("Refused: the app's environment sends its logs to no Log Analytics workspace. Create environments with containerapp env create, which does.")
}

// workspaceID answers id, without a trailing slash, when it is the Azure Resource Manager id of a
// Log Analytics workspace.
func workspaceID(id string) (string, bool) {
	id = strings.TrimSuffix(id, "/")
	r, ok := parseID(id)
	if !ok || !strings.HasPrefix(strings.ToLower(id), "/subscriptions/") || !strings.EqualFold(r.typ(), "Microsoft.OperationalInsights/workspaces") {
		return "", false
	}
	return id, true
}

// appMemory is the memory a Consumption app of that many CPUs gets: twice as many GiB.
func appMemory(cpu float64) string {
	return strconv.FormatFloat(cpu*2, 'f', -1, 64) + "Gi"
}

// appSize says an app's size in words.
func appSize(cpu float64) string {
	return strconv.FormatFloat(cpu, 'f', -1, 64) + " CPU and " + strings.TrimSuffix(appMemory(cpu), "Gi") + " GiB of memory"
}
