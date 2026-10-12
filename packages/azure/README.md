# Azure 1.0.0

A plugin member that builds and runs a team's own Azure resources: resource groups, deployments
from Bicep or ARM templates, DNS zones and records, budgets, and role assignments for the managed
identities it creates. You sign in with Microsoft once; the member then acts **with your own Azure
rights**, so it can never do more than you can yourself in the Azure portal.

Everything starts in **whatif**: each changing command says what it would do and changes nothing,
until you set `mode` to `apply`.

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
   Azure portal, and accept. Microsoft asks once for all five APIs the member uses: Azure
   management, Azure SQL, Azure Storage, Key Vault and Log Analytics.
3. **Tell the member `setup`.** With no subscription set yet, it lists every subscription your
   account can read, by id and name.
4. **Settings.** In the member's settings, put the subscription's id in `subscription` and the
   regions you want resource groups in under `regions`. Everything else has a default (below).
5. **Tell the member `setup` again.** It checks each thing and says the next step in words:
   - you are signed in, with a token for each API;
   - the subscription can be read;
   - your role allows creating resource groups (if not, ask the subscription's owner for
     Contributor);
   - each region in `regions` is offered to the subscription;
   - each SQL server in `sharedSqlServers` and registry in `sharedRegistries` can be reached;
   - the resource providers it uses (Microsoft.App, Microsoft.OperationalInsights,
     Microsoft.Storage, Microsoft.KeyVault, Microsoft.Network, Microsoft.ContainerRegistry,
     Microsoft.Sql, Microsoft.ManagedIdentity and Microsoft.Consumption) are registered. A
     provider is Azure's switch for one kind of resource. In apply, setup registers any that are
     missing when your role allows it; in whatif it says it would.

   It ends with `Ready:` or with `Next:` and numbered steps.
6. When the team has a plan you agree with, set `mode` to `apply`, and set it back to `whatif`
   when the work is done.

## Settings

| Setting | Default | What it does |
|---|---|---|
| `subscription` | empty | The id of the Azure subscription this member works in; `setup` lists the ones your account can read. |
| `regions` | empty | The Azure regions resource groups may be created in, such as `westeurope`. |
| `mode` | `whatif` | `whatif` answers what each changing command would do and changes nothing; `apply` makes the changes. |
| `allowDelete` | off | Off: every delete is refused, in whatif and apply; on: the member may delete what its team owns. |
| `sharedSqlServers` | empty | Existing SQL servers, by name, on which the member may create its own databases. |
| `sharedRegistries` | empty | Existing container registries, by name, it may push images to and grant pull rights on. |
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

## What it will and will not touch

It **will**:

- read anything your account can read: `list`, `show`, `whoami`;
- create resource groups, tagged `yawble-owner: <team id>` and `yawble-plugin: azure`, and
  anything inside them;
- change resources carrying those tags, or inside a resource group that does;
- create a new database on a server in `sharedSqlServers` (tagged, and then the team's);
- push images to, and grant pull rights on, a registry in `sharedRegistries`;
- grant a short list of roles (Contributor, Storage Blob/Table/Queue Data Contributor, Key Vault
  Secrets Officer, Key Vault Secrets User, AcrPull) on what the team owns, only to managed
  identities the team created;
- delete what the team owns, only when `allowDelete` is on.

It **will not**:

- change, or create anything inside, a resource group or resource without the team's tags, other
  than the two shared exceptions above;
- deploy a template that would change anything outside what the team owns: every deployment is
  checked with Azure's what-if first, and one change outside refuses the whole deployment,
  naming each resource;
- grant any role to a user or a group, or any other role;
- read a storage key, a connection string or a Key Vault secret value; storage accounts are
  created with shared key access off;
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
```

A template comes from the team's documents folder or a worktree of the run; any other path is
refused. A Bicep file is compiled by the bicep compiler that ships inside the plugin (a pinned
release, checked by its sha256 when the package is built).

For example, with `mode` at `whatif`:

```
rg create my-project-rg westeurope
deploy my-project-rg infra/main.bicep infra/main.parameters.json
```

answers what would be created and changes nothing; with `mode` at `apply`, the same lines create
the group and deploy exactly what the what-if checked.

## App secrets

An app's own secrets (a third-party API key, a webhook signing secret) go in Key Vault, set by a
person in the Azure portal or by another plugin. They never go in a template, a parameters file or
the repository; the member refuses a template that reads keys and a parameters file that holds a
secret's value. A template points at the secret with a Key Vault reference, and the app reads it
with its managed identity.

## Events

- `plugin.azure.planned`: what a changing command would do, or what a deployment's what-if found,
  with counts by change type and any refusals.
- `plugin.azure.applied`: what changed in apply, with resource ids.
- `plugin.azure.refused`: the command and why.

## Tests

```sh
go test ./...
```

runs every command against a fake Azure (`httptest`); nothing reaches a network.
