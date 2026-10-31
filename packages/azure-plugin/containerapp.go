package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Container Apps: an environment whose logs go to its own Log Analytics workspace, and apps in it
// that pull their image with a managed identity the plugin creates for each, and read their secrets
// from Key Vault through that identity, or from a field of an API-key connection bound to this
// member's appSecrets slot (added in Team settings -> Connections), or from one of its old named
// secrets. No registry password is ever part of an app, and a
// secret's value is put only in the app's configuration, never in what the plugin writes out.
// Logs are read through Azure management's query on the workspace resource, or, for an environment
// whose diagnostic setting archives its logs to a storage account instead, from that account's
// hourly blobs.

const (
	diagAPIVersion  = "2021-05-01-preview"
	logsAPIVersion  = "2017-01-01-preview"
	maxLogLines     = 500
	appSecretFields = 8                 // appSecret1 to appSecret8, which 3.0.0 moved to the appSecrets slot
	clientIDVar     = "AZURE_CLIENT_ID" // the variable Azure's SDKs read for a user-assigned identity
)

var (
	appName    = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9]){0,29}[a-z0-9]?$`)
	envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	acrImage   = regexp.MustCompile(`^([a-z0-9]{5,50})\.azurecr\.io/(.+)$`)
	secretName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`) // a named secret's name, as the Host holds it
)

// namedSecret answers the name and value of the member's named secret called name. A name is unique
// per member ignoring case, so it matches ignoring case too.
func (p *plugin) namedSecret(name string) (string, string, bool) {
	if v, ok := p.namedSecrets[name]; ok {
		return name, v, true
	}
	for n, v := range p.namedSecrets {
		if strings.EqualFold(n, name) {
			return n, v, true
		}
	}
	return "", "", false
}

// connectionSecret answers the field name and value of the field called name in the connections
// bound to the appSecrets slot, matched ignoring case, and whether one holds it. Two connections
// holding a field by that name with different values is a refusal naming both, never a value.
func (p *plugin) connectionSecret(env, name string) (string, string, bool, error) {
	field, value, from, at := "", "", "", 0
	for i, k := range p.keys {
		for f, v := range k.Fields {
			if !strings.EqualFold(f, name) || v == "" {
				continue
			}
			if from != "" && v != value {
				named := "both hold a field named " + field
				if f != field {
					named = "hold fields named " + field + " and " + f
				}
				return "", "", false, refusef("Refused: secret=%s=%s is a field of two connections bound to this member's appSecrets slot, the %s (%s) and the %s (%s) connection, which %s with different values, so ask the person to rename the field in one of them in Team settings -> Connections, then run containerapp create again; nothing was changed.", env, name, ordinal(at+1), from, ordinal(i+1), k.Account, named)
			}
			if from == "" {
				field, value, from, at = f, v, k.Account, i
			}
		}
	}
	return field, value, from != "", nil
}

// ordinal is 1st, 2nd, 3rd, 4th, ...: a connection's place among the slot's bindings, which the Host
// hands over in binding order (appSecrets, appSecrets#2, ...), the order the member's settings show.
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// namedRefusal is the sentence for secret=NAME=<name> when no connection bound to appSecrets holds a
// field by that name: the agent asks the person to add it, never for its value.
func namedRefusal(env, name string) refusal {
	return refusef("Refused: secret=%s=%s names no app secret this member holds (secret= takes a Key Vault secret's address or the name of a field of a connection bound to this member's appSecrets slot, never a value), so ask the person to add an API-key connection (preset Other) with a field named %s in Team settings -> Connections, assign it to this team and bind it to the Azure member's appSecrets slot, never to send its value in a message, then run containerapp create again; nothing was changed.", env, name, name)
}

