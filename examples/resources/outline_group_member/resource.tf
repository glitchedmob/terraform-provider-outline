# Replace these UUIDs with the existing group and user IDs.
resource "outline_group_member" "alice" {
  group_id   = "550e8400-e29b-41d4-a716-446655440000"
  user_id    = "8b58a9b0-043e-4b76-a0b7-a164674537cf"
  permission = "member"
}
