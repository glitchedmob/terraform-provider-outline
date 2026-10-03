resource "outline_collection" "engineering" {
  name          = "Engineering"
  description   = "Engineering documentation"
  permission    = null
  sharing       = false
  allow_destroy = false
}

# The target must not own the API key used by this provider.
resource "outline_user" "alice" {
  email              = "alice@example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false
}

# This is a direct grant, separate from collection defaults and group grants.
# The creator's automatic admin grant remains outside this resource.
resource "outline_collection_user" "alice" {
  collection_id = outline_collection.engineering.id
  user_id       = outline_user.alice.id
  permission    = "read"
}