func init() {
	register(
		command{words: []string{"containerapp", "env", "create"}, usage: "<resource group> <environment>", run: (*plugin).envCreate},
		command{words: []string{"containerapp", "create"}, usage: "<resource group> <app> <environment> <registry>.azurecr.io/<image:tag> [port=<n>] [cpu=<0.25 to 2>] [env=<NAME>=<value>]... [secret=<NAME>=<Key Vault secret address>|<app secret name>]...", run: (*plugin).appCreate},
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
	if _, _, err := p.put(ws, map[string]any{
		"location": location, "tags": p.ownTags(),
		"properties": map[string]any{"sku": map[string]any{"name": "PerGB2018"}, "retentionInDays": 30},
	}, "the workspace "+name+"-logs"); err != nil {
		return "", err
	}
	p.progress("azure: creating environment " + name + " (a few minutes)")
	if _, _, err := p.put(env, map[string]any{
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
// vault and secret set), or from an appSecrets connection's field or an old named secret (field set
// to its name, value its value).
type appSecret struct {
	env, name, address, vault, secret string
	field, value                      string
}

func (p *plugin) appCreate(args []string) (string, error) {
	if len(args) < 4 {
		return "", refusal("Refused: containerapp create takes <resource group> <app> <environment> <registry>.azurecr.io/<image:tag>, then optionally port=<n>, cpu=<0.25 to 2> (its memory is twice that in GiB; 0.5 by default), env=<NAME>=<value> and secret=<NAME>=https://<vault>.vault.azure.net/secrets/<name> or secret=<NAME>=<the name of a field of a connection bound to this member's appSecrets slot>.")
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
		return "", refusef("Refused: %q is not an image in an Azure container registry, such as <registry>.azurecr.io/my-app:1.0. This member runs images only from registries its team owns and those in sharedContainerRegistries.", image)
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
					return "", refusef("Refused: %s looks like a secret. App secrets go in Key Vault or in a connection: pass it as secret=%s=https://<vault>.vault.azure.net/secrets/<name> for a Key Vault secret, set by a person or another plugin, or as secret=%s=<name> for the field of that name in an API-key connection a person adds in Team settings -> Connections and binds to this member's appSecrets slot.", v, v, v)
				}
				envs = append(envs, map[string]any{"name": v, "value": val})
				continue
			}
			sname := strings.ReplaceAll(strings.ToLower(v), "_", "-")
			if secretSeen[sname] {
				return "", refusef("Refused: %s would be the app's secret %s, which another secret= already names.", v, sname)
			}
			secretSeen[sname] = true
			field, value, ok, err := p.connectionSecret(v, val)
			if err != nil {
				return "", err
			}
			if !ok {
				field, value, ok = p.namedSecret(val)
			}
			if ok {
				secrets = append(secrets, appSecret{env: v, name: sname, field: field, value: value})
				continue
			}
			sm := secretRefForm.FindStringSubmatch(val)
			if sm == nil {
				if secretName.MatchString(val) && !p.isHidden(val) {
					return "", namedRefusal(v, val)
				}
				return "", refusef("Refused: secret=%s takes a Key Vault secret's address, https://<vault>.vault.azure.net/secrets/<name>, or the name of a field of a connection bound to this member's appSecrets slot; never a value.", v)
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
		return "", refusef("Refused: %s. This member runs images only from registries its team owns and those in sharedContainerRegistries. Nothing was changed.", why)
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

	// AN APP THAT EXISTS IS UPDATED, NOT REPLACED. The PUT below is the whole app, and Azure drops
	// whatever it leaves out, so what the command does not set is read from the app and kept: its
	// custom domains above all, which domain bind added and a PUT without them unbinds.
	var existing map[string]any
	appExists, err := p.az.get(appID+"?api-version="+versionOf("Microsoft.App/containerApps"), &existing)
	if err != nil {
		return "", err
	}
	var kept keptApp
	if appExists {
		if !p.ownedBy(stringMap(existing["tags"])) {
			return "", refusef("Refused: container app %s already exists in %s and does not carry this team's tags, so this member does not change it. Nothing was changed.", name, rg)
		}
		if kept, err = keepFrom(existing, name, secrets); err != nil {
			return "", err
		}
	}
	identityExists, err := p.az.get(identityID+"?api-version="+versionOf("Microsoft.ManagedIdentity/userAssignedIdentities"), nil)
	if err != nil {
		return "", err
	}

	server := registryHost(registry.Name)
	identityStep := "create managed identity " + name + "-id in " + rg
	if identityExists {
		identityStep = "use managed identity " + name + "-id (exists)"
	}
	steps := []string{identityStep, "grant it AcrPull on registry " + registry.Name}
	for _, v := range vaults {
		steps = append(steps, "grant it Key Vault Secrets User on Key Vault "+v.Name)
	}
	ingress := "no ingress"
	if port > 0 {
		ingress = fmt.Sprintf("public HTTPS ingress to port %d", port)
	} else if kept.ingress != nil {
		ingress = fmt.Sprintf("its public HTTPS ingress to port %v, kept", kept.ingress["targetPort"])
	}
	// THE APP KNOWS ITS IDENTITY. Azure's SDKs (DefaultAzureCredential, ManagedIdentityCredential)
	// read AZURE_CLIENT_ID to pick a user-assigned identity; without it an app asking for "the
	// managed identity" finds none. The command's own AZURE_CLIENT_ID is kept as given.
	ownClientID := seen[clientIDVar]
	settings := fmt.Sprintf("%d settings", len(envs))
	if !ownClientID {
		settings = fmt.Sprintf("%d settings and %s set to %s-id's client id", len(envs), clientIDVar, name)
	}
	appStep := "create container app " + name
	if appExists {
		appStep = "update container app " + name + " (exists)"
	}
	steps = append(steps, fmt.Sprintf("%s in environment %s running %s on %s, with %s, %s, %d Key Vault secrets and %d secrets from this member's connections", appStep, env.Name, image, appSize(cpu), ingress, settings, vaultSecrets, keySecrets))
	if appExists {
		steps = append(steps, kept.says())
	}
	what := strings.Join(steps, "; ")
	if !p.cfg.apply() {
		counts := map[string]int{"create": 1 + len(vaults)} // the role assignments
		for _, exists := range []bool{identityExists, appExists} {
			if exists {
				counts["modify"]++
			} else {
				counts["create"]++
			}
		}
		return p.whatif("containerapp create", appID, what, counts), nil
	}

	location := env.Location
	if location == "" {
		if location, err = p.rgLocation(rg); err != nil {
			return "", err
		}
	}
	ids := []string{}
	actions := map[string]string{} // each id's resource event: created, or changed when it existed
	did := func(id string, existed bool) {
		ids = append(ids, id)
		actions[id] = actionCreated
		if existed {
			actions[id] = actionChanged
		}
	}
	report := func(summary string) {
		p.appliedAs("containerapp create", appID, summary, ids, func(id string) string { return actions[id] })
	}
	done := func() { report("partly: " + what) }
	identity, existed, err := p.put(identityID, map[string]any{"location": location, "tags": p.ownTags()}, "the identity "+name+"-id")
	if err != nil {
		return "", err
	}
	did(identityID, existed)
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
		assignment, existed, err := p.grantRole(g.scope, g.role, principal)
		if err != nil {
			done()
			return "", fmt.Errorf("%s could not be granted on %s: %w", grantable[g.role], g.scope, err)
		}
		did(assignment, existed)
	}

	clientID, _ := props(identity)["clientId"].(string)
	clientNote := ""
	switch {
	case ownClientID:
		clientNote = fmt.Sprintf(" %s is the command's own, kept as given; Azure's SDKs sign in as the identity it names.", clientIDVar)
	case clientID != "":
		envs = append(envs, map[string]any{"name": clientIDVar, "value": clientID})
		clientNote = fmt.Sprintf(" %s is set to %s-id's client id %s, so Azure's SDKs (DefaultAzureCredential, ManagedIdentityCredential) sign in as that identity when the app asks for its managed identity.", clientIDVar, name, clientID)
	default:
		clientNote = fmt.Sprintf(" %s is not set: Azure gave no client id for %s-id yet; run containerapp create again in a minute to set it.", clientIDVar, name)
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
	appSecrets = append(appSecrets, kept.secrets...)
	config := map[string]any{
		"activeRevisionsMode": "Single",
		"registries":          []map[string]any{{"server": server, "identity": identityID}},
	}
	for k, v := range kept.config {
		config[k] = v
	}
	if len(appSecrets) > 0 {
		config["secrets"] = appSecrets
	}
	if port > 0 {
		config["ingress"] = map[string]any{"external": true, "targetPort": port, "transport": "auto", "allowInsecure": false}
	} else if kept.ingress != nil {
		config["ingress"] = map[string]any{}
	}
	if ing, ok := config["ingress"].(map[string]any); ok {
		for k, v := range kept.ingress {
			if _, set := ing[k]; !set || port == 0 {
				ing[k] = v
			}
		}
	}
	scale := map[string]any{}
	for k, v := range kept.scale {
		scale[k] = v
	}
	scale["minReplicas"], scale["maxReplicas"] = 0, 1
	identities := map[string]any{identityID: map[string]any{}}
	for _, id := range kept.identities {
		identities[id] = map[string]any{}
	}
	identityType := "UserAssigned"
	if kept.systemIdentity {
		identityType = "SystemAssigned,UserAssigned"
	}
	body := map[string]any{
		"location": location, "tags": p.ownTags(),
		"identity": map[string]any{"type": identityType, "userAssignedIdentities": identities},
		"properties": map[string]any{
			"managedEnvironmentId": env.ID,
			"configuration":        config,
			"template": map[string]any{
				"containers": []map[string]any{{"name": name, "image": image, "env": envs, "resources": map[string]any{"cpu": cpu, "memory": appMemory(cpu)}}},
				"scale":      scale,
			},
		},
	}
	if appExists {
		p.progress("azure: updating container app " + name + " (a few minutes)")
	} else {
		p.progress("azure: creating container app " + name + " (a few minutes)")
	}
	app, existed, err := p.put(appID, body, "the container app "+name)
	if err != nil {
		done()
		return "", fmt.Errorf("%w. A new identity's pull right can take a few minutes to reach the registry: run containerapp create again then", err)
	}
	did(appID, existed || appExists)
	answer := fmt.Sprintf("Container app %s runs %s in environment %s, pulling with its own identity %s-id.", name, image, env.Name, name)
	if appExists {
		answer = fmt.Sprintf("Container app %s existed and was updated: it runs %s in environment %s, pulling with its own identity %s-id.", name, image, env.Name, name)
	}
	if fqdn := appFQDN(app); fqdn != "" {
		answer += " It answers at https://" + fqdn + "."
	}
	summary := what
	if appExists {
		domains := kept.domainsAfter(app, name)
		answer += " " + domains
		summary += "; " + domains
	}
	report(summary)
	return answer + clientNote, nil
}

// keptIngress are the ingress settings containerapp create does not set itself, kept from an app
// that exists; keptConfig likewise for its configuration.
var (
	keptIngress = []string{"ipSecurityRestrictions", "stickySessions", "corsPolicy", "clientCertificateMode", "additionalPortMappings"}
	keptConfig  = []string{"dapr", "maxInactiveRevisions", "service"}
)

// keptApp is what containerapp create keeps of an app that exists: its custom domains, the ingress
// and configuration settings the command does not set, its scale settings beyond min and max
// replicas, the secrets those refer to and the identities those secrets are read with.
type keptApp struct {
	domains        []string       // the custom domains' host names, as the app had them
	ingress        map[string]any // customDomains and keptIngress; with no port= also external, targetPort, transport, allowInsecure
	config         map[string]any // keptConfig
	scale          map[string]any // everything but minReplicas and maxReplicas
	secrets        []map[string]any
	identities     []string // other user-assigned identities a kept secret is read with
	systemIdentity bool
	names          []string // the settings kept, in words
}

// keepFrom reads what to keep from an app as Azure answers it. A secret the command no longer names
// is kept only when a kept setting refers to it, and only a Key Vault one: Azure never answers a
// secret's value, so a kept setting reading a value secret is refused, naming the secret, until the
// command names it again.
func keepFrom(app map[string]any, name string, given []appSecret) (keptApp, error) {
	k := keptApp{config: map[string]any{}, scale: map[string]any{}}
	pr := props(app)
	cfg, _ := pr["configuration"].(map[string]any)
	ing, _ := cfg["ingress"].(map[string]any)
	if ing != nil {
		k.ingress = map[string]any{}
		var domains []map[string]any
		for _, d := range anyList(ing["customDomains"]) {
			host := str(d["name"])
			if host == "" {
				continue
			}
			kd := map[string]any{"name": host, "bindingType": str(d["bindingType"])}
			if c := str(d["certificateId"]); c != "" {
				kd["certificateId"] = c
			}
			domains = append(domains, kd)
			k.domains = append(k.domains, host)
		}
		if len(domains) > 0 {
			k.ingress["customDomains"] = domains
		}
		for _, f := range keptIngress {
			if v, ok := ing[f]; ok && v != nil {
				k.ingress[f] = v
				k.names = append(k.names, "ingress "+f)
			}
		}
		for _, f := range []string{"external", "targetPort", "transport", "allowInsecure"} {
			if v, ok := ing[f]; ok && v != nil {
				k.ingress[f] = v
			}
		}
	}
	for _, f := range keptConfig {
		if v, ok := cfg[f]; ok && v != nil {
			k.config[f] = v
			k.names = append(k.names, f)
		}
	}
	tpl, _ := pr["template"].(map[string]any)
	scale, _ := tpl["scale"].(map[string]any)
	for f, v := range scale {
		if f != "minReplicas" && f != "maxReplicas" && v != nil {
			k.scale[f] = v
		}
	}
	if len(anyList(scale["rules"])) > 0 {
		k.names = append(k.names, "scale rules")
	}

	// The secrets a kept setting refers to.
	refs := map[string]bool{}
	secretRefs(k.ingress, refs)
	secretRefs(k.config, refs)
	secretRefs(k.scale, refs)
	for _, s := range given {
		delete(refs, s.name)
	}
	for _, s := range anyList(cfg["secrets"]) {
		sname := str(s["name"])
		if !refs[sname] {
			continue
		}
		delete(refs, sname)
		url := str(s["keyVaultUrl"])
		if url == "" {
			return keptApp{}, refusef("Refused: container app %s has a setting that reads its secret %s, which holds a value Azure does not answer back, so a containerapp create that leaves it out would remove it. Name it again with secret=%s=<the name of a field of a connection bound to this member's appSecrets slot>. Nothing was changed.", name, sname, strings.ToUpper(strings.ReplaceAll(sname, "-", "_")))
		}
		ks := map[string]any{"name": sname, "keyVaultUrl": url}
		if id := str(s["identity"]); id != "" {
			ks["identity"] = id
			if !strings.EqualFold(id, "system") && !strings.HasSuffix(strings.ToLower(id), "/userassignedidentities/"+name+"-id") {
				k.identities = append(k.identities, id)
			}
			k.systemIdentity = k.systemIdentity || strings.EqualFold(id, "system")
		}
		k.secrets = append(k.secrets, ks)
	}
	for _, missing := range sortedSet(refs) {
		return keptApp{}, refusef("Refused: container app %s has a setting that reads a secret %s it does not hold. Nothing was changed.", name, missing)
	}
	if id, _ := app["identity"].(map[string]any); id != nil && strings.Contains(strings.ToLower(str(id["type"])), "systemassigned") {
		k.systemIdentity = true
	}
	return k, nil
}

// secretRefs gathers every secretRef (and passwordSecretRef) value under v.
func secretRefs(v any, into map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			if s, ok := val.(string); ok && (strings.EqualFold(key, "secretRef") || strings.EqualFold(key, "passwordSecretRef")) && s != "" {
				into[s] = true
				continue
			}
			secretRefs(val, into)
		}
	case []any:
		for _, e := range t {
			secretRefs(e, into)
		}
	case []map[string]any:
		for _, e := range t {
			secretRefs(e, into)
		}
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// says is the plan's words for what an existing app keeps.
func (k keptApp) says() string {
	s := "keeps its custom domains " + strings.Join(k.domains, ", ")
	if len(k.domains) == 0 {
		s = "it has no custom domains to keep"
	}
	if len(k.names) > 0 {
		s += ", and keeps its " + strings.Join(k.names, ", ")
	}
	return s
}

// domainsAfter says which of the app's custom domains are still bound in the app Azure answers
// after the PUT, and plainly which are not.
func (k keptApp) domainsAfter(app map[string]any, name string) string {
	cfg, _ := props(app)["configuration"].(map[string]any)
	ing, _ := cfg["ingress"].(map[string]any)
	bound := map[string]bool{}
	var now []string
	for _, d := range anyList(ing["customDomains"]) {
		if h := str(d["name"]); h != "" {
			bound[strings.ToLower(h)] = true
			now = append(now, h)
		}
	}
	var lost []string
	for _, h := range k.domains {
		if !bound[strings.ToLower(h)] {
			lost = append(lost, h)
		}
	}
	s := "Custom domains still bound: none."
	if len(now) > 0 {
		s = "Custom domains still bound: " + strings.Join(now, ", ") + "."
	}
	for _, h := range lost {
		s += fmt.Sprintf(" %s is no longer bound; run domain bind %s %s again.", h, name, h)
	}
	return s
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
	envID := strings.TrimSuffix(str(props(app)["managedEnvironmentId"]), "/")
	dest, err := p.logDestination(envID)
	if err != nil {
		return "", err
	}
	if dest.workspace == "" {
		return p.archivedLogs(r.Name, envID, dest.storage, minutes)
	}
	workspace := dest.workspace
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

// logDestination is where an environment's logs can be read: the Azure Resource Manager id of a
// Log Analytics workspace, or when there is none, of the storage account a diagnostic setting
// archives them to.
type logDestination struct {
	workspace string
	storage   string
}

// logDestination answers where an environment's logs go: for an environment that names the
// workspace's customerId (not a secret), the workspace in the subscription with that customerId;
// otherwise the workspace its diagnostic settings name, and failing that the storage account they
// name.
func (p *plugin) logDestination(envID string) (logDestination, error) {
	if envID == "" {
		return logDestination{}, refusal("Refused: the app names no environment.")
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
		return logDestination{}, err
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
			return logDestination{}, err
		}
		if id, ok := workspaceID(found); ok {
			return logDestination{workspace: id}, nil
		}
		return logDestination{}, refusef("Refused: the app's environment sends its logs to the Log Analytics workspace %s, which this sign-in cannot see in the subscription. Its owner gives this account Log Analytics Reader (or Reader) on that workspace.", cid)
	}
	var diags struct {
		Value []struct {
			Properties struct {
				WorkspaceID      string `json:"workspaceId"`
				StorageAccountID string `json:"storageAccountId"`
			} `json:"properties"`
		} `json:"value"`
	}
	if _, err := p.az.arm(http.MethodGet, envID+"/providers/Microsoft.Insights/diagnosticSettings?api-version="+diagAPIVersion, nil, &diags); err != nil {
		return logDestination{}, err
	}
	for _, d := range diags.Value {
		if id, ok := workspaceID(d.Properties.WorkspaceID); ok {
			return logDestination{workspace: id}, nil
		}
	}
	for _, d := range diags.Value {
		if id, ok := storageAccountID(d.Properties.StorageAccountID); ok {
			return logDestination{storage: id}, nil
		}
	}
	return logDestination{}, refusal("Refused: the app's environment sends its logs neither to a Log Analytics workspace nor, through a diagnostic setting, to a storage account, so there is nothing to read them from. Create environments with containerapp env create, whose logs go to a workspace, or give the environment a diagnostic setting that archives its logs to a storage account.")
}

// storageAccountID answers id, without a trailing slash, when it is the Azure Resource Manager id
// of a storage account.
func storageAccountID(id string) (string, bool) {
	id = strings.TrimSuffix(id, "/")
	r, ok := parseID(id)
	if !ok || !strings.HasPrefix(strings.ToLower(id), "/subscriptions/") || !strings.EqualFold(r.typ(), "Microsoft.Storage/storageAccounts") {
		return "", false
	}
	return id, true
}

// logArchives are the containers a diagnostic setting archives an environment's logs to, one per
// category, and how a line of each is marked: the apps' console lines as they are, the platform's
// own lines about an app (a revision started, a container restarted) as system.
var logArchives = []struct{ container, mark string }{
	{"insights-logs-containerappconsolelogs", ""},
	{"insights-logs-containerappsystemlogs", "system: "},
}

// archivedLogs reads an app's log lines of the last minutes from the storage account its
// environment's diagnostic setting archives them to: one blob of JSON lines per category and hour,
// resourceId=<ENVIRONMENT ID IN CAPITALS>/y=YYYY/m=MM/d=DD/h=HH/m=00/PT1H.json, holding every app
// of the environment. It reads as the signed-in person, as blob get does.
func (p *plugin) archivedLogs(app, envID, storageID string, minutes int) (string, error) {
	account := storageID[strings.LastIndex(storageID, "/")+1:]
	now := p.now().UTC()
	since := now.Add(-time.Duration(minutes) * time.Minute)
	type entry struct {
		at   time.Time
		text string
	}
	var entries []entry
	for _, archive := range logArchives {
		for hour := since.Truncate(time.Hour); !hour.After(now); hour = hour.Add(time.Hour) {
			blob := fmt.Sprintf("resourceId=%s/y=%04d/m=%02d/d=%02d/h=%02d/m=00/PT1H.json", strings.ToUpper(envID), hour.Year(), int(hour.Month()), hour.Day(), hour.Hour())
			p.progress(fmt.Sprintf("azure: reading %s/%s for %s", account, archive.container, hour.Format("2006-01-02 15:00")))
			ans, err := p.az.send(apiStorage, http.MethodGet, blobURL(account, archive.container, blob), nil, map[string]string{"x-ms-version": blobVersion})
			if isStatus(err, http.StatusNotFound) {
				continue // nothing was logged that hour, or nothing of this category at all
			}
			if isStatus(err, http.StatusForbidden) {
				return "", refusef("Refused: the signed-in account cannot read the logs of %s, which its environment archives to storage account %s. It needs Storage Blob Data Reader (or more) on that account; its owner gives it in the Azure portal (the storage account, Access control (IAM), Add role assignment).", app, account)
			}
			if err != nil {
				return "", err
			}
			for _, rec := range archivedRecords(ans.body) {
				if !strings.EqualFold(rec.appName, app) {
					continue
				}
				at := rec.at
				if at.IsZero() {
					at = hour // a record whose time cannot be read is placed at its hour
				} else if at.Before(since) || at.After(now) {
					continue
				}
				entries = append(entries, entry{at, rec.time + "  " + archive.mark + strings.TrimRight(rec.text, "\r\n")})
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if len(entries) > maxLogLines {
		entries = entries[len(entries)-maxLogLines:] // the newest, as the workspace query keeps them
	}
	if len(entries) == 0 {
		return fmt.Sprintf("Container app %s wrote no log lines in the last %d minutes (they reach storage account %s a few minutes late).", app, minutes, account), nil
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.text
	}
	return fmt.Sprintf("Logs of %s, the last %d minutes, from storage account %s (%d lines, at most %d, oldest first):\n%s", app, minutes, account, len(lines), maxLogLines, strings.Join(lines, "\n")), nil
}

// archivedRecord is one log record a diagnostic setting archived: when, which app and what it said.
type archivedRecord struct {
	time    string
	at      time.Time
	appName string
	text    string
}

// archivedRecords reads an archive blob: JSON records one after another (JSON lines), or an object
// holding them in records. A record's fields sit in properties (an object, or an object written as
// a string), its time beside them; names are matched in any case, and a console line's Log as well
// as a system line's Msg is its text. What does not parse is passed over.
func archivedRecords(raw []byte) []archivedRecord {
	var out []archivedRecord
	add := func(rec map[string]any) {
		fields := map[string]any{}
		for k, v := range rec {
			fields[strings.ToLower(k)] = v
		}
		inner := fields["properties"]
		if text, ok := inner.(string); ok {
			var m map[string]any
			if json.Unmarshal([]byte(text), &m) == nil {
				inner = m
			}
		}
		if m, ok := inner.(map[string]any); ok {
			for k, v := range m {
				fields[strings.ToLower(k)] = v
			}
		}
		first := func(names ...string) string {
			for _, n := range names {
				if v := str(fields[n]); v != "" {
					return v
				}
			}
			return ""
		}
		r := archivedRecord{
			time:    first("time", "timegenerated", "timestamp"),
			appName: first("containerappname", "containerappname_s"),
			text:    first("log", "log_s", "msg", "message"),
		}
		if t, err := time.Parse(time.RFC3339Nano, r.time); err == nil {
			r.at = t.UTC()
		}
		if r.appName != "" {
			out = append(out, r)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			return out
		}
		if list, ok := rec["records"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					add(m)
				}
			}
			continue
		}
		add(rec)
	}
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
