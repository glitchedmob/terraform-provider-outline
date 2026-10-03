data "outline_user" "by_email" {
  email = "alice@example.com"
}

# Replace this placeholder UUID with an existing user's ID.
data "outline_user" "by_id" {
  id = "550e8400-e29b-41d4-a716-446655440000"
}
