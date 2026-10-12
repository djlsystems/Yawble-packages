package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const deployAPIVersion = "2021-04-01"

func init() {
	register(command{words: []string{"deploy"}, usage: "<resource group> <template .bicep or .json> [parameters .json]", run: (*plugin).deploy})
}

// compileBicep turns a Bicep file into ARM JSON with the pinned bicep compiler. Only tests change it.
var compileBicep = func(p *plugin, path string) ([]byte, error) {
	bin, err := p.bicep()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "build", path, "--stdout")
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1", "DOTNET_CLI_TELEMETRY_OPTOUT=1",
		"DOTNET_BUNDLE_EXTRACT_BASE_DIR="+filepath.Join(filepath.Dir(bin), "extract"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[:2000] + "..."
		}
		return nil, fmt.Errorf("bicep could not compile %s:\n%s", filepath.Base(path), msg)
	}
	return stdout.Bytes(), nil
}

// readAllowed reads a file the team may deploy from: inside the team's documents folder or a
// worktree of this run, after links are followed. Any other path is refused.
func (p *plugin) readAllowed(arg string) (string, []byte, error) {
	real, err := p.allowedPath(arg, "deploys only templates from those")
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, refusef("Refused: %s is not a file.", arg)
	}
	if info.Size() > 4<<20 {
		return "", nil, refusef("Refused: %s is larger than 4 MB, Azure's limit for a template.", arg)
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		return "", nil, refusef("Refused: %s cannot be read.", arg)
	}
	return real, raw, nil
}

// allowedPath answers where a path the team names really is, after links are followed, when that is
// inside the team's documents folder or a worktree of this run. A relative path is the documents
// folder's. Any other path is refused, the sentence ending with what the command takes.
func (p *plugin) allowedPath(arg, takes string) (string, error) {
	path := arg
	if !filepath.IsAbs(path) && p.docs != "" {
		path = filepath.Join(p.docs, path)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", refusef("Refused: %s cannot be read; give a path in the team's documents folder or a worktree of this run.", arg)
	}
	if p.insideTeamFolders(real) {
		return real, nil
	}
	return "", refusef("Refused: %s is outside the team's documents folder and this run's worktrees; this member %s.", arg, takes)
}

// insideTeamFolders reports whether a path, links already followed, is in the team's documents
// folder or a worktree of this run.
func (p *plugin) insideTeamFolders(real string) bool {
	var roots []string
	if p.docs != "" {
		roots = append(roots, p.docs)
	}
	roots = append(roots, p.trees...)
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		if within(r, real) {
			return true
		}
	}
	return false
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// secretFunctions matches every list function of a template: list() and each list*() function.
// They answer keys, connection strings and credentials, so a template that calls one is refused:
// app secrets go in Key Vault.
var secretFunctions = regexp.MustCompile(`(?i)\blist\w*\s*\(`)

var secretParam = regexp.MustCompile(`(?i)(password|secret|key|token|connectionstring)`)

// deployment is a checked template and its parameters: the exact bytes the what-if saw.
type deployment struct {
	rg         string
	file       string
	template   json.RawMessage
	parameters json.RawMessage
	sum        string // sha256 of template and parameters
	name       string
}

