---
name: plugin-azure
description: Use when a team needs Azure resources - resource groups, Bicep or ARM deployments, image builds, Azure SQL databases and SQL, storage blobs, tables and queues, Key Vault, Container Apps and their logs and domains, DNS, role assignments for its managed identities, budgets - through the Azure plugin member: its commands, whatif versus apply, the ownership rule, and where app secrets go.
roles: manager member
---

# Azure (plugin member)

A plugin member, not an agent: it runs no model. Send it commands with `tell`, one per line and
nothing else: a line that is not a command (a sentence of context, a note on what not to repeat)
is refused and fails the run, though the commands beside it still run. It acts in one Azure
subscription, signed in as the person who connected it, and hands back one result. It can never
do more than that person can in Azure. You never see a token, and never ask anybody for one.

## whatif and apply

`mode` is a person's setting. In `whatif` (the default) every changing command answers what it
would do and changes nothing in Azure. In `apply` it makes the change. Neither you nor the member
can switch it: when a plan is ready, ask the person to set `mode` to `apply` in the member's
settings, then send the same command again. Ask them to set it back to `whatif` when the work is
done.

`allowDelete` is off by default; while off, every delete is refused, in either mode.

## The ownership rule

Everything the member creates carries the tags `yawble-owner: <team id>` and
`yawble-plugin: azure`. It changes only:

- a resource carrying this team's tags, or anything inside a resource group that does;
- a new database on a server in `sharedSqlServers`, which is tagged and then owned;
- images built in, and AcrPull granted on, an Azure Container Registry in
  `sharedContainerRegistries` (`sharedRegistries` is its old name, still read; setup and
  `whoami` say to move what is still there).

SQL runs only on a database the team owns, even on a shared server. Blob, table and queue changes
go only to storage accounts the team owns. It reads anything the person can read. Anything else is refused, naming the resource, and
`plugin.azure.refused` is published.

## Commands

Quote a value with spaces (`"Storage Blob Data Contributor"`).

- `setup` - checks the sign-in, the subscription, that the person's role can create resource
  groups, the regions, each shared SQL server and registry, and that the resource providers it
  uses are registered (registering them in apply when the role allows). Ends with `Next:` and
  numbered steps in words, or `Ready:`. Pass the `Next:` steps to the person as they are.
- `whoami` - the account, the subscription and every setting.
- `list [resource group]` - the resource groups, or the resources in one; `[owned]` marks this
  team's.
- `show <resource id>` - one resource as Azure answers it, with any key-, password- or
  secret-named value hidden.
- `rg create <name> <region>` - a resource group named `namePrefix` + name, in a region from
  `regions`, tagged for the team, with the monthly budget (alerts at 80% and 100%) when
  `monthlyBudget` is set.
- `deploy <resource group> <template> [parameters file]` - a Bicep (`.bicep`) or ARM JSON
  (`.json`) template from the team's documents folder or a worktree of this run, into a resource
  group the team owns. A relative path is read from the team's documents folder. It always runs a
  what-if first and answers each change. Any change outside what the team owns - a nested
  deployment into another resource group, or a resource the what-if marks as ignored, included -
  refuses the whole deployment, naming each resource. In apply it then deploys exactly the bytes
  and parameters it checked (the answer names their sha256), in Incremental mode, so it never
  deletes. A template that calls any list function (`list`, `listKeys`, `listAuthKeys` and every
  other `list*`; every string and object key is checked after its JSON escapes are decoded, with
  any space, Unicode space or invisible character before the bracket, so a word such as `list (`
  in a description counts too, and a template that is not valid JSON is
  refused), a storage account without `allowSharedKeyAccess: false`, a custom role, or a
  role assignment the `role assign` rules would refuse, is refused.
- `role assign <role> <managed identity id> <scope id>` - grants one of Contributor, Storage
  Blob Data Contributor, Storage Table Data Contributor, Storage Queue Data Contributor, Key Vault
  Secrets Officer, Key Vault Secrets User or AcrPull, on a scope the team owns (or AcrPull on a
  shared registry), to a managed identity the team created: a user-assigned identity's resource
  id, or the resource id of an owned resource with a system-assigned identity. Never to a user or
  a group.
- `role list <scope id>` - the role assignments at a scope.
- `dns zone create <resource group> <zone>` - a zone in an owned group; answers the name servers
  to set at the registrar.
- `dns record set <zone> <name> <A|AAAA|CNAME|TXT|MX> <value...> [ttl=<seconds>]` - in a zone
  the team owns; `@` is the zone itself; MX values come as `<preference> <host>` pairs.
- `dns record delete <zone> <name> <type>` - needs `allowDelete`.
- `delete <resource id or resource group>` - owned only, with `allowDelete` on.

A path (template, build folder, SQL file, file to upload) is in the team's documents folder or a
worktree of this run; a relative path is the documents folder's, and an absolute one is taken as
it is. `blob get` writes only in the documents folder, to a path relative to it or absolute
inside it.

Registries:

- `acr create <resource group> <registry> [Basic|Standard|Premium]` - admin user off.
- `acr build <registry> <image:tag> <folder>` - into an owned registry or one in
  `sharedContainerRegistries`. Uploads the folder (without `.git` and what `.dockerignore` leaves out),
  builds its `Dockerfile` in the registry, and answers the digest and the end of the log.

Azure SQL (Microsoft Entra sign-in only; no SQL password exists):

