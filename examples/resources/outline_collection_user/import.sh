# Replace both UUIDs, in collection_id/user_id order.
# Match both parent IDs and the required read, read_write, or admin permission.
# Use an active admin's key that is not owned by the target user.
# Import an automatic creator grant only if you intend to manage it.
terraform import outline_collection_user.alice 550e8400-e29b-41d4-a716-446655440000/8b58a9b0-043e-4b76-a0b7-a164674537cf