func (p *plugin) deploy(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "deploy", "<resource group> <template .bicep or .json> [parameters .json]"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	d, err := p.prepare(args)
	if err != nil {
		return "", err
	}
	rgID := rgPath(p.cfg.Subscription, d.rg)
	if err := p.mustOwn(change{id: rgID, kind: changeModify}); err != nil {
		return "", err
	}

	// WHAT-IF FIRST, ALWAYS.
	p.progress("azure: what-if of " + filepath.Base(d.file) + " into " + d.rg)
	changes, err := p.whatIf(d)
	if err != nil {
		return "", err
	}
	report, counts, refusals, ids, err := p.judge(d, changes)
	if err != nil {
		return "", err
	}
	head := fmt.Sprintf("What-if of %s into %s (sha256 %s): %d to create, %d to modify, %d to delete, %d unchanged.",
		filepath.Base(d.file), d.rg, d.sum[:12], counts["create"], counts["modify"], counts["delete"], counts["unchanged"])
	summary := fmt.Sprintf("%s into %s: %d create, %d modify, %d delete, %d unchanged", filepath.Base(d.file), d.rg,
		counts["create"], counts["modify"], counts["delete"], counts["unchanged"])
	p.planned(plan{command: "deploy", target: rgID, summary: summary, counts: counts, refusals: refusals})
	if len(refusals) > 0 {
		return "", refusal("Refused: the deployment changes what this team does not own, so none of it is deployed:\n- " +
			strings.Join(refusals, "\n- ") + "\n" + head)
	}
	if !p.cfg.apply() {
		return head + "\n" + strings.Join(report, "\n") + "\nwhatif: nothing was deployed. A person sets mode to apply in this member's settings to deploy it.", nil
	}

	// APPLY: the same bytes and parameters the what-if checked.
	p.progress("azure: deploying " + filepath.Base(d.file) + " into " + d.rg)
	path := rgID + "/providers/Microsoft.Resources/deployments/" + d.name + "?api-version=" + deployAPIVersion
	body := map[string]any{
		"tags":       p.ownTags(),
		"properties": map[string]any{"mode": "Incremental", "template": d.template, "parameters": d.parameters},
	}
	ans, err := p.az.arm(http.MethodPut, path, body, nil)
	if err != nil {
		return "", err
	}
	if _, err := p.az.wait(ans, "the deployment "+d.name); err != nil {
		return "", err
	}
	var result struct {
		Properties struct {
			ProvisioningState string `json:"provisioningState"`
			OutputResources   []struct {
				ID string `json:"id"`
			} `json:"outputResources"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"properties"`
	}
	if _, err := p.az.arm(http.MethodGet, path, nil, &result); err != nil {
		return "", err
	}
	if !strings.EqualFold(result.Properties.ProvisioningState, "Succeeded") {
		return "", fmt.Errorf("the deployment %s ended %s: %s %s", d.name, result.Properties.ProvisioningState, result.Properties.Error.Code, result.Properties.Error.Message)
	}
	if len(result.Properties.OutputResources) > 0 {
		ids = ids[:0]
		for _, r := range result.Properties.OutputResources {
			ids = append(ids, r.ID)
		}
	}
	p.applied("deploy", rgID, summary, ids)
	return fmt.Sprintf("Deployed %s into %s as %s (sha256 %s): %d created, %d modified.\n%s",
		filepath.Base(d.file), d.rg, d.name, d.sum[:12], counts["create"], counts["modify"], strings.Join(report, "\n")), nil
}

// prepare reads, compiles and checks the template and its parameters.
func (p *plugin) prepare(args []string) (*deployment, error) {
	d := &deployment{rg: args[0]}
	file, raw, err := p.readAllowed(args[1])
	if err != nil {
		return nil, err
	}
	d.file = file
	switch strings.ToLower(filepath.Ext(file)) {
	case ".bicep":
		p.progress("azure: compiling " + filepath.Base(file) + " with bicep")
		raw, err = compileBicep(p, file)
		if err != nil {
			return nil, err
		}
	case ".json":
	default:
		return nil, refusef("Refused: %s is not a Bicep (.bicep) or ARM JSON (.json) template.", args[1])
	}
	var tpl struct {
		Schema    string          `json:"$schema"`
		Resources json.RawMessage `json:"resources"`
	}
	if json.Unmarshal(raw, &tpl) != nil || len(tpl.Resources) == 0 {
		return nil, refusef("Refused: %s is not an ARM template: it has no resources.", args[1])
	}
	if s := strings.ToLower(tpl.Schema); s != "" && !strings.Contains(s, "/deploymenttemplate.json") {
		return nil, refusef("Refused: %s is not a resource group template (its $schema is %s); this member deploys into a resource group only.", args[1], tpl.Schema)
	}
	if m := secretFunctions.FindString(string(raw)); m != "" {
		return nil, refusef("Refused: %s calls %s, and this member refuses every list function, since they read keys, connection strings and credentials. App secrets go in Key Vault, set by a person or another plugin, and apps read them with a managed identity; nothing was deployed.", args[1], strings.TrimSpace(strings.TrimSuffix(m, "(")))
	}
	d.template = raw
	d.parameters = json.RawMessage(`{}`)
	if len(args) == 3 {
		pfile, praw, err := p.readAllowed(args[2])
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(filepath.Ext(pfile), ".json") {
			return nil, refusef("Refused: %s is not a parameters file (.json).", args[2])
		}
		var params struct {
			Parameters map[string]json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(praw, &params) != nil || params.Parameters == nil {
			return nil, refusef("Refused: %s is not an ARM parameters file: it has no parameters.", args[2])
		}
		for name, v := range params.Parameters {
			var val struct {
				Value     json.RawMessage `json:"value"`
				Reference json.RawMessage `json:"reference"`
			}
			_ = json.Unmarshal(v, &val)
			if secretParam.MatchString(name) && len(val.Value) > 0 && string(val.Value) != `""` && string(val.Value) != "null" {
				return nil, refusef("Refused: %s gives parameter %s a value in the file. A secret never goes in a parameters file or the repository: put it in Key Vault and give the parameter a Key Vault reference.", args[2], name)
			}
		}
		d.parameters, _ = json.Marshal(params.Parameters)
	}
	sum := sha256.New()
	sum.Write(d.template)
	sum.Write([]byte{0})
	sum.Write(d.parameters)
	d.sum = hex.EncodeToString(sum.Sum(nil))
	d.name = "yawble-" + d.sum[:16]
	return d, nil
}

// whatIfChange is one change a what-if answers.
type whatIfChange struct {
	ResourceID string         `json:"resourceId"`
	ChangeType string         `json:"changeType"`
	After      map[string]any `json:"after"`
}

func (p *plugin) whatIf(d *deployment) ([]whatIfChange, error) {
	path := rgPath(p.cfg.Subscription, d.rg) + "/providers/Microsoft.Resources/deployments/" + d.name + "/whatIf?api-version=" + deployAPIVersion
	body := map[string]any{"properties": map[string]any{
		"mode": "Incremental", "template": d.template, "parameters": d.parameters,
		"whatIfSettings": map[string]any{"resultFormat": "FullResourcePayloads"},
	}}
	ans, err := p.az.arm(http.MethodPost, path, body, nil)
	if err != nil {
		return nil, err
	}
	raw, err := p.az.wait(ans, "the what-if of "+filepath.Base(d.file))
	if err != nil {
		return nil, err
	}
	var result struct {
		Status     string `json:"status"`
		Properties struct {
			Changes []whatIfChange `json:"changes"`
		} `json:"properties"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, fmt.Errorf("the what-if's answer could not be read")
	}
	if result.Error.Code != "" || strings.EqualFold(result.Status, "Failed") {
		return nil, fmt.Errorf("the what-if failed: %s %s", result.Error.Code, result.Error.Message)
	}
	return result.Properties.Changes, nil
}

// judge runs every change of a what-if through the guard and the template rules, and answers the
// lines of the report, the counts, the refusals (each naming its resource) and the changed ids.
func (p *plugin) judge(d *deployment, changes []whatIfChange) (report []string, counts map[string]int, refusals, ids []string, err error) {
	counts = map[string]int{"create": 0, "modify": 0, "delete": 0, "unchanged": 0}
	ids = []string{}
	var principals map[string]bool
	for _, c := range changes {
		kind := strings.ToLower(c.ChangeType)
		r, _ := parseID(c.ResourceID)
		switch kind {
		case "nochange":
			counts["unchanged"]++
			continue
		case "ignore":
			// ARM also answers Ignore for resources beyond its nested-template expansion limits,
			// which it still deploys, so each one must be inside what the team owns.
			counts["unchanged"]++
			why, err := p.guard(change{id: c.ResourceID, kind: changeModify})
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if why != "" {
				refusals = append(refusals, why)
			}
			continue
		case "delete":
			counts["delete"]++
			refusals = append(refusals, c.ResourceID+" would be deleted; deploy never deletes (use delete, with allowDelete on)")
			continue
		case "create":
			counts["create"]++
		default: // modify, deploy, unsupported
			counts["modify"]++
		}
		report = append(report, fmt.Sprintf("- %s %s", c.ChangeType, c.ResourceID))
		ids = append(ids, c.ResourceID)

		ch := change{id: c.ResourceID, kind: changeModify}
		if kind == "create" {
			ch.kind = changeCreate
		}
		ch.tags = stringMap(c.After["tags"])
		props, _ := c.After["properties"].(map[string]any)
		isAssignment := r.ext != nil && strings.EqualFold(r.ext.ns+"/"+strings.Join(r.ext.types, "/"), "Microsoft.Authorization/roleAssignments")
		if isAssignment {
			ch.role = roleOf(str(props["roleDefinitionId"]))
		}
		why, err := p.guard(ch)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if why != "" {
			refusals = append(refusals, why)
			continue
		}

		// THE TEMPLATE RULES beyond ownership.
		t := r.typ()
		if r.ext != nil {
			t = r.ext.ns + "/" + strings.Join(r.ext.types, "/")
		}
		switch {
		case isAssignment:
			if _, ok := grantable[ch.role]; !ok {
				refusals = append(refusals, fmt.Sprintf("%s grants role %s, which is not one this member grants (%s)", c.ResourceID, ch.role, grantableNames()))
				continue
			}
			if !strings.EqualFold(str(props["principalType"]), "ServicePrincipal") {
				refusals = append(refusals, fmt.Sprintf("%s does not set principalType to ServicePrincipal; this member grants roles only to managed identities it created, never to a user or a group", c.ResourceID))
				continue
			}
			if pid := str(props["principalId"]); guidAnywhere.MatchString(pid) {
				if principals == nil {
					if principals, err = p.ownedPrincipals(d.rg); err != nil {
						return nil, nil, nil, nil, err
					}
				}
				if !principals[strings.ToLower(pid)] {
					refusals = append(refusals, fmt.Sprintf("%s grants to principal %s, which is not a managed identity in resource group %s", c.ResourceID, pid, d.rg))
				}
			}
		case strings.EqualFold(t, "Microsoft.Authorization/roleDefinitions"):
			refusals = append(refusals, c.ResourceID+" defines a custom role; this member grants only built-in roles")
		case strings.EqualFold(t, "Microsoft.Storage/storageAccounts"):
			if v, ok := props["allowSharedKeyAccess"].(bool); !ok || v {
				refusals = append(refusals, c.ResourceID+" does not set allowSharedKeyAccess to false; storage accounts are created with shared key access off")
			}
		}
	}
	sort.Strings(refusals)
	return report, counts, refusals, ids, nil
}

// ownedPrincipals are the principals of the managed identities in an owned resource group: each
// user-assigned identity's and each resource's system-assigned identity's.
func (p *plugin) ownedPrincipals(rg string) (map[string]bool, error) {
	found := map[string]bool{}
	var identities []string
	err := p.az.list(rgPath(p.cfg.Subscription, rg)+"/resources?api-version="+rgAPIVersion, func(raw json.RawMessage) {
		var r struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Identity struct {
				PrincipalID string `json:"principalId"`
			} `json:"identity"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return
		}
		if r.Identity.PrincipalID != "" {
			found[strings.ToLower(r.Identity.PrincipalID)] = true
		}
		if strings.EqualFold(r.Type, "Microsoft.ManagedIdentity/userAssignedIdentities") {
			identities = append(identities, r.ID)
		}
	})
	if err != nil {
		return nil, err
	}
	for _, id := range identities {
		var body struct {
			Properties struct {
				PrincipalID string `json:"principalId"`
			} `json:"properties"`
		}
		if _, err := p.az.get(id+"?api-version="+knownVersions["Microsoft.ManagedIdentity/userAssignedIdentities"], &body); err != nil {
			return nil, err
		}
		if body.Properties.PrincipalID != "" {
			found[strings.ToLower(body.Properties.PrincipalID)] = true
		}
	}
	return found, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, item := range m {
		out[k] = str(item)
	}
	return out
}
