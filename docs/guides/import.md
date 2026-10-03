---
page_title: "Import existing objects - Outline"
description: |-
  Adopt existing Outline collections, users, groups, and group memberships without recreating them.
---

# Import existing objects

Import adds an existing object to Terraform state. It does not create the object or make omitted configuration values unmanaged. Match the configuration to the existing object and review the next plan before applying. Example UUIDs are placeholders.

Use an unrestricted Outline API key owned by an active workspace admin. Set `OUTLINE_API_KEY` and, for a self-hosted instance, `OUTLINE_BASE_URL` including `/api`. Keep credentials and state backups out of version control. These contracts target Outline 1.10.1 only.

## Import a collection

Find the collection's stable UUID through the Outline API or the [collection data source](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/data-sources/collection). Do not use its short URL ID, full URL, or name as the import ID. UUIDs must be canonical lowercase and nonzero. Name lookup includes archives and rejects duplicate exact names, so use UUID lookup when names are ambiguous.

Archived collections cannot be imported into the resource. Restore them outside Terraform first. A collection with the server's `admin` default permission also cannot be imported. Change its default outside Terraform to `read`, `read_write`, or private, or use the data source without managing it.

Write a resource block matching the current values. For example, if the collection has read-only default access and allows public document sharing:

```hcl
resource "outline_collection" "engineering" {
  name          = "Engineering"
  description   = "Engineering documentation"
  permission    = "read"
  sharing       = true
  allow_destroy = false
}
```

Use the CLI to import its UUID, then inspect the plan:

```shell
terraform import outline_collection.engineering 550e8400-e29b-41d4-a716-446655440000
terraform plan
```

Or use a declarative import block with Terraform 1.5 or later, alongside the matching resource block:

```hcl
import {
  to = outline_collection.engineering
  id = "550e8400-e29b-41d4-a716-446655440000"
}
```

Review `terraform plan` before applying the import block. A declarative import can also apply planned configuration changes; it is not a promise of an import-only apply.

Collection defaults are enforced after import. Omitted or null `permission` means private and removes default workspace access, omitted `description` clears it, and omitted `sharing` disables public document sharing. Match these attributes explicitly to avoid an unwanted private reset or other changes. Import resets the local `allow_destroy` guard to false.

Import itself does not change grants. The resource does not manage direct user or group collection grants, and making a collection private leaves those grants in place. A later update from `read_write` to another default can create a caller-admin grant if the caller has no direct membership. Admin metadata access is not private-document access.

## Other import formats

Each resource page documents its full import behavior.

- [Users](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/resources/user) use the account UUID, not email. Match `email`, set the required `suspended` value, and configure the current role if it is not `member`. Omit `name` to leave it unmanaged or configure the name Terraform should enforce. Import initializes `suppress_email` to true and `delete_permanently` to false. Use another admin's key to manage the API-key owner's account.
- [Groups](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/resources/group) use the group UUID, not name or external identifier. Match `name`, `description`, and `disable_mentions`. Externally linked or synchronized groups cannot be imported.
- [Group memberships](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/resources/group_member) use `group_id/user_id` in that order, with exactly one slash and two UUIDs. Match both parent IDs. Set `permission = "admin"` for an admin membership, or the default reconciles it to `member`. This is not Outline's `userId-groupId` presenter ID.

```shell
terraform import outline_user.alice 8b58a9b0-043e-4b76-a0b7-a164674537cf
terraform import outline_group.engineering 550e8400-e29b-41d4-a716-446655440000
terraform import outline_group_member.alice 550e8400-e29b-41d4-a716-446655440000/8b58a9b0-043e-4b76-a0b7-a164674537cf
```

Import each object or membership into only one resource address and one Terraform state. Literal parent UUIDs do not establish dependencies. When Terraform manages the parents, reference their `id` attributes in membership configuration so Terraform orders creation and destruction correctly.

## Stop managing without deleting

Removing a resource block normally plans destruction, not a handoff. For a collection, the default guard blocks that destruction; applying `allow_destroy = true` permits destructive deletion of collection content.

To hand an object back to another system, back up state, remove only its state entry, and remove its configuration before the next apply. For a collection:

```shell
terraform state pull > outline-state-backup.json
terraform state rm outline_collection.engineering
```

Protect the backup as sensitive data. Keeping the resource block after removing state plans creation again. Collections permit duplicate names, so that plan can create a second collection rather than recover the existing one.
