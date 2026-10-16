# Azure 1.1.3

A plugin member that builds and runs a team's own Azure resources: resource groups, deployments
from Bicep or ARM templates, container registries and image builds, Azure SQL databases and the SQL
run on them, storage accounts with their blobs, tables and queues, Key Vaults, Container Apps with
their logs and custom domains, DNS zones and records, budgets, and role assignments for the managed
identities it creates. You sign in with Microsoft once; the member then acts **with your own Azure
rights**, so it can never do more than you can yourself in the Azure portal.

Everything starts in **whatif**: each changing command says what it would do and changes nothing,
until you set `mode` to `apply`.

## New in 1.1.3

- **A failed deployment says why.** When `deploy` ends failed, the answer lists each failed
  operation of the deployment - resource type, name, Azure's error code and message, one line
  each, at most 10 and then how many more - instead of only Azure's "At least one resource
  deployment operation failed".
- **`show` reads a resource with its own type's api-version**, the last resource type in its id:
  a diagnostic setting on an environment is read as `Microsoft.Insights/diagnosticSettings`, not
  as the environment. A type it knows no version for, and Azure lists none for, is refused naming
  the type, never sent with a guess.
- **`acr build` packs portable modes**: files 0644, files with any execute bit 0755, folders 0755,
  all owned by root (uid and gid 0, no names), as git checks them out and `docker build` sees
  them, whatever the modes on disk. An image whose last stage runs as a non-root user can read its
  own files.
- **You can read the logs it archives.** `storage create` also gives you Storage Blob Data Reader
  on the new account, and a `deploy` whose diagnostic setting sends a Container Apps environment's
  logs to a storage account the team owns gives you that role there, in the same command, recorded
  and reported like its other role assignments, so `containerapp logs` reads them at once. When
  your role may not assign roles, the answer says so once, with who can fix it.
- **An app knows its managed identity.** `containerapp create` sets the app's `AZURE_CLIENT_ID` to
  the client id of its identity `<app>-id`, unless the command sets `AZURE_CLIENT_ID` itself, so
  Azure's SDKs (`DefaultAzureCredential`, `ManagedIdentityCredential`) sign in as it.
- **`domain bind` checks DNS where the records are**: it asks the domain's own authoritative name
  servers (read from its NS records), falling back to a public resolver, never only this
  machine's resolver, so a cached "not found" no longer hides records that exist.
- **`domain bind` says "run again"** when the records are in place but Azure has not seen them
  yet, instead of asking a person to set records that exist.
- **Binding one host name never removes another**: `domain bind` keeps every other custom domain
  on the app as Azure lists it.

## Azure in two minutes

