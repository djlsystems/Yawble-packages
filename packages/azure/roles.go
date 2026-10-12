package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

const roleAPIVersion = "2022-04-01"

// The built-in roles the plugin may grant, by Azure's own role definition id. Built-in role ids are
// the same in every tenant.
const (
	roleContributor          = "b24988ac-6180-42a0-ab88-20f7382dd24c"
	roleBlobDataContributor  = "ba92f5b4-2d11-453d-a403-e96b0029c9fe"
	roleTableDataContributor = "0a9a7e1f-b9d0-4cc4-a60d-0319b160aaa3"
	roleQueueDataContributor = "974c5e8b-45b9-4653-ba55-5f855dd0fb88"
	roleKeyVaultSecretsOff   = "b86a8fe4-44ce-4948-aee5-eccb2c155cd7"
	roleKeyVaultSecretsUser  = "4633458b-17de-408a-b874-0445c86b69e6"
	roleAcrPull              = "7f951dda-4ed3-4680-a7ca-43fe172d538d"
)

var grantable = map[string]string{
	roleContributor:          "Contributor",
	roleBlobDataContributor:  "Storage Blob Data Contributor",
	roleTableDataContributor: "Storage Table Data Contributor",
	roleQueueDataContributor: "Storage Queue Data Contributor",
	roleKeyVaultSecretsOff:   "Key Vault Secrets Officer",
	roleKeyVaultSecretsUser:  "Key Vault Secrets User",
	roleAcrPull:              "AcrPull",
}

// roleByName finds a grantable role by its name, in any case and spacing.
func roleByName(name string) (string, bool) {
	squeeze := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, " ", "")) }
	for id, n := range grantable {
		if squeeze(n) == squeeze(name) {
			return id, true
		}
	}
	return "", false
}

