resource "outline_collection" "engineering" {
  name          = "Engineering"
  description   = "Engineering documentation"
  permission    = null
  sharing       = false
  allow_destroy = false
}

resource "outline_group" "engineering" {
  name             = "Engineering"
  description      = "Engineering team"
  disable_mentions = false
}

# Manage only this group's explicit grant, not the collection's default access.
resource "outline_collection_group" "engineering" {
  collection_id = outline_collection.engineering.id
  group_id      = outline_group.engineering.id
  permission    = "read_write"
}