- **Azure** is Microsoft's cloud. You pay for what you create in it.
- A **subscription** is the account the bill goes to. Everything in Azure lives in one. You can
  see yours in the Azure portal (https://portal.azure.com) under **Subscriptions**; each has a
  name and an id such as `00000000-0000-0000-0000-000000000000`. If you have none, the portal
  offers to create one.
- A **resource group** is a folder in a subscription that holds related resources, such as
  `my-project-rg`. Deleting a group deletes everything in it.
- A **role** decides what your account may do in a subscription. **Contributor** may create and
  change resources; **Reader** may only look. The subscription's owner gives roles under
  **Access control (IAM)**.
- A **region** is where a resource runs, such as `westeurope` or `eastus2`.
- A **managed identity** is an identity Azure gives an app, so the app reaches storage, Key Vault
  or a database without any password.

## Set up, with no script

1. **Marketplace**, **Get** Azure, then **Add member** on a team and choose the Azure plugin.
2. **Sign in.** The member's `microsoft` connection slot asks you to sign in with Microsoft: it
   shows a link and a code. Open the link, enter the code, sign in with the account you use in the
   Azure portal, and accept. Microsoft asks once for the three APIs the member uses: Azure
   management, Azure SQL and Azure Storage. Key Vault and the apps' logs are reached through Azure
   management, so they need no sign-in of their own.
3. **Tell the member `setup`.** With no subscription set yet, it lists every subscription your
   account can read, by id and name.
4. **Settings.** In the member's settings, put the subscription's id in `subscription` and pick the
   regions you want resource groups in from `regions` (eastus2 unless you choose others). Everything else has a default (below).
5. **Tell the member `setup` again.** It checks each thing and says the next step in words:
   - you are signed in, with a token for each API;
   - the subscription can be read;
   - your role allows creating resource groups (if not, ask the subscription's owner for
     Contributor);
   - each region in `regions` is offered to the subscription;
   - each SQL server in `sharedSqlServers` and Azure Container Registry in `sharedContainerRegistries` can be reached;
   - the resource providers it uses (Microsoft.App, Microsoft.OperationalInsights,
     Microsoft.Storage, Microsoft.KeyVault, Microsoft.Network, Microsoft.ContainerRegistry,
     Microsoft.Sql, Microsoft.ManagedIdentity, Microsoft.Consumption and Microsoft.Insights) are
     registered. A
     provider is Azure's switch for one kind of resource. In apply, setup registers any that are
     missing when your role allows it; in whatif it says it would.

   It ends with `Ready:` or with `Next:` and numbered steps.
6. When the team has a plan you agree with, set `mode` to `apply`, and set it back to `whatif`
   when the work is done.

## Settings

| Setting | Default | What it does |
|---|---|---|
| `subscription` | empty | The id of the Azure subscription this member works in; `setup` lists the ones your account can read. |
| `regions` | `eastus2` | The Azure regions resource groups may be created in, chosen from a list of Azure regions. |
| `mode` | `whatif` | `whatif` answers what each changing command would do and changes nothing; `apply` makes the changes. |
| `allowDelete` | off | Off: every delete is refused, in whatif and apply; on: the member may delete what its team owns. |
| `sharedSqlServers` | empty | Existing SQL servers, by name, on which the member may create its own databases. |
| `sharedContainerRegistries` | empty | Existing Azure Container Registries (ACR), by name such as `myregistry`, it may build and push images to and grant pull rights on; a Container App's image is pulled from one of them. |
| `sharedRegistries` (advanced) | empty | The old name of `sharedContainerRegistries`, still read; setup and `whoami` name what is still under it so a person moves it. |
| `monthlyBudget` | 0 | A monthly budget put on every resource group it creates, alerting at 80% and 100%; 0 means none. |
| `budgetEmails` | empty | Who gets the budget alerts; needed when `monthlyBudget` is more than 0. |
| `namePrefix` (advanced) | empty | Text put before the name of every resource group it creates. |

Only a person sets these. A Manager or another agent cannot change them.

## What sign-in allows

The sign-in lets the member call Azure as you, for as long as the connection stays connected.
Microsoft gives the platform one token per API, each for about an hour, and the platform renews
them. The member never sees your password, and no token appears in anything it writes.

It acts with your own rights: whatever your roles allow, and nothing more. If your account can
only read a subscription, the member can only read it too.

Data inside a resource needs a data role of its own, even for a subscription's Owner:

- reading and writing blobs, tables and queues needs Storage Blob, Table or Queue Data Contributor
  on the storage account (or its resource group);
- listing a Key Vault's secret names needs Reader (or more) on the vault, since the member lists
  them through Azure management, which answers names and never a value;
- reading an app's logs needs Log Analytics Reader (or Reader, or more) on its environment's
  workspace, which the member queries through Azure management, or, for an environment whose logs
  are archived to a storage account instead, Storage Blob Data Reader (or more) on that account;
- running SQL needs your account to be the SQL server's Microsoft Entra administrator (true of a
  server the member creates) or a user in the database, and the server's firewall to let this
  instance in (Azure portal, SQL server, Networking).

## What it will and will not touch

It **will**:

- read anything your account can read: `list`, `show`, `whoami`;
- create resource groups, tagged `yawble-owner: <team id>` and `yawble-plugin: azure`, and
  anything inside them: registries, SQL servers and databases, storage accounts, Key Vaults,
  Container Apps environments and apps, DNS zones, managed certificates;
