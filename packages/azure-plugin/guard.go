package main

import (
	"fmt"
	"net/http"
	"strings"
)

// The tags on everything the plugin creates. A resource group carrying them is owned whole.
// pluginTag stays "azure" though the plugin's id is azure-plugin: what earlier versions created
// carries it, and stays owned.
const (
	tagOwner  = "yawble-owner"
	tagPlugin = "yawble-plugin"
	pluginTag = "azure"
)

// rid is an Azure resource id, read into its parts.
type rid struct {
	raw   string
	sub   string
	rg    string   // "" for a subscription-level id
	ns    string   // the provider namespace of the first resource, such as Microsoft.Storage
	types []string // its type and each child's: storageAccounts, blobServices
	names []string // its name and each child's
	scope string   // for an extension resource (.../providers/Microsoft.Authorization/roleAssignments/x): the id it sits on
	ext   *rid     // the extension resource's own type and name, when scope is set
}

// parseID reads an id such as /subscriptions/<s>/resourceGroups/<rg>/providers/<ns>/<type>/<name>.
func parseID(id string) (rid, bool) {
	r := rid{raw: strings.TrimSuffix(id, "/")}
	parts := strings.Split(strings.TrimPrefix(r.raw, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "subscriptions") || parts[1] == "" {
		return r, false
	}
	r.sub = strings.ToLower(parts[1])
	rest := parts[2:]
	if len(rest) >= 2 && strings.EqualFold(rest[0], "resourceGroups") {
		r.rg = rest[1]
		rest = rest[2:]
	}
	if len(rest) == 0 {
		return r, r.rg != "" || len(parts) == 2
	}
	// An extension resource: the last providers segment past the first resource.
	for i := len(rest) - 1; i > 0; i-- {
		if strings.EqualFold(rest[i], "providers") {
			scope := "/" + strings.Join(parts[:len(parts)-len(rest)+i], "/")
			ext := rid{}
			if !readProvider(&ext, rest[i:]) {
				return r, false
			}
			inner, ok := parseID(scope)
			if !ok {
				return r, false
			}
			inner.raw, inner.scope, inner.ext = r.raw, scope, &ext
			return inner, true
		}
	}
	if !readProvider(&r, rest) {
		return r, false
	}
	// A role assignment or lock on a resource group sits on the group.
	if r.rg != "" && strings.EqualFold(r.ns, "Microsoft.Authorization") {
		ext := rid{ns: r.ns, types: r.types, names: r.names}
		group := rid{raw: r.raw, sub: r.sub, rg: r.rg, scope: rgPath(r.sub, r.rg), ext: &ext}
		return group, true
	}
	return r, true
}

// readProvider reads providers/<ns>/<type>/<name>[/<type>/<name>...].
func readProvider(r *rid, rest []string) bool {
	if len(rest) < 4 || !strings.EqualFold(rest[0], "providers") || len(rest[2:])%2 != 0 {
		return false
	}
	r.ns = rest[1]
	for i := 2; i < len(rest); i += 2 {
		if rest[i] == "" || rest[i+1] == "" {
			return false
		}
		r.types = append(r.types, rest[i])
		r.names = append(r.names, rest[i+1])
	}
	return true
}

// typ is the resource's full type: Microsoft.Sql/servers/databases.
func (r rid) typ() string {
	if r.ns == "" {
		if r.rg != "" {
			return "Microsoft.Resources/resourceGroups"
		}
		return "Microsoft.Resources/subscriptions"
	}
	return r.ns + "/" + strings.Join(r.types, "/")
}

func (r rid) isType(t string) bool { return strings.EqualFold(r.typ(), t) }

// name is the last name in the id.
func (r rid) name() string {
	if len(r.names) > 0 {
		return r.names[len(r.names)-1]
	}
	return r.rg
}

func rgPath(sub, rg string) string { return "/subscriptions/" + sub + "/resourceGroups/" + rg }

// rgID is the id of the resource group the resource is in.
func (r rid) rgID() string { return rgPath(r.sub, r.rg) }

// topID is the id of the first-level resource: the server of a database.
func (r rid) topID() string {
	return r.rgID() + "/providers/" + r.ns + "/" + r.types[0] + "/" + r.names[0]
}

// parentID is the id one level up: the server of a database, the group of a first-level resource.
func (r rid) parentID() string {
	if len(r.types) <= 1 {
		return r.rgID()
	}
	id := r.rgID() + "/providers/" + r.ns
	for i := 0; i < len(r.types)-1; i++ {
		id += "/" + r.types[i] + "/" + r.names[i]
	}
	return id
}

// ownTags are the tags the plugin puts on everything it creates.
func (p *plugin) ownTags() map[string]string {
	return map[string]string{tagOwner: p.team, tagPlugin: pluginTag}
}

// ownedBy reports whether tags carry this team's mark. Tag names are compared in any case, as Azure
// does; the values exactly.
func (p *plugin) ownedBy(tags map[string]string) bool {
	if p.team == "" {
		return false
	}
	var owner, plug bool
	for k, v := range tags {
		switch {
		case strings.EqualFold(k, tagOwner):
			owner = v == p.team
		case strings.EqualFold(k, tagPlugin):
			plug = v == pluginTag
		}
	}
	return owner && plug
}

// tagsOf reads a resource's or a resource group's tags; nil when it does not exist.
func (p *plugin) tagsOf(id string) (map[string]string, error) {
	key := strings.ToLower(id)
	if t, ok := p.tags[key]; ok {
		return t, nil
	}
	r, ok := parseID(id)
	if !ok {
		return nil, refusef("Refused: %q is not an Azure resource id.", id)
	}
	var body struct {
		Tags       map[string]string `json:"tags"`
		Properties struct {
			Tags map[string]string `json:"tags"`
		} `json:"properties"`
	}
	var found bool
	var err error
	if r.ns == "" && r.scope == "" {
		found, err = p.az.get(rgPath(r.sub, r.rg)+"?api-version="+rgAPIVersion, &body)
	} else {
		found, err = p.az.get(id+"/providers/Microsoft.Resources/tags/default?api-version="+rgAPIVersion, &body)
		body.Tags = body.Properties.Tags
	}
	if isStatus(err, http.StatusBadRequest) || isStatus(err, http.StatusMethodNotAllowed) {
		// A type that takes no tags, such as a DNS record set: it exists, and carries none.
		p.tags[key] = map[string]string{}
		return p.tags[key], nil
	}
	if err != nil {
		return nil, err
	}
	if !found {
		p.tags[key] = nil
		return nil, nil
	}
	if body.Tags == nil {
		body.Tags = map[string]string{}
	}
	p.tags[key] = body.Tags
	return body.Tags, nil
}

// forget drops what the run has read of a resource's tags, after it changed them.
func (p *plugin) forget(id string) { delete(p.tags, strings.ToLower(id)) }

// change is one change the plugin is asked to make, for the guard to judge.
type change struct {
	id   string
	kind changeKind
	tags map[string]string // the tags a created resource will carry
	role string            // for a grant: the role definition id's last part
}

type changeKind int

const (
	changeCreate changeKind = iota
	changeModify
	changeDelete
	changeGrant // a role assignment, id being the scope it is on
	changePush  // an image pushed to a registry
)

// guard is the ownership rule, the one check every change goes through. It answers "" when the
// change is allowed, or the reason it is not, naming the resource:
//   - only inside the configured subscription;
//   - a resource carrying this team's tags, or inside a resource group that does;
//   - a new resource only inside a resource group that does, except a database created, tagged,
//     on a server in sharedSqlServers;
//   - a push to, or an AcrPull grant on, a registry in sharedContainerRegistries.
//
// Reads never come here: the plugin reads anything the person can.
func (p *plugin) guard(c change) (string, error) {
	r, ok := parseID(c.id)
	if !ok {
		return fmt.Sprintf("%s is not an Azure resource id", c.id), nil
	}
	if r.scope != "" && c.kind != changeGrant {
		// An extension resource (a role assignment, a lock) is judged by what it sits on.
		if r.ext != nil && strings.EqualFold(r.ext.ns+"/"+strings.Join(r.ext.types, "/"), "Microsoft.Authorization/roleAssignments") {
			return p.guard(change{id: r.scope, kind: changeGrant, role: c.role})
		}
		return p.guard(change{id: r.scope, kind: changeModify})
	}
	if p.cfg.Subscription == "" || r.sub != p.cfg.Subscription {
		return fmt.Sprintf("%s is outside the subscription in this member's settings", c.id), nil
	}
	if r.rg == "" {
		return fmt.Sprintf("%s is not inside a resource group", c.id), nil
	}
	if c.kind == changeDelete && !p.cfg.AllowDelete {
		return fmt.Sprintf("%s would be deleted, and allowDelete is off", c.id), nil
	}

	// The two shared exceptions.
	if (c.kind == changePush || (c.kind == changeGrant && c.role == roleAcrPull)) &&
		r.isType("Microsoft.ContainerRegistry/registries") && p.cfg.sharedRegistry(r.name()) {
		return "", nil
	}
	if c.kind == changeCreate && r.isType("Microsoft.Sql/servers/databases") && p.cfg.sharedSQL(r.names[0]) {
		if p.ownedBy(c.tags) {
			return "", nil
		}
		return fmt.Sprintf("%s is a new database on shared server %s without this team's tags", c.id, r.names[0]), nil
	}

	group, err := p.tagsOf(r.rgID())
	if err != nil {
		return "", err
	}
	if p.ownedBy(group) {
		return "", nil
	}
	if r.ns == "" {
		if group == nil && c.kind == changeCreate {
			return "", nil // a new resource group, created with the tags
		}
		return fmt.Sprintf("resource group %s does not carry this team's tags", r.rg), nil
	}
	if c.kind == changeCreate {
		return fmt.Sprintf("%s would be created in resource group %s, which does not carry this team's tags", c.id, r.rg), nil
	}
	// The resource itself, then each parent up to the first level: a database on a shared server
	// carries its own tags.
	for id := c.id; ; {
		t, err := p.tagsOf(id)
		if err != nil {
			return "", err
		}
		if p.ownedBy(t) {
			return "", nil
		}
		ir, _ := parseID(id)
		if len(ir.types) <= 1 {
			break
		}
		id = ir.parentID()
	}
	return fmt.Sprintf("%s does not carry this team's tags, nor does its resource group %s", c.id, r.rg), nil
}

// mustOwn refuses a command whose change the guard does not allow.
func (p *plugin) mustOwn(c change) error {
	why, err := p.guard(c)
	if err != nil {
		return err
	}
	if why != "" {
		return refusef("Refused: %s. This member changes only what its team owns. Nothing was changed.", why)
	}
	return nil
}

// needSubscription refuses a command that needs the subscription before it is set.
func (p *plugin) needSubscription() error {
	if p.cfg.Subscription == "" {
		return refusal("Refused: no subscription is set. A person runs setup to see the subscriptions this sign-in can read, then puts one in this member's subscription setting.")
	}
	if !p.az.has(apiARM) {
		return missingToken(apiARM)
	}
	return nil
}

// ownType is the type of the resource the id names: for an extension resource such as
// .../managedEnvironments/<env>/providers/Microsoft.Insights/diagnosticSettings/<name>, the
// extension's own type, never the type it sits on.
func (r rid) ownType() string {
	if r.ext != nil {
		return r.ext.ns + "/" + strings.Join(r.ext.types, "/")
	}
	return r.typ()
}

// apiVersionFor answers the api-version of the resource an id names, by the last resource type in
// it: the version the plugin knows for that type, or else the newest stable one Azure lists for it.
// A type with neither is refused, naming it; no version is guessed.
func (p *plugin) apiVersionFor(r rid) (string, error) {
	typ := r.ownType()
	if v, ok := knownVersion(typ); ok {
		return v, nil
	}
	ns, want := r.ns, strings.Join(r.types, "/")
	if r.ext != nil {
		ns, want = r.ext.ns, strings.Join(r.ext.types, "/")
	}
	var prov struct {
		ResourceTypes []struct {
			ResourceType string   `json:"resourceType"`
			APIVersions  []string `json:"apiVersions"`
		} `json:"resourceTypes"`
	}
	found, err := p.az.get("/subscriptions/"+r.sub+"/providers/"+ns+"?api-version="+rgAPIVersion, &prov)
	if err != nil {
		return "", err
	}
	if found {
		for _, t := range prov.ResourceTypes {
			if !strings.EqualFold(t.ResourceType, want) {
				continue
			}
			for _, v := range t.APIVersions { // newest first
				if !strings.Contains(v, "preview") {
					return v, nil
				}
			}
			if len(t.APIVersions) > 0 {
				return t.APIVersions[0], nil
			}
		}
	}
	return "", refusef("Refused: this member knows no api-version for %s, and Azure lists none for it in this subscription, so it does not read or change %s with a guessed one.", typ, r.raw)
}

const rgAPIVersion = "2021-04-01"

// knownVersions are the api-versions of the types the plugin itself creates or names, each one Azure
// supports for that type.
var knownVersions = map[string]string{
	"Microsoft.Resources/resourceGroups":                        rgAPIVersion,
	"Microsoft.Resources/subscriptions":                         subAPIVersion,
	"Microsoft.Resources/deployments":                           deployAPIVersion,
	"Microsoft.Network/dnsZones/A":                              dnsAPIVersion,
	"Microsoft.Network/dnsZones/AAAA":                           dnsAPIVersion,
	"Microsoft.Network/dnsZones/CNAME":                          dnsAPIVersion,
	"Microsoft.Network/dnsZones/TXT":                            dnsAPIVersion,
	"Microsoft.Network/dnsZones/MX":                             dnsAPIVersion,
	"Microsoft.Sql/servers/firewallRules":                       sqlAPIVersion,
	"Microsoft.Storage/storageAccounts/blobServices":            "2023-05-01",
	"Microsoft.Storage/storageAccounts/blobServices/containers": "2023-05-01",
	"Microsoft.Storage/storageAccounts/tableServices":           "2023-05-01",
	"Microsoft.Storage/storageAccounts/tableServices/tables":    "2023-05-01",
	"Microsoft.Storage/storageAccounts/queueServices":           "2023-05-01",
	"Microsoft.Storage/storageAccounts/queueServices/queues":    "2023-05-01",
	"Microsoft.KeyVault/vaults/secrets":                         "2023-07-01",
	"Microsoft.Insights/diagnosticSettings":                     diagAPIVersion,
	"Microsoft.Consumption/budgets":                             budgetAPIVersion,
	"Microsoft.Network/dnsZones":                                dnsAPIVersion,
	"Microsoft.ManagedIdentity/userAssignedIdentities":          "2023-01-31",
	"Microsoft.Authorization/roleAssignments":                   roleAPIVersion,
	"Microsoft.ContainerRegistry/registries":                    acrAPIVersion,
	"Microsoft.Sql/servers":                                     sqlAPIVersion,
	"Microsoft.Sql/servers/databases":                           sqlAPIVersion,
	"Microsoft.Storage/storageAccounts":                         "2023-05-01",
	"Microsoft.KeyVault/vaults":                                 "2023-07-01",
	"Microsoft.OperationalInsights/workspaces":                  "2023-09-01",
	"Microsoft.App/containerApps":                               "2024-03-01",
	"Microsoft.App/managedEnvironments":                         "2024-03-01",
	"Microsoft.App/managedEnvironments/certificates":            "2024-03-01",
	"Microsoft.App/managedEnvironments/managedCertificates":     "2024-03-01",
}

// knownVersion answers the api-version of a type the plugin creates, in any case.
func knownVersion(typ string) (string, bool) {
	for t, v := range knownVersions {
		if strings.EqualFold(t, typ) {
			return v, true
		}
	}
	return "", false
}

// isNotFound reports whether err is Azure's 404.
func isNotFound(err error) bool { return isStatus(err, http.StatusNotFound) }
