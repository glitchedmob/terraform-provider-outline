---
page_title: "Provision Outline users for OIDC"
description: |-
  Provision and retire Outline accounts that sign in through an external OIDC identity provider.
---

# Provision Outline users for OIDC

The provider manages Outline workspace accounts through the admin API. Your identity provider authenticates users, and Outline handles OIDC sign-in. Terraform does not perform an OIDC handshake, exchange tokens, set passwords, use magic-link authentication, or create API keys.

This guide targets [Outline v1.10.1, server commit `4a5a616a21be800257dc11cef4263d0dd0412156`](https://github.com/outline/outline/tree/4a5a616a21be800257dc11cef4263d0dd0412156). Compatibility with other releases is not claimed.

## Before provisioning

Configure OIDC on your Outline deployment separately, following the [Outline hosting documentation](https://docs.getoutline.com/s/hosting). Configure the IdP to supply the email that the account will use in Outline. Provisioning an email does not prove that its owner can authenticate through your IdP.

Use an unrestricted Outline API key owned by an active admin. Obtain that key outside Terraform and set `OUTLINE_API_KEY`; set `OUTLINE_BASE_URL` to your self-hosted API URL, including `/api`. Do not put the key in committed configuration. The user resource checks the caller and refuses any modifying action against the key owner's account, including deletion. Manage that account only with another admin's key.

Check whether the account already exists. The [user data source](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/data-sources/user) reads by UUID or exact normalized email, including pending and suspended users:

```hcl
data "outline_user" "existing" {
  email = "alice@example.com"
}
```

A lookup for a missing account returns an error, not an empty result. Import existing accounts by UUID rather than trying to recreate them. Create never adopts an account. The provider checks every page of the admin-visible user list and rejects `unsent` invitations returned by `users.invite`, including conflicts caused by a concurrent invitation.

## Provision without invitation email

```hcl
resource "outline_user" "alice" {
  email              = "alice@example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false
}
```

The resource's `id` is the UUID assigned by Outline. Use that UUID for import and later account references. This resource does not manage group memberships or transfer content.

`suspended` is required. Set it to `false` when the account should be allowed access, or `true` when it should remain suspended. The provider reconciles this explicit final state after invitation and role changes. An account can be active before its first SSO sign-in. That activation is not proof of a working OIDC login.

`suppress_email` defaults to `true`, so creation provisions the account without sending invitation email or relying on SMTP. It is creation-only; changing it later does not send or resend an invitation. Suppressing email does not disable or configure other sign-in methods on the server.

`email` is required. The provider sends a lowercase, normalized email in the invitation and preserves equivalent configured casing in Terraform state. The IdP and Outline must agree on the account email. Do not treat this resource as proof that a particular IdP will link an invitation to an SSO identity.

Omit `name` if the IdP should control the display name. It is optional and computed, with no default. Creation uses `Pending user` as the invitation name when you omit it. This avoids treating an email containing `www.` as a URL, which Outline rejects in display names. Configured names must contain 1 to 255 Unicode code points and must not contain a URL. After creation, an omitted name is unmanaged. Outline can update it at IdP sign-in. Add `name = "Alice Example"` only if Terraform should enforce that value and reapply it after sign-in changes it.

## Roles and suspended accounts

`role` defaults to `member`; supported values are `admin`, `member`, `viewer`, and `guest`. Tests exercise all four API roles against Outline 1.10.1. Editions, licenses, API-key scopes, and server policies may restrict available roles or reject a change. The provider reports those errors rather than treating a successful invitation as proof that the requested role took effect.

Outline 1.10.1 coerces a guest invitation to member. The provider reads the stored account and performs a follow-up role update when needed.

Changing the role of a suspended account temporarily activates it, changes the role, then restores the configured `suspended` value. This can briefly restore access. If a later write fails and the desired state is suspended, the provider makes a best-effort attempt to suspend it again. Check the account's actual state after an error; recovery can fail too.

Role defaults still apply to imported accounts. Configure `role` to match a non-member account before applying, or Terraform will plan to change it to member.

## Email changes and import

The provider does not implement Outline's email-change confirmation flow. Complete any rename outside Terraform. Any change to the configured `email` requires replacement, including a casing-only edit. Replacement does not rename the old account or transfer memberships or content. The default destroy policy leaves the old account suspended, then Terraform invites a separate account for the new email.

If you renamed an existing account outside Terraform and want to keep managing its UUID, back up state, update configuration to match the confirmed email, remove only that resource's state entry, then import the same UUID. Removing state does not suspend or delete the account. Do not apply between removing state and importing it.

```shell
terraform state pull > outline-state-backup.json
terraform state rm outline_user.alice
terraform import outline_user.alice 550e8400-e29b-41d4-a716-446655440000
terraform plan
```

Replace the UUID with the existing account's ID. Protect the state backup as sensitive data and keep it out of version control. Configure `email` and the required `suspended` value before import, and review role and name changes in the plan. Import initializes `suppress_email` to `true` and `delete_permanently` to `false`; it sends no invitation. Use another admin's key if the target owns your current API key.

## Failed-create recovery

An invitation can commit before a follow-up read, role update, name update, or suspension request fails. Once the provider receives a newly created account's matching UUID, it retains that UUID in state before the follow-up work. Terraform taints failed creates even when their UUID is retained.

Fix the reported error, inspect the account, then untaint the resource to reconcile it in place:

```shell
terraform untaint outline_user.alice
terraform plan
terraform apply
```

Do not accept replacement blindly. Default destruction suspends and retains the account, so the next Create refuses its existing email. If state was lost, or a failed invitation response did not provide a usable UUID, find the account in Outline and import its UUID before retrying.

## Retire or delete an account

For reversible offboarding, keep the resource and set `suspended = true`. To hand management back to an administrator or another system without suspending or deleting it, back up state, remove only that resource's state entry, and remove its configuration before the next apply. Running destroy would invoke the account's destroy policy; removing only state while keeping the configuration would plan another Create.

With the default `delete_permanently = false`, destroy suspends the user, retains the account, memberships, and content in Outline, and removes the resource from Terraform state. A later Create with the same email refuses to adopt that account. Import its UUID to manage or activate it again.

To opt in to `users.delete`, set `delete_permanently = true` and apply that policy while the resource is still in configuration. Then review the destroy plan. Removing the resource block first uses the last deletion policy stored in state.

Deletion is irreversible through this provider. Outline 1.10.1 anonymizes and soft-deletes the user. It does not guarantee a full purge of database rows or retained content and does not delete the user's documents. Use a separate retention process if you need content erasure.

Refresh does not treat authorization errors as proof of deletion. The release returns `403 authorization_error` for a nonexistent `users.info` target. The provider independently verifies an active admin and checks every page of `users.list` with the legacy `filter = all`, including suspended accounts. Only absence from that complete list permits removal from state. A bare 403, a listed UUID, or a failed check remains an error. An unexpected `users.info` 404 also requires the complete list check, since a missing route or proxy response does not prove the account was deleted.

## What the tests verify

Container acceptance tests use a disposable Outline 1.10.1 deployment and an admin API key. They test admin-side account provisioning without SMTP. They do not include a fake IdP or test an OIDC handshake, token exchange, first SSO login, or identity linking. Verify those flows with your actual Outline and IdP configuration before relying on them.

See the [user resource](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/resources/user) for the full schema and import behavior.
