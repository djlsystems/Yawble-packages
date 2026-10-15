package main

import "strings"

// maxResourceEvents caps the resource events of one command; past it, the applied summary alone
// covers the rest.
const maxResourceEvents = 25

// The actions a resource event names.
const (
	actionCreated = "created"
	actionChanged = "changed"
	actionDeleted = "deleted"
)

// commandActions is what each changing command does to the Azure resources it names. A command not
// listed writes data inside a resource (a blob, an entity, a message, SQL, an image) or restarts
// one, and publishes no resource event: its applied summary says what it did.
var commandActions = map[string]string{
	"rg create":                actionCreated,
	"acr create":               actionCreated,
	"containerapp env create":  actionCreated,
	"containerapp create":      actionCreated,
	"dns zone create":          actionCreated,
	"dns record set":           actionChanged,
	"dns record delete":        actionDeleted,
	"domain bind":              actionChanged, // its managed certificate is created; see actionOf
	"keyvault create":          actionCreated,
	"role assign":              actionCreated,
	"sql server create":        actionCreated,
	"sql db create":            actionCreated,
	"storage create":           actionCreated,
	"storage container create": actionCreated,
	"storage queue create":     actionCreated,
	"storage table create":     actionCreated,
	"delete":                   actionDeleted,
}

// actionOf answers what command did to the resource id, or "" when it publishes no resource event.
func actionOf(command, id string) string {
	if command == "domain bind" {
		if r, ok := parseID(id); ok && r.isType("Microsoft.App/managedEnvironments/managedCertificates") {
			return actionCreated
		}
	}
	return commandActions[command]
}

// resourceKinds names the Azure types the plugin knows by a short kind.
var resourceKinds = map[string]string{
	"microsoft.resources/resourcegroups":               "resource-group",
	"microsoft.sql/servers":                            "sql-server",
	"microsoft.sql/servers/databases":                  "sql-database",
	"microsoft.app/containerapps":                      "container-app",
	"microsoft.app/managedenvironments":                "container-app-environment",
	"microsoft.containerregistry/registries":           "container-registry",
	"microsoft.keyvault/vaults":                        "key-vault",
	"microsoft.keyvault/vaults/secrets":                "secret",
	"microsoft.storage/storageaccounts":                "storage-account",
	"microsoft.managedidentity/userassignedidentities": "managed-identity",
	"microsoft.authorization/roleassignments":          "role-assignment",
	"microsoft.operationalinsights/workspaces":         "log-analytics",
	"microsoft.consumption/budgets":                    "budget",
	"microsoft.network/dnszones":                       "dns-zone",
}

// resourceKind reads a resource's short kind and its own name from its id alone: an extension
// resource, such as a role assignment, by its own type and name; a type the plugin does not name is
// other, named by the id's last segment.
func resourceKind(id string) (kind, name string) {
	trimmed := strings.TrimSuffix(id, "/")
	name = trimmed[strings.LastIndex(trimmed, "/")+1:]
	r, ok := parseID(id)
	if !ok {
		return "other", name
	}
	t := r.typ()
	if r.ext != nil {
		t = r.ext.ns + "/" + strings.Join(r.ext.types, "/")
	}
	if k, known := resourceKinds[strings.ToLower(t)]; known {
		return k, name
	}
	return "other", name
}

// resourceEvents publishes plugin.azure.resource for each id an applied command touched, up to
// maxResourceEvents. action answers each id's action; "" publishes nothing for it. A payload
// carries the id, its kind and name, the action and the command: never a value or a body.
func (p *plugin) resourceEvents(command string, ids []string, action func(id string) string) {
	n := 0
	for _, id := range ids {
		if n == maxResourceEvents {
			return
		}
		a := action(id)
		if a == "" || id == "" {
			continue
		}
		kind, name := resourceKind(id)
		p.publish("resource", map[string]any{"resourceId": id, "resourceType": kind, "name": name, "action": a, "command": command})
		n++
	}
}
