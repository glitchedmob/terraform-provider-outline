# Outline API generation

## Pinned sources

`outline.openapi.json` is the unmodified `spec3.json` from [outline/openapi at `40f51b75efad84e3e860af3e1c8b5a58fd74bb48`](https://github.com/outline/openapi/tree/40f51b75efad84e3e860af3e1c8b5a58fd74bb48). The upstream commit is dated 2026-09-23. The file's SHA-256 is `71c211078aff5b950df1b478aed628a242df26fe94c3b5f055fef9b1d46f2731`. `LICENSE` is the original BSD-3-Clause license from that commit. This specification and code generated from it retain that license; provider code and the local overlay use the repository's MPL-2.0 license. Release archives include the upstream notice as `OUTLINE-OPENAPI-LICENSE`.

The IAM contracts target [Outline v1.10.1, commit `4a5a616a21be800257dc11cef4263d0dd0412156`](https://github.com/outline/outline/tree/4a5a616a21be800257dc11cef4263d0dd0412156) only. Compatibility with other releases is not claimed. Do not verify collection changes against `main`; its description and archive/delete APIs have changed since this release.

`oapi-codegen.yaml` selects 29 IAM operations:

- `auth.info`
- Users list, info, invite, update, update_role, suspend, activate, delete
- Groups list, info, create, update, delete, memberships, add_user, remove_user, update_user
- Collections list, info, create, update, delete, memberships, add_user, remove_user, group_memberships, add_group, remove_group

The original full specification is committed, but we generate only these operations and their model dependencies. The provider registers the `outline_user` and `outline_group` resources and data sources; collection and membership operations do not yet have Terraform resources. The new `usersUpdate` selection supports display-name changes using an explicit target UUID. API key creation/deletion require application sessions and are not generated. Passwords, token exchange, and magic-link authentication are outside this provider.

## Generation and drift checks

Run from the repository root:

```shell
make generate
make check-generated
```

`make generate` uses `go tool oapi-codegen` v2.8.0, pinned in `go.mod`, to apply `outline.overlay.yaml` with strict target matching and generate models and client methods in `internal/client/client.gen.go`. Nullable fields use `github.com/oapi-codegen/nullable` v1.2.0; the client runtime is v1.7.0. Generation reads only committed local specifications and overlay files. It never downloads a live specification. Go may download pinned tool dependencies on the first run.

Commit source, overlay, config, and generated output together. `make check-generated` regenerates and rejects tracked changes and untracked files under `internal/client/`. The Generated Client workflow runs this on pull requests and pushes to `main`. Unit tests also compare generation into a temporary file byte-for-byte and verify the upstream source checksum. When deliberately updating upstream, update the pin, checksum test, license, and this document after rechecking the server contracts.

## Release-verified corrections

All paths below are relative to the pinned [Outline server tree](https://github.com/outline/outline/tree/4a5a616a21be800257dc11cef4263d0dd0412156).

| Correction | Evidence |
| --- | --- |
| Add `admin` to collection/document `Permission`; give role enums stable Go names. Template management accepts only `admin` or `read_write`. | `shared/types.ts`; `server/routes/api/collections/schema.ts` |
| Separate integer request pagination from response metadata, adding `total` and `nextPath`. Limits are 1 through 100; offsets start at 0. | `shared/constants.ts`; `server/routes/api/middlewares/pagination.ts`; users/groups/collections handlers |
| Model `ok` and integer `status` on the selected success envelopes, keeping endpoint-specific `success` fields. Error status and group member counts are integers too. | `server/routes/api/middlewares/apiResponse.ts`; `server/presenters/group.ts` |
| Require invitation name, email, and role; model `unsent` alongside `sent` and newly created `users`. Nullable user avatar/email reflect the presenter. | `server/routes/api/users/schema.ts`; `server/routes/api/users/users.ts`; `server/commands/userInviter.ts`; `server/presenters/user.ts` |
| Add UUID `id` to `users.update` and require it locally to prevent updating the API-key owner by omission. Include `users.update` in the corrected `ok`/integer `status` success envelopes. | `server/routes/api/users/schema.ts`; `server/routes/api/users/users.ts`; `server/routes/api/middlewares/apiResponse.ts` |
| Add group `externalId` and `disableMentions` on create/update. Remove create description, which the release handler validates but drops. Add non-null update description and make update name optional. Search queries and external IDs are strings, not UUIDs. Add list name/source filters. | `server/routes/api/groups/schema.ts`; `server/routes/api/groups/groups.ts` |
| Model group users separately as `GroupUser` with userId, groupId, composite string id, nested user, and `member`/`admin` group permission. Fix group list/membership/add/update response item types and missing permission parameters. Group membership request id is a UUID. | `server/presenters/groupUser.ts`; `server/routes/api/groups/groups.ts`; `server/routes/api/groups/schema.ts` |
| Collection add_group/group_memberships return `data.groupMemberships`, not `collectionGroupMemberships`. These remain `GroupMembership`, distinct from `GroupUser`. | `server/routes/api/collections/collections.ts`; `server/presenters/groupMembership.ts` |
| Model nullable default collection permission so an update can remove default access. Add sort, commenting, template management, create index, and supported nullable description/data/color/icon fields. Share one CollectionSort schema to avoid duplicate generated names. | `server/routes/api/collections/schema.ts`; `server/presenters/collection.ts` |
| Add collection list `includeListOnly`; remove unsupported Sorting parameters. Remove update deprecatedReason, delete reason, and the collection deprecatedReason response field, which belong to later server versions. | `server/routes/api/collections/schema.ts`; `server/routes/api/collections/collections.ts`; `server/presenters/collection.ts` |

`users.invite.suppressEmail` is already in this upstream pin and works in v1.10.1. No overlay addition is needed. We retain upstream's required IDs on info/delete calls for explicit resource targeting, even where the server allows a self-user default or external group lookup. The release's `users.update` target ID is optional and defaults to the caller; the overlay deliberately requires it in the generated request. This is a local safety constraint, not a claim that the server requires it. Generated types describe wire fields, not every server validation rule.

## Calling conventions for resource authors

`internal/provider/apiClient` embeds `*client.ClientWithResponses` and uses the existing origin-restricted bearer transport, provider user agent, timeout, and redirect rejection. Provider Configure only builds this client; it does not call `auth.info` or any other endpoint.

Import `github.com/glitchedmob/terraform-provider-outline/internal/client`. Each RPC is a POST:

```go
response, err := api.UsersInfoWithResponse(ctx, client.UsersInfoJSONRequestBody{Id: userID})
```

`userID` is `github.com/google/uuid.UUID`, also aliased as `github.com/oapi-codegen/runtime/types.UUID`. Most ID fields use that type. GroupUser's returned id is a composite string, not a UUID. Required body fields are values; optional fields are pointers. Use the generated `...JSONRequestBody` type for calls, not a parallel handwritten request struct. Auth info has no body: `api.AuthInfoWithResponse(ctx)`.

Response wrappers expose `StatusCode()`, `HTTPResponse`, raw `Body`, `JSON200`, and documented `JSON400`/`JSON401`/`JSON403`/`JSON404`/`JSON429` variants. HTTP errors do not automatically become Go errors. Check status, the selected JSON variant, and required nested data before using it. Optional response fields stay pointers so missing data is detectable; decoding does not validate required fields or enum values. Use `.Valid()` on enums when validating input. Unknown statuses/content types retain raw Body. Read `Retry-After` from HTTPResponse on 429 responses. Generated wrappers do not retry requests or produce Terraform diagnostics. The group and user resources and data sources add response validation and diagnostics in provider helpers; reuse those checks rather than handling only transport errors.

Key models are `User`, `Invite`, `Group`, `GroupUser`, `Collection`, `Membership`, `GroupMembership`, `PaginationResponse`, and `Auth`. Use `PermissionRead`, `PermissionReadWrite`, `PermissionAdmin` for collection grants; `GroupPermissionMember`/`GroupPermissionAdmin` for group membership; `UserRoleAdmin`/`UserRoleMember`/`UserRoleViewer`/`UserRoleGuest` for users. Collection defaults and direct grants are separate permissions.

Nullable fields have three states. A zero `nullable.Nullable[T]` omits the field, `nullable.NewNullNullable[T]()` writes JSON null, and `nullable.NewNullableWithValue(value)` writes a value. In particular, `CollectionsUpdateJSONRequestBody.Permission` must send null to make a collection private; omitting it preserves the current default. Direct user/group grants use non-null `*Permission`.

List bodies use `*int` Limit/Offset. Responses return PaginationResponse with total and nextPath. The release middleware produces nextPath even for the last page. Advance offset using the returned page metadata and stop against total; do not rely on nextPath becoming empty or follow it through a different HTTP client. `checkResponse` and `checkEnvelope` validate status and success envelopes, redact the configured key from API diagnostics, and never retry writes. `nextOffset` rejects malformed and non-progressing pagination. The bearer transport waits on a context-aware five-requests-per-second limiter.

`walkGroups` rejects duplicate IDs and changing totals. It uses the release's default `updatedAt` ordering; concurrent changes can produce a diagnostic requiring a new lookup rather than an incomplete result. Group string validators count UTF-16 code units to match the release's Zod validators, including supplementary Unicode characters. The group data source checks all pages for exact, case-sensitive name matches and rejects ambiguous results. Group list memberships are only a preview. Use paginated `GroupsMembershipsWithResponse` for the full membership set. Admin `includeListOnly` can expose otherwise inaccessible collections but does not grant access to their contents.

In v1.10.1, omitting `users.list.filter` excludes suspended users, even for admins. Explicit `filter = all` includes pending, active, and suspended users for admins. `walkUsers` explicitly requests `filter = all`, orders by `createdAt ASC`, validates each returned user, and rejects duplicate IDs and changing totals. User email lookups compare exact lowercase, normalized emails across the complete list and reject ambiguity. Do not substitute an active-only list when checking email conflicts or proving absence.

Do not send X-Client-Version, which can change pagination totals, or X-Api-Version, which switches collection descriptions to rich-text presentation. The provider's default calls preserve the release's markdown description presentation.

## IAM behavior and limits

Use an unrestricted admin-owned Outline API key for these IAM operations. User operations verify that the caller is an active admin; all user modifying actions reject the key owner's UUID or normalized email before a write. Endpoint-scoped keys and server policies can still reject calls. OIDC provisioning uses invitations with `suppressEmail` and separate SSO configuration, not passwords or magic-link sign-in.

The release inviter ignores existing accounts, reports them in `unsent`, and returns only new users. Create checks the full admin user list first and still rejects `unsent` to handle races. Existing users are never adopted or modified during Create; import their UUID instead. Guest invitations become member invitations in v1.10.1, so the resource reads the stored role and reconciles it. The supported API roles `admin`, `member`, `viewer`, and `guest` are tested on this release. Editions and licenses may restrict roles; the provider reports server errors or a response that fails to confirm the intended role.

The user resource requires an explicit final `suspended` boolean. A role change on a suspended account temporarily activates it, changes the role, then restores the requested final state. If a follow-up write fails and that state is suspended, it makes a best-effort attempt to suspend again. Activation is not a sign-in or proof that an IdP account can authenticate.

User creation retains a newly returned matching UUID in state before follow-up validation, reads, and writes. Terraform taints failed creates. After fixing the error, untaint to reconcile in place; if state was lost, inspect the workspace and import the account's UUID. Default destruction only suspends the account, retains its memberships and content, and removes Terraform state. Recreating the same email refuses the retained account and requires import. Opt-in `delete_permanently` calls `users.delete`, which is irreversible through this provider. The released server anonymizes and soft-deletes users; this does not guarantee a full purge or delete their documents.

User email is required and immutable. Invitations use lowercase, normalized email while state preserves equivalent configured casing. Configuration changes require replacement, not renaming; Outline's confirmation flow must run outside the provider. The optional computed name has no default. Omission uses `Pending user` on creation and leaves the name unmanaged afterward. The released User ORM validates name length by Unicode code points, unlike group Zod limits, and rejects names containing its `NotContainsUrl` pattern. Configured names are reapplied if IdP sign-in changes them. Role defaults to member, `suppress_email` defaults to true and is creation-only, and `delete_permanently` defaults to false. See the [OIDC guide](../docs/guides/oidc.md) for replacement, import, and recovery procedures.

Group description requires an update after creation in this release; the group resource performs that follow-up update. It persists the returned ID before the second write. A failed create still taints the resource in Terraform; untaint after fixing the error to recover with an in-place update. Group update cannot clear externalId with null, and externally synchronized groups reject manual name/membership changes. The resource rejects externally synchronized groups and exposes no external_id argument. The data source can read an externally synchronized group by UUID. Membership additions are upserts; updates need explicit intended permissions. Collection creation defaults sharing to true and templateManagement to admin at the server, so future resources should set their intended values explicitly. In v1.10.1, updates to a read_write collection can create an admin membership for the caller even when permission is omitted. The handler compares permission to read_write before checking whether the field was supplied.

The group resource implements UUID import, in-place updates, refresh, and deletion for manually maintained groups. The v1.10.1 `groups.info` handler authorizes a nil model, producing `403 authorization_error` for nonexistent IDs. `readGroup` does not equate 403 with absence. It requires `auth.info` to confirm an admin and `walkGroups` to complete an unfiltered workspace list with consistent pagination totals. A listed ID, failed check, or malformed page preserves the error and state. Its data source requires exactly one configured UUID or name and returns description and mention settings.

The missing-user contract was independently verified against v1.10.1. `users.info` also authorizes a nil model and returns `403 authorization_error` for a nonexistent UUID. `readUser` only treats this response as absence after verifying an active admin and completing the full `filter = all` workspace user list, including suspended accounts. A bare 403 is never absence. An unexpected `users.info` 404 also requires the complete list check, since a missing route or proxy response does not prove the account was deleted. A listed UUID or failed verification preserves the error and resource state. The user data source requires exactly one UUID or email and returns the current name, role, and suspension state. Future resources must verify their own missing-object contracts rather than copy either fallback blindly. Collection and membership ownership policies remain work for later resources.

Contract and provider unit tests use local HTTP fixtures. `make testacc` exercises group behavior and user admin API provisioning against a disposable Docker stack pinned to Outline 1.10.1. The test-only bootstrap uses that released image's ORM hooks to create an admin and unrestricted API key in an empty database; it does not use ambient Outline credentials or add API-key endpoints to the generated client. User provisioning tests run without SMTP. They include no fake IdP and make no OIDC-handshake, token-exchange, first-sign-in, or identity-linking acceptance claim. See [acceptance test setup](../README.md#acceptance-tests) for requirements, isolation, and cleanup.
