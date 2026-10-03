# Workspace IAM example

This configuration connects all six managed resources and reads the resulting user, group, and collection through the three data sources. It provisions Alice without invitation email, adds her to a manual group, creates a private collection, and assigns separate group and direct user grants.

Use an unrestricted key owned by an active workspace admin who is not Alice. Set `OUTLINE_API_KEY` and `OUTLINE_BASE_URL` outside committed configuration. Change the example email and names before applying. Existing accounts and grants require import, not creation. Collections allow duplicate names.

The direct grant is `read`, while the group grant is `read_write`. These are stored permissions, not a report of effective access. Alice can still receive write access through the group. Outline also creates an unavoidable direct admin grant for the collection creator. This example neither adopts nor manages it. To manage it intentionally, use another active admin's key and import its collection/user pair.

Parent ID references order creation and destruction. The collection's `allow_destroy = false` guard blocks collection deletion. Apply `allow_destroy = true` before an intentional destructive deletion, or remove state and configuration to stop managing without deleting. Destroying the grant resources removes only their pairs; destroying the user suspends it by default and retains the account.

The lookups use managed UUIDs. To look up objects Terraform should not manage, use existing UUIDs, an exact normalized user email, or exact group/collection names instead. Missing or ambiguous lookups are errors. Lookups do not report effective collection access.

These examples target Outline 1.10.1. Provisioning does not test an OIDC handshake, IdP login, or identity linking. This provider does not manage documents, document grants, or API keys.

The provider is not published to the Terraform Registry yet. Use a local development build; the source address is the intended Registry address. Documentation validation does not execute this example.
