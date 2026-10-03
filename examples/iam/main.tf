terraform {
  required_version = ">= 1.0"
  required_providers {
    outline = {
      source = "glitchedmob/outline"
    }
  }
}

# Set OUTLINE_API_KEY to an active admin's key, not Alice's.
# Set OUTLINE_BASE_URL to this workspace's API URL, including /api.
provider "outline" {
  timeout_seconds = 30
}

resource "outline_user" "alice" {
  email              = "alice@example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false
  # Omit name to leave it unmanaged after provisioning.
}

resource "outline_group" "engineering" {
  name             = "Engineering"
  description      = "Engineering team"
  disable_mentions = false
}

resource "outline_group_member" "alice" {
  group_id   = outline_group.engineering.id
  user_id    = outline_user.alice.id
  permission = "member"
}

resource "outline_collection" "engineering" {
  name          = "Engineering"
  description   = "Engineering documentation"
  permission    = null
  sharing       = false
  allow_destroy = false
}

resource "outline_collection_group" "engineering" {
  collection_id = outline_collection.engineering.id
  group_id      = outline_group.engineering.id
  permission    = "read_write"
}

# A direct read grant does not cap Alice's access through the group above.
# Outline also creates an unmanaged direct admin grant for the creator.
resource "outline_collection_user" "alice" {
  collection_id = outline_collection.engineering.id
  user_id       = outline_user.alice.id
  permission    = "read"
}

# These lookups read the managed objects after their IDs are available.
# For objects managed elsewhere, configure an existing UUID, email, or name.
data "outline_user" "alice" {
  id = outline_user.alice.id
}

data "outline_group" "engineering" {
  id = outline_group.engineering.id
}

data "outline_collection" "engineering" {
  id = outline_collection.engineering.id
}

output "iam" {
  value = {
    user_id                       = data.outline_user.alice.id
    user_role                     = data.outline_user.alice.role
    user_suspended                = data.outline_user.alice.suspended
    group_id                      = data.outline_group.engineering.id
    collection_id                 = data.outline_collection.engineering.id
    collection_default_permission = data.outline_collection.engineering.permission
    direct_permission             = outline_collection_user.alice.permission
    group_permission              = outline_collection_group.engineering.permission
  }
}