- change resources carrying those tags, or inside a resource group that does;
- create a new database on a server in `sharedSqlServers` (tagged, and then the team's);
- build images in, and grant pull rights on, an Azure Container Registry in `sharedContainerRegistries`;
- run SQL on databases the team owns, and read and write data in storage accounts it owns;
- grant a short list of roles (Contributor, Storage Blob Data Reader, Storage Blob/Table/Queue
  Data Contributor, Key Vault Secrets Officer, Key Vault Secrets User, AcrPull) on what the team
  owns, only to managed identities the team created;
- delete what the team owns, only when `allowDelete` is on.

It **will not**:

- change, or create anything inside, a resource group or resource without the team's tags, other
  than the two shared exceptions above;
- deploy a template that would change anything outside what the team owns: every deployment is
  checked with Azure's what-if first, and one change outside refuses the whole deployment,
  naming each resource, including a resource the what-if marks as ignored;
- grant any role to a user or a group, or any other role, except to you, the signed-in person,
  read from your own sign-in: a storage account it creates gives you Storage Blob Data Reader and
  Storage Blob, Table and Queue Data Contributor on that account alone, and a storage account the
  team owns that a deployed diagnostic setting sends an environment's logs to gives you Storage
  Blob Data Reader there (a subscription Owner holds no data rights, and with shared key access off
  you would otherwise be refused your own blobs and logs);
- read a storage key, a connection string, a registry password or a Key Vault secret value;
  storage accounts are created with shared key access off, registries with their admin user off,
  and SQL servers with Microsoft Entra sign-in only, so no SQL password exists;
- run SQL on a database without the team's tags, even on a shared server where your account could;
- delete anything while `allowDelete` is off.

## Commands

One command per line, told to the member. The plugin-azure skill has each one in full.

```
setup
whoami
list [resource group]
show <resource id>
rg create <name> <region>
deploy <resource group> <template .bicep or .json> [parameters .json]
role assign <role> <managed identity id> <scope id>
role list <scope id>
dns zone create <resource group> <zone>
dns record set <zone> <name> <A|AAAA|CNAME|TXT|MX> <value...> [ttl=<seconds>]
dns record delete <zone> <name> <type>
delete <resource id or resource group>

acr create <resource group> <registry> [Basic|Standard|Premium]
acr build <registry> <image:tag> <folder>

sql server create <resource group> <server>
sql db create <server> <database> <sku>
sql exec <server>/<database> <file.sql>
sql grant <server>/<database> <managed identity id> <db_datareader|db_datawriter|db_ddladmin...>

storage create <resource group> <account> [sku]
storage container create <account> <container>
storage table create <account> <table>
storage queue create <account> <queue>
blob list <account> <container> [prefix]
blob get <account> <container> <blob> [path in the team's documents]
blob put <account> <container> <blob> <file>
blob delete <account> <container> <blob>
table query <account> <table> [filter]
table upsert <account> <table> <partition key> <row key> '<JSON properties>'
table delete <account> <table> <partition key> <row key>
queue send <account> <queue> <text>
queue peek <account> <queue> [count]
queue receive <account> <queue> [count]
queue delete <account> <queue> <message id> <pop receipt>

keyvault create <resource group> <vault>
keyvault secrets <vault>

containerapp env create <resource group> <environment>
containerapp create <resource group> <app> <environment> <registry>.azurecr.io/<image:tag> [port=<n>] [cpu=<0.25 to 2>] [env=<NAME>=<value>]... [secret=<NAME>=<Key Vault secret address>|appSecret<1-8>]...
containerapp show <app>
containerapp revisions <app>
containerapp restart <app>
containerapp logs <app> [minutes]
domain bind <app> <hostname>
```

A template, a build folder, a SQL file or a file to upload comes from the team's documents folder
or a worktree of the run; any other path is refused. A relative path is the documents folder's,
and an absolute one is taken as it is. `blob get` writes only in the documents folder, to a path
relative to it or absolute inside it.

An instruction holds commands only, one per line. A line that is not a command is refused in one
sentence naming its line, and the run fails, though the commands beside it still run; an
instruction holding no command at all is answered with the list of commands.

A Bicep file is compiled by a pinned release of the bicep compiler (v0.48.1). It is not inside
the package, which would then be over the Marketplace's 100 MB limit: the first Bicep deployment
downloads it once from the release on github.com into the member's working folder, about 120 MB,
and the member runs it only while its sha256 is the pinned one. ARM JSON templates need no
compiler.

For example, with `mode` at `whatif`:

```
rg create my-project-rg westeurope
deploy my-project-rg infra/main.bicep infra/main.parameters.json
```

answers what would be created and changes nothing; with `mode` at `apply`, the same lines create
the group and deploy exactly what the what-if checked. A deployment that fails answers each failed
operation's resource type, name, error code and message, at most 10 and then how many more.

`show` reads a resource with the api-version of the last resource type in its id, so an extension
resource such as a diagnostic setting is read as itself; a type with no version it knows or Azure
lists is refused, naming the type.

An app from start to finish, in apply, `<registry>` and `<vault>` standing for names of your own
that are unique in all of Azure:

```
rg create my-project-rg westeurope
acr create my-project-rg <registry>
acr build <registry> my-app:1.0 app
keyvault create my-project-rg <vault>
containerapp env create my-project-rg my-project-env
containerapp create my-project-rg my-app my-project-env <registry>.azurecr.io/my-app:1.0 port=8080 secret=API_KEY=https://<vault>.vault.azure.net/secrets/api-key
domain bind my-app www.example.com
```

- `acr build` uploads the folder `app` from the team's documents (leaving out `.git` and what its
  `.dockerignore` leaves out), builds it in the registry and answers the image's digest and the
  end of the build's log.