- `sql server create <resource group> <server>` - the signed-in person is its administrator. Its
  firewall lets nothing in: deploy `Microsoft.Sql/servers/firewallRules` in a template
  (`AllowAzureServices`, 0.0.0.0 to 0.0.0.0, for apps in Azure, and this instance's address, which
  the first refused `sql exec` names) before `sql exec` reaches it.
- `sql db create <server> <database> <sku>` - on an owned server, or tagged on a server in
  `sharedSqlServers`. A sku such as `Basic`, `S0` or `GP_S_Gen5_1`.
- `sql exec <server>/<database> <file.sql>` - runs the file's batches (split at `GO` lines) in
  order, stopping at the first that fails; answers rows affected, or each result capped at 200
  rows. A byte-order mark at the file's start is dropped. On owned databases only. In whatif it does not connect, even for a SELECT.
- `sql grant <server>/<database> <managed identity id> <role...>` - makes a managed identity the
  team created a database user with `db_datareader`, `db_datawriter` and/or `db_ddladmin`.

Storage (in accounts the team owns; reads anywhere the person has a Storage Data role):

- `storage create <resource group> <account> [sku]` - StorageV2, shared key access off, no
  public blob access; the signed-in person gets Storage Blob, Table and Queue Data Contributor on
  it (a few minutes to apply), and running it again on an account the team owns grants them again.
- `storage container|table|queue create <account> <name>`.
- `blob list <account> <container> [prefix]` (first 200), `blob get <account> <container> <blob>
  [path]` (writes into the team's documents), `blob put <account> <container> <blob> <file>`,
  `blob delete <account> <container> <blob>`.
- `table query <account> <table> [filter]` (first 200, an OData filter such as
  `PartitionKey eq 'orders'`), `table upsert <account> <table> <partition key> <row key>
  '<JSON>'` (the JSON in single quotes), `table delete <account> <table> <partition key> <row
  key>`.
- `queue send <account> <queue> <text>` (base64, as Azure Functions reads it), `queue peek
  <account> <queue> [count]`, `queue receive <account> <queue> [count]` (hides the messages for
  5 minutes and answers each pop receipt), `queue delete <account> <queue> <message id> <pop
  receipt>`.
- Every `delete` needs `allowDelete`.

Key Vault:

- `keyvault create <resource group> <vault>` - access by Azure roles.
- `keyvault secrets <vault>` - the secret names only, never a value, listed through Azure
  management. The signed-in account needs Reader (or more) on the vault.

Container Apps:

- `containerapp env create <resource group> <environment>` - with its own Log Analytics
  workspace `<environment>-logs`.
- `containerapp create <resource group> <app> <environment> <registry>.azurecr.io/<image:tag>
  [port=<n>] [cpu=<0.25 to 2>] [env=<NAME>=<value>]... [secret=<NAME>=https://<vault>.vault.azure.net/secrets/<name>]...
  [secret=<NAME>=appSecret<1-8>]...`
  - in an owned environment, from an owned or shared registry. Creates the identity
  `<app>-id`, grants it AcrPull on the registry and Key Vault Secrets User on each owned vault a
  `secret=` names, then creates or updates the app (0 to 1 replicas, 0.5 CPU and 1 GiB unless `cpu=` names
  another size, which gets twice as many GiB). `port=`
  gives it public HTTPS ingress. Each Key Vault secret must already be in the vault, and each
  `appSecret<N>` must be bound and set (see Secrets). An `env=` that looks like a secret is
  refused.
- `containerapp show <app>`, `containerapp revisions <app>`, `containerapp logs <app> [minutes]`
  (default 30, at most 500 lines) - reads. The logs are queried through Azure management on the
  environment's workspace; the account needs Log Analytics Reader (or Reader) on it. An
  environment with no workspace whose diagnostic setting archives its logs to a storage account
  has them read from that account's hourly blobs instead (console lines, and the platform's own
  lines marked `system:`); the account needs Storage Blob Data Reader (or more) on it.
- `containerapp restart <app>` - restarts the latest ready revision.
- `domain bind <app> <hostname>` - says the TXT and CNAME (A for a domain's apex) records the
  domain needs, and sets them when the team owns its Azure DNS zone. Once they resolve, run it
  again: it creates the managed certificate and binds it. Pass the records to the person when the
  zone is elsewhere.

The first `deploy` of a Bicep file downloads the pinned bicep compiler (about 120 MB) once.

## Events

- `plugin.azure.planned` - what a changing command would do, or a deployment's what-if: counts
  by change type (`create`, `modify`, `delete`, `unchanged`) and `refusals`.
- `plugin.azure.applied` - a change made in apply, with `resourceIds`.
- `plugin.azure.refused` - `command` and `reason`.

None ever holds a token, key or connection string.

## Secrets

The member never reads a key, a connection string or a Key Vault secret value, and creates storage
accounts with shared key access off. Apps reach storage, Key Vault and SQL with their managed
identity and a role from the list above.

An app secret (a third-party API key, a webhook signing secret) goes in Key Vault, set by a person
or by another plugin. It never goes in a template, a parameters file, the repository, a command or
a message. A template refers to it with a Key Vault reference; a container app with
`secret=<NAME>=<the secret's address>`. The app reads it with its managed identity (Key Vault
Secrets User). The member never calls Key Vault for a value.

An app secret can also come from this member's own keys, `appSecret1` to `appSecret8`: a person
binds the field to a key and sets the key's value on the screen (this member's settings).
`secret=<NAME>=appSecret<N>` on `containerapp create` puts the value in the app's own secrets and
nowhere else; the member never writes it out. When the field is unbound or its key is not set, the
command is refused naming the key: tell the person to set it in this member's settings, then run
the command again. Never put a secret's value in an instruction.