func grantableNames() string {
	var names []string
	for _, n := range grantable {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

var roleDefinitionTail = regexp.MustCompile(`(?i)roleDefinitions/([0-9a-f-]{36})$`)

// roleOf reads the role definition id's last part, lower case.
func roleOf(definitionID string) string {
	if m := roleDefinitionTail.FindStringSubmatch(strings.TrimSpace(definitionID)); m != nil {
		return strings.ToLower(m[1])
	}
	return strings.ToLower(strings.TrimSpace(definitionID))
}

var guidAnywhere = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// identityPrincipal answers the principal of a managed identity the team owns, given as its
// resource id: a user-assigned identity, or the system-assigned identity of a resource. A user, a
// group or any other principal is refused.
func (p *plugin) identityPrincipal(id string) (string, error) {
	switch {
	case strings.Contains(id, "@"):
		return "", refusef("Refused: %s is a user. This member grants roles only to managed identities it created, never to a user or a group.", id)
	case guidAnywhere.MatchString(id):
		return "", refusef("Refused: %s is a bare principal id, which could be a user or a group. Name the managed identity by its resource id (/subscriptions/.../providers/Microsoft.ManagedIdentity/userAssignedIdentities/<name>).", id)
	}
	r, ok := parseID(id)
	if !ok || r.ns == "" || r.scope != "" {
		return "", refusef("Refused: %q is not a managed identity's resource id. This member grants roles only to managed identities it created, never to a user or a group.", id)
	}
	if why, err := p.guard(change{id: id, kind: changeModify}); err != nil {
		return "", err
	} else if why != "" {
		return "", refusef("Refused: %s; this member grants roles only to managed identities it created.", why)
	}
	version, err := p.apiVersionFor(r)
	if err != nil {
		return "", err
	}
	var body struct {
		Identity struct {
			PrincipalID string `json:"principalId"`
		} `json:"identity"`
		Properties struct {
			PrincipalID string `json:"principalId"`
		} `json:"properties"`
	}
	found, err := p.az.get(id+"?api-version="+version, &body)
	if err != nil {
		return "", err
	}
	if !found {
		return "", refusef("Refused: %s does not exist.", id)
	}
	if r.isType("Microsoft.ManagedIdentity/userAssignedIdentities") && body.Properties.PrincipalID != "" {
		return body.Properties.PrincipalID, nil
	}
	if body.Identity.PrincipalID != "" {
		return body.Identity.PrincipalID, nil
	}
	return "", refusef("Refused: %s has no managed identity. Give it a system-assigned identity, or name a user-assigned identity this team created.", id)
}

// assignmentName is the role assignment's name: the same scope, role and principal always make the
// same name, so granting twice changes nothing.
func assignmentName(scope, role, principal string) string {
	sum := sha1.Sum([]byte(strings.ToLower(scope + "|" + role + "|" + principal)))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func init() {
	register(
		command{words: []string{"role", "assign"}, usage: "<role> <managed identity id> <scope id>", run: (*plugin).roleAssign},
		command{words: []string{"role", "list"}, usage: "<scope id>", run: (*plugin).roleList},
	)
}

func (p *plugin) roleAssign(args []string) (string, error) {
	if len(args) < 3 {
		return "", refusef("Refused: role assign takes <role> <managed identity id> <scope id>; the roles are %s.", grantableNames())
	}
	// A role name may have spaces without quotes: the last two words are the identity and the scope.
	name := strings.Join(args[:len(args)-2], " ")
	identity, scope := args[len(args)-2], strings.TrimSuffix(args[len(args)-1], "/")
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	role, ok := roleByName(name)
	if !ok {
		return "", refusef("Refused: %s is not a role this member grants. It grants only %s.", name, grantableNames())
	}
	if why, err := p.guard(change{id: scope, kind: changeGrant, role: role}); err != nil {
		return "", err
	} else if why != "" {
		return "", refusef("Refused: %s. This member grants roles only on what its team owns, and AcrPull on a registry in sharedRegistries. Nothing was changed.", why)
	}
	principal, err := p.identityPrincipal(identity)
	if err != nil {
		return "", err
	}
	assignment := scope + "/providers/Microsoft.Authorization/roleAssignments/" + assignmentName(scope, role, principal)
	what := fmt.Sprintf("grant %s to %s on %s", grantable[role], identity, scope)
	if !p.cfg.apply() {
		return p.whatif("role assign", scope, what, map[string]int{"create": 1}), nil
	}
	if _, already, err := p.grantRole(scope, role, principal); err != nil {
		return "", err
	} else if already {
		return fmt.Sprintf("%s already holds %s on %s. Nothing was changed.", identity, grantable[role], scope), nil
	}
	p.applied("role assign", scope, what, []string{assignment})
	return fmt.Sprintf("Granted %s to %s on %s.", grantable[role], identity, scope), nil
}

// grantRole assigns a role to a managed identity's principal on a scope the guard has allowed, and
// answers the assignment's id, and whether the identity held the role already.
func (p *plugin) grantRole(scope, role, principal string) (string, bool, error) {
	r, _ := parseID(scope)
	assignment := scope + "/providers/Microsoft.Authorization/roleAssignments/" + assignmentName(scope, role, principal)
	body := map[string]any{"properties": map[string]any{
		"roleDefinitionId": "/subscriptions/" + r.sub + "/providers/Microsoft.Authorization/roleDefinitions/" + role,
		"principalId":      principal,
		"principalType":    "ServicePrincipal",
	}}
	_, err := p.az.arm(http.MethodPut, assignment+"?api-version="+roleAPIVersion, body, nil)
	if isStatus(err, http.StatusConflict) {
		return assignment, true, nil
	}
	return assignment, false, err
}

func (p *plugin) roleList(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "role list", "<scope id>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	scope := strings.TrimSuffix(args[0], "/")
	var lines []string
	err := p.az.list(scope+"/providers/Microsoft.Authorization/roleAssignments?$filter=atScope()&api-version="+roleAPIVersion, func(raw json.RawMessage) {
		var a struct {
			Properties struct {
				RoleDefinitionID string `json:"roleDefinitionId"`
				PrincipalID      string `json:"principalId"`
				PrincipalType    string `json:"principalType"`
				Scope            string `json:"scope"`
			} `json:"properties"`
		}
		if json.Unmarshal(raw, &a) != nil {
			return
		}
		role := roleOf(a.Properties.RoleDefinitionID)
		if n, ok := grantable[role]; ok {
			role = n
		}
		lines = append(lines, fmt.Sprintf("- %s: %s %s on %s", role, a.Properties.PrincipalType, a.Properties.PrincipalID, a.Properties.Scope))
	})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No role assignments at " + scope + ".", nil
	}
	sort.Strings(lines)
	return fmt.Sprintf("Role assignments at %s (%d):\n%s", scope, len(lines), strings.Join(lines, "\n")), nil
}
