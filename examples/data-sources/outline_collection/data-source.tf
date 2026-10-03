data "outline_collection" "by_name" {
  name = "Engineering"
}

# Replace this placeholder UUID with an existing collection's ID.
# UUID lookup avoids ambiguity when active or archived names are duplicated.
data "outline_collection" "by_id" {
  id = "550e8400-e29b-41d4-a716-446655440000"
}
