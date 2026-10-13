package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Azure SQL: servers that accept only Microsoft Entra sign-ins (there is no SQL password), databases
// in what the team owns or, created and tagged, on a server in sharedSqlServers; SQL run only on a
// database the team owns; database roles granted only to the team's own managed identities.

var (
	dbName      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,127}$`)
	dbSKU       = regexp.MustCompile(`^[A-Za-z0-9_]{1,40}$`)
	goLine      = regexp.MustCompile(`(?im)^[ \t]*GO[ \t]*;?[ \t]*$`)
	sqlRoles    = []string{"db_datareader", "db_datawriter", "db_ddladmin"}
	sqlUserName = regexp.MustCompile(`^[A-Za-z0-9_.()-]{1,128}$`)
)

// sqlHost is a server's address.
func sqlHost(server string) string { return strings.ToLower(server) + ".database.windows.net" }

func init() {
	register(
		command{words: []string{"sql", "server", "create"}, usage: "<resource group> <server>", run: (*plugin).sqlServerCreate},
		command{words: []string{"sql", "db", "create"}, usage: "<server> <database> <sku>", run: (*plugin).sqlDBCreate},
		command{words: []string{"sql", "exec"}, usage: "<server>/<database> <file.sql>", run: (*plugin).sqlExec},
		command{words: []string{"sql", "grant"}, usage: "<server>/<database> <managed identity id> <role...>", run: (*plugin).sqlGrant},
	)
}

func (p *plugin) sqlServerCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "sql server create", "<resource group> <server>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name := args[0], strings.ToLower(args[1])
	if !sqlName.MatchString(name) {
		return "", refusef("Refused: %q is not a SQL server name: lower-case letters, digits and hyphens, unique in all of Azure.", args[1])
	}
	id := rgPath(p.cfg.Subscription, rg) + "/providers/Microsoft.Sql/servers/" + name
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	oid, tenant, account, err := p.signedIn()
	if err != nil {
		return "", err
	}
	what := fmt.Sprintf("create SQL server %s in %s that accepts only Microsoft Entra sign-ins, with %s as its administrator, tagged for this team", name, rg, account)
	if !p.cfg.apply() {
		return p.whatif("sql server create", id, what, map[string]int{"create": 1}), nil
	}
	location, err := p.rgLocation(rg)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"location": location, "tags": p.ownTags(),
		"properties": map[string]any{
			"version": "12.0", "minimalTlsVersion": "1.2", "publicNetworkAccess": "Enabled",
			"administrators": map[string]any{
				"administratorType": "ActiveDirectory", "principalType": "User", "login": account,
				"sid": oid, "tenantId": tenant, "azureADOnlyAuthentication": true,
			},
		},
	}
	p.progress("azure: creating SQL server " + name + " (a few minutes)")
	if _, err := p.put(id, body, "the SQL server "+name); err != nil {
		return "", err
	}
	p.applied("sql server create", id, what, []string{id})
	return fmt.Sprintf("SQL server %s is ready in %s at %s; it accepts only Microsoft Entra sign-ins, and %s is its administrator. Its firewall lets nothing in until a rule is added: deploy Microsoft.Sql/servers/firewallRules in a template (AllowAzureServices, 0.0.0.0, for apps in Azure, and this instance's address, which a refused sql exec names), or a person adds an address in the Azure portal (SQL server, Networking).", name, rg, sqlHost(name), account), nil
}

func (p *plugin) sqlDBCreate(args []string) (string, error) {
	if err := argsExactly(args, 3, 3, "sql db create", "<server> <database> <sku>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	serverName, _ := lastName(args[0], sqlServerID, ".database.windows.net")
	name, sku := args[1], args[2]
	if !dbName.MatchString(name) || strings.EqualFold(name, "master") {
		return "", refusef("Refused: %q is not a database name this member creates.", name)
	}
	if !dbSKU.MatchString(sku) {
		return "", refusef("Refused: %q is not a database sku such as Basic, S0 or GP_S_Gen5_1.", sku)
	}
	server, err := p.findByName("Microsoft.Sql/servers", serverName, "SQL server")
	if err != nil {
		return "", err
	}
	id := server.ID + "/databases/" + name
	if tags, err := p.tagsOf(id); err != nil {
		return "", err
	} else if tags != nil {
		if !p.ownedBy(tags) {
			return "", refusef("Refused: database %s already exists on %s and is not this team's. Nothing was changed.", name, server.Name)
		}
		return fmt.Sprintf("Database %s on %s already exists and is this team's. Nothing was changed.", name, server.Name), nil
	}
	if err := p.mustOwn(change{id: id, kind: changeCreate, tags: p.ownTags()}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("create database %s (%s) on SQL server %s, tagged for this team", name, sku, server.Name)
	if !p.cfg.apply() {
		return p.whatif("sql db create", id, what, map[string]int{"create": 1}), nil
	}
	location := server.Location
	if location == "" {
		r, _ := parseID(server.ID)
		if location, err = p.rgLocation(r.rg); err != nil {
			return "", err
		}
	}
	body := map[string]any{"location": location, "tags": p.ownTags(), "sku": map[string]any{"name": sku}}
	p.progress("azure: creating database " + name + " on " + server.Name)
	if _, err := p.put(id, body, "the database "+name); err != nil {
		return "", err
	}
	p.applied("sql db create", id, what, []string{id})
	return fmt.Sprintf("Database %s (%s) is ready on %s, tagged for this team. Run SQL on it with sql exec %s/%s <file.sql>.", name, sku, server.Name, server.Name, name), nil
}

// ownedDatabase reads <server>/<database>, finds it, and refuses one the team does not own: the
// database carries its tags, or its server or resource group does.
func (p *plugin) ownedDatabase(arg string) (resource, string, error) {
	if err := p.needSubscription(); err != nil {
		return resource{}, "", err
	}
	serverArg, name, ok := strings.Cut(arg, "/")
	serverName, _ := lastName(serverArg, sqlServerID, ".database.windows.net")
	if !ok || !sqlName.MatchString(serverName) || !dbName.MatchString(name) {
		return resource{}, "", refusef("Refused: %q is not <server>/<database>, such as my-sql-server/my-db.", arg)
	}
	server, err := p.findByName("Microsoft.Sql/servers", serverName, "SQL server")
	if err != nil {
		return resource{}, "", err
	}
	id := server.ID + "/databases/" + name
	tags, err := p.tagsOf(id)
	if err != nil {
		return resource{}, "", err
	}
	if tags == nil {
		return resource{}, "", refusef("Refused: there is no database %s on %s.", name, server.Name)
	}
	if why, err := p.guard(change{id: id, kind: changeModify}); err != nil {
		return resource{}, "", err
	} else if why != "" {
		return resource{}, "", refusef("Refused: %s. This member runs SQL only on databases its team owns. Nothing was run.", why)
	}
	return server, name, nil
}

// batches cuts a SQL file at its GO lines.
func batches(sql string) []string {
	var out []string
	for _, b := range goLine.Split(sql, -1) {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func (p *plugin) sqlExec(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "sql exec", "<server>/<database> <file.sql>"); err != nil {
		return "", err
	}
	file, err := p.allowedPath(args[1], "runs only SQL files from those")
	if err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		return "", refusef("Refused: %s is not a file.", args[1])
	}
	if info.Size() > 4<<20 {
		return "", refusef("Refused: %s is larger than 4 MB.", args[1])
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", refusef("Refused: %s cannot be read.", args[1])
	}
	server, db, err := p.ownedDatabase(args[0])
	if err != nil {
		return "", err
	}
	list := batches(string(raw))
	if len(list) == 0 {
		return "", refusef("Refused: %s holds no SQL.", args[1])
	}
	id := server.ID + "/databases/" + db
	what := fmt.Sprintf("run %s (%d batches) on database %s on %s", filepath.Base(file), len(list), db, server.Name)
	if !p.cfg.apply() {
		return p.whatif("sql exec", id, what, map[string]int{"modify": 1}), nil
	}
	out, err := p.runSQL(server.Name, db, list)
	if err != nil {
		return "", err
	}
	p.applied("sql exec", id, fmt.Sprintf("ran %s on %s/%s", filepath.Base(file), server.Name, db), []string{id})
	return fmt.Sprintf("Ran %s on %s/%s.\n%s", filepath.Base(file), server.Name, db, out), nil
}

// runSQL runs each batch in order, stopping at the first that fails, and answers the rows each
// changed, or each result capped at 200 rows.
func (p *plugin) runSQL(server, db string, list []string) (string, error) {
	token := p.az.tokens[apiSQL]
	if token == "" {
		return "", missingToken(apiSQL)
	}
	p.progress("azure: signing in to " + server + "/" + db)
	conn, err := sqlConnect(sqlHost(server), db, token)
	if err != nil {
		return "", sqlHint(err)
	}
	defer conn.close()
	var parts []string
	for i, b := range list {
		p.progress(fmt.Sprintf("azure: running batch %d of %d on %s/%s", i+1, len(list), server, db))
		out, err := conn.exec(b)
		if err != nil {
			return "", fmt.Errorf("batch %d: %w", i+1, err)
		}
		text := describe(out)
		if len(out.errors) > 0 {
			done := ""
			if len(parts) > 0 {
				done = "\nThe batches before it ran:\n" + strings.Join(parts, "\n")
			}
			return "", fmt.Errorf("batch %d of %d failed, and the batches after it did not run: %s%s", i+1, len(list), strings.Join(out.errors, "; "), done)
		}
		if len(list) > 1 {
			text = fmt.Sprintf("Batch %d: %s", i+1, text)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n"), nil
}

// describe says what a batch answered: each result as a table, then the rows changed.
func describe(out *tdsOutcome) string {
	var parts []string
	for _, s := range out.sets {
		var b strings.Builder
		fmt.Fprintf(&b, "%d rows", s.total)
		if s.more > 0 {
			fmt.Fprintf(&b, " (the first %d shown)", maxSQLRows)
		}
		b.WriteString(":\n" + strings.Join(s.columns, " | "))
		for _, r := range s.rows {
			b.WriteString("\n" + strings.Join(r, " | "))
		}
		parts = append(parts, b.String())
	}
	if out.counted || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d rows affected.", out.affected))
	}
	if len(out.messages) > 0 {
		parts = append(parts, "Messages: "+strings.Join(out.messages, "; "))
	}
	return strings.Join(parts, "\n")
}

// sqlHint adds the next step to a sign-in Azure SQL refused.
func sqlHint(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "error 40615"):
		return fmt.Errorf("%s. Add this instance's address to the server's firewall: deploy a Microsoft.Sql/servers/firewallRules rule for it on a server the team owns, or a person adds it in the Azure portal (SQL server, Networking)", msg)
	case strings.Contains(msg, "error 18456"), strings.Contains(msg, "error 33134"):
		return fmt.Errorf("%s. The signed-in account needs to be the server's Microsoft Entra administrator, or a user in the database", msg)
	}
	return err
}

func (p *plugin) sqlGrant(args []string) (string, error) {
	if len(args) < 3 {
		return "", refusef("Refused: sql grant takes <server>/<database> <managed identity id> <role...>; the roles are %s.", strings.Join(sqlRoles, ", "))
	}
	var roles []string
	for _, r := range args[2:] {
		r = strings.ToLower(r)
		if !contains(sqlRoles, r) {
			return "", refusef("Refused: %s is not a database role this member grants. It grants only %s.", r, strings.Join(sqlRoles, ", "))
		}
		if !contains(roles, r) {
			roles = append(roles, r)
		}
	}
	server, db, err := p.ownedDatabase(args[0])
	if err != nil {
		return "", err
	}
	identity := args[1]
	principal, err := p.identityPrincipal(identity)
	if err != nil {
		return "", err
	}
	ir, _ := parseID(identity)
	user := ir.name()
	if !sqlUserName.MatchString(user) || !guidForm.MatchString(strings.ToLower(principal)) {
		return "", refusef("Refused: %s cannot be a database user's name.", user)
	}
	quoted := "[" + strings.ReplaceAll(user, "]", "]]") + "]"
	sql := fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')\n  CREATE USER %s FROM EXTERNAL PROVIDER WITH OBJECT_ID = '%s';\n",
		strings.ReplaceAll(user, "'", "''"), quoted, strings.ToLower(principal))
	for _, r := range roles {
		sql += fmt.Sprintf("ALTER ROLE [%s] ADD MEMBER %s;\n", r, quoted)
	}
	id := server.ID + "/databases/" + db
	what := fmt.Sprintf("make managed identity %s a user of %s/%s with %s", user, server.Name, db, strings.Join(roles, ", "))
	if !p.cfg.apply() {
		return p.whatif("sql grant", id, what, map[string]int{"modify": 1}), nil
	}
	if _, err := p.runSQL(server.Name, db, []string{sql}); err != nil {
		return "", err
	}
	p.applied("sql grant", id, what, []string{id})
	return fmt.Sprintf("Managed identity %s is a user of %s/%s with %s.", user, server.Name, db, strings.Join(roles, ", ")), nil
}