- `acr build` packs each file as 0644 (0755 when it has an execute bit) and each folder as 0755,
  owned by root, whatever the modes on disk.
- `containerapp create` makes a managed identity for the app and grants it AcrPull on the registry
  and Key Vault Secrets User on each vault a `secret=` names. The app pulls its image and reads its
  Key Vault secrets with that identity; no password is involved. Each secret must already be in the
  vault. It sets `AZURE_CLIENT_ID` to that identity's client id, unless the command gives its own
  `env=AZURE_CLIENT_ID=...`, so Azure's SDKs in the app sign in as it.
- `domain bind` answers the TXT and CNAME (or, for a domain's apex, A) records the domain needs. In
  a DNS zone the team owns it sets them itself. Once they resolve, `domain bind` again creates a
  free managed certificate, which Azure renews, and binds it.

`sql exec` runs a file's batches (split at `GO` lines) in order, stopping at the first that fails,
and answers the rows each changed, or each result capped at 200 rows. A UTF-8 byte-order mark at
the file's start is dropped. In whatif it says what it would run and does not connect.

`containerapp logs` reads the app's console lines from the environment's Log Analytics workspace,
the last 30 minutes unless told otherwise, at most 500 lines. It queries the workspace through
Azure management: the workspace the environment's diagnostic settings name, or, for an environment
that names only a workspace's customer id, the workspace in the subscription with that id. An
environment with no workspace whose diagnostic setting archives its logs to a storage account has
them read from that account instead: the hourly blobs of its `insights-logs-containerappconsolelogs`
and `insights-logs-containerappsystemlogs` containers covering the minutes asked, the app's lines
only, the platform's own lines marked `system:`. With neither, it is refused, saying so.

## App secrets

An app's own secrets (a third-party API key, a webhook signing secret) go in Key Vault, set by a
person in the Azure portal or by another plugin. They never go in a template, a parameters file, a
command or the repository: the member refuses a template that calls any list function (`list`,
`listKeys`, `listAuthKeys` and every other `list*`, in any string or object key of the template,
read after its JSON escapes are decoded, with any space, Unicode space or invisible character
before the bracket, so the words `list (` in a description count too), a
template that is not valid JSON, a parameters file that holds a secret's
value, and an `env=` whose name or value looks like a secret. A container app
points at the secret with `secret=<NAME>=https://<vault>.vault.azure.net/secrets/<name>`, and
reads it with its managed identity; `keyvault secrets <vault>` lists the names a vault holds,
never their values. The member never calls Key Vault itself: Azure reads the secret with the
app's identity.

An app's secret can also come from this member's own keys. The member has eight optional secret
fields, `appSecret1` to `appSecret8`. A person binds one to a key and sets that key's value on the
screen, in this member's settings, under the field (or with `yawble secret set <KEY>` and
`yawble up`). `containerapp create ... secret=<NAME>=appSecret<N>` then puts that value in the
app's own secrets, named after `<NAME>` in lower case with hyphens for underscores, and the app
reads it as the environment variable `<NAME>`. The value goes only to Azure, in the app's
configuration: no what-if, answer, event or progress line holds it, and every value the member is
given is redacted from what it writes. The what-if and the answer count these as "secrets from
this member's keys", apart from the Key Vault secrets. A field that is not one of the eight, one
the member does not bind, or one whose key is not set is refused before anything changes, naming
the field and the key, and saying a person sets it on the screen first.

## Events

- `plugin.azure.planned`: what a changing command would do, or what a deployment's what-if found,
  with counts by change type and any refusals.
- `plugin.azure.applied`: what changed in apply, with resource ids.
- `plugin.azure.resource`: one per Azure resource an apply created, changed or deleted, at most 25
  a command, with its id, kind, name, action and command. The Monitor shows each as a line with an
  icon for its kind (a folder for a resource group, a key for a vault or a secret), its action and
  its name; a data write such as a blob, a message or SQL shows only in `applied`.
- `plugin.azure.refused`: the command and why.

## Tests

```sh
go test ./...
```

runs every command against fakes of Azure Resource Manager, the registry, Azure SQL (a local TDS
server) and storage; nothing reaches a network, and a request to Key Vault or Log Analytics
fails the test.
