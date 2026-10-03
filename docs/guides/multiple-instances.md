---
page_title: "Manage multiple Outline instances"
description: |-
  Use explicit provider aliases with separate API URLs and sensitive API-key variables.
---

# Manage multiple Outline instances

Use one provider alias per Outline instance or workspace. Set each alias's API URL and API key explicitly instead of relying on shared `OUTLINE_API_KEY` or `OUTLINE_BASE_URL` values. Each URL includes `/api`; each key must belong to an active admin in that workspace.

The [complete example](https://github.com/glitchedmob/terraform-provider-outline/tree/main/examples/multiple_instances) manages all six resources and reads all three data sources in each instance. Every scoped block uses an explicit alias. A smaller configuration follows.

## Configure separate clients

```hcl
terraform {
  required_providers {
    outline = {
      source = "glitchedmob/outline"
    }
  }
}

variable "eu_base_url" {
  type    = string
  default = "https://outline-eu.example.com/api"
}

variable "us_base_url" {
  type    = string
  default = "https://outline-us.example.com/api"
}

variable "eu_api_key" {
  type      = string
  sensitive = true
}

variable "us_api_key" {
  type      = string
  sensitive = true
}

provider "outline" {
  alias    = "eu"
  base_url = var.eu_base_url
  api_key  = var.eu_api_key
}

provider "outline" {
  alias    = "us"
  base_url = var.us_base_url
  api_key  = var.us_api_key
}

resource "outline_collection" "eu" {
  provider      = outline.eu
  name          = "Engineering"
  permission    = null
  sharing       = false
  allow_destroy = false
}

resource "outline_collection" "us" {
  provider      = outline.us
  name          = "Engineering"
  permission    = null
  sharing       = false
  allow_destroy = false
}

data "outline_collection" "eu" {
  provider = outline.eu
  id       = outline_collection.eu.id
}

data "outline_collection" "us" {
  provider = outline.us
  id       = outline_collection.us.id
}
```

Replace both example URLs before applying. Supply the required sensitive variables through your secret store or environment, not committed `tfvars` files. Bash can prompt without adding keys to shell history:

```shell
read -r -s -p "EU Outline admin API key: " TF_VAR_eu_api_key
printf '\n'
export TF_VAR_eu_api_key
read -r -s -p "US Outline admin API key: " TF_VAR_us_api_key
printf '\n'
export TF_VAR_us_api_key
```

Terraform's sensitive flag hides values in normal output. It does not encrypt configuration, plans, state, backups, or logs. Protect those artifacts and obtain keys outside Terraform. The provider does not manage API keys.

## Scope every resource and lookup

Always set `provider = outline.eu` or `provider = outline.us` on scoped resource and data blocks. An omitted provider can select an implicit default configuration and fall back to ambient environment credentials. Identical collection or group names in two workspaces do not establish shared identity.

Keep membership and grant parents in the same instance. For example:

```hcl
data "outline_user" "eu_existing" {
  provider = outline.eu
  email    = "alice@eu.example.com"
}

resource "outline_collection_user" "eu_alice" {
  provider      = outline.eu
  collection_id = outline_collection.eu.id
  user_id       = data.outline_user.eu_existing.id
  permission    = "read"
}
```

The EU key owner must not be Alice. The direct grant resource refuses its caller's pair during every operation, including read and import. The collection creator's automatic direct admin grant is not silently adopted. To manage it intentionally, select a different active admin's key and import the pair.

Provider aliases route requests; they do not translate UUIDs between instances. Use the matching instance's lookups and IDs. Admin metadata access does not grant private-document access, and explicit grants do not summarize effective roles, collection defaults, or group access.

## Import and retain aliases

Import selects the provider configured on the target resource. Configure the target IDs and existing required permission first, then import the pair from the matching instance:

```shell
terraform import outline_collection_user.eu_alice 550e8400-e29b-41d4-a716-446655440000/8b58a9b0-043e-4b76-a0b7-a164674537cf
terraform plan
```

The placeholder pair must exist in the EU workspace, and its user must not own the EU key. Import reads the grant without changing it. Review the next plan before applying. See the [import guide](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/guides/import) for all six resource formats.

Keep each provider alias while its resources remain in state, including while destroying them. Collection deletion remains blocked until `allow_destroy = true` has been applied. To stop managing without deleting, remove the state entries and their configuration rather than running destroy.

The contracts target Outline 1.10.1 only. These examples do not configure OIDC or test an OIDC handshake, IdP login, or identity linking. The intended provider source address is not published to the Terraform Registry yet.
