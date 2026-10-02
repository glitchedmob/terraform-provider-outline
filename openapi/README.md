# Outline API generation

## Pinned sources

`outline.openapi.json` is the unmodified `spec3.json` from [outline/openapi at `40f51b75efad84e3e860af3e1c8b5a58fd74bb48`](https://github.com/outline/openapi/tree/40f51b75efad84e3e860af3e1c8b5a58fd74bb48). The upstream commit is dated 2026-09-23. The file's SHA-256 is `71c211078aff5b950df1b478aed628a242df26fe94c3b5f055fef9b1d46f2731`. `LICENSE` is the original BSD-3-Clause license from that commit. This specification and code generated from it retain that license; provider code and the local overlay use the repository's MPL-2.0 license. Release archives include the upstream notice as `OUTLINE-OPENAPI-LICENSE`.

The IAM contracts target [Outline v1.10.1, commit `4a5a616a21be800257dc11cef4263d0dd0412156`](https://github.com/outline/outline/tree/4a5a616a21be800257dc11cef4263d0dd0412156). Do not verify collection changes against `main`; its description and archive/delete APIs have changed since this release.

`oapi-codegen.yaml` selects 28 operations for the initial IAM work:

- `auth.info`
- Users list, info, invite, update_role, suspend, activate, delete
- Groups list, info, create, update, delete, memberships, add_user, remove_user, update_user
- Collections list, info, create, update, delete, memberships, add_user, remove_user, group_memberships, add_group, remove_group

The original full specification is committed, but we generate only these operations and their model dependencies. No resources or data sources are registered yet. API key creation/deletion require application sessions and are not generated.

## Generation and drift checks

Run from the repository root:

```shell
make generate
make check-generated
```

`make generate` uses `go tool oapi-codegen` v2.8.0, pinned in `go.mod`, to apply `outline.overlay.yaml` with strict target matching and generate models **and** client methods in `internal/client/client.gen.go`. Nullable fields use `github.com/oapi-codegen/nullable` v1.2.0; the client runtime is v1.7.0. Generation reads only committed local specifications and overlay files. It never downloads a live specification. Go may download pinned tool dependencies on the first run.

Commit source, overlay, config, and generated output together. `make check-generated` regenerates and rejects tracked changes and untracked files under `internal/client/`. The Generated Client workflow runs this on pull requests and pushes to `main`. Unit tests also compare generation into a temporary file byte-for-byte and verify the upstream source checksum. When deliberately updating upstream, update the pin, checksum test, license, and this document after rechecking the server contracts.

## Release-verified corrections

All paths below are relative to the pinned [Outline server tree](https://github.com/outline/outline/tree/4a5a616a21be800257dc11cef4263d0dd0412156).

| Correction | Evidence |
| --- | --- |
| Add `admin` to collection/document `Permission`; give role enums stable Go names. Template management accepts only `admin` or `read_write`. | `shared/types.ts`; `server/routes/api/collections/schema.ts` |
| Separate integer request pagination from response metadata, adding `total` and `nextPath`. Limits are 1 through 100; offsets start at 0. | `shared/constants.ts`; `server/routes/api/middlewares/pagination.ts`; users/groups/collections handlers |
| Model `ok` and integer `status` on the selected success envelopes, keeping endpoint-specific `success` fields. Error status and group member counts are integers too. | `server/routes/api/middlewares/apiResponse.ts`; `server/presenters/group.ts` |
| Require invitation name, email, and role; model `unsent` alongside `sent` and newly created `users`. Nullable user avatar/email reflect the presenter. | `server/routes/api/users/schema.ts`; `server/routes/api/users/users.ts`; `server/commands/userInviter.ts`; `server/presenters/user.ts` |
| Add group `externalId` and `disableMentions` on create/update. Remove create description, which the release handler validates but drops. Add non-null update description and make update name optional. Search queries and external IDs are strings, not UUIDs. Add list name/source filters. | `server/routes/api/groups/schema.ts`; `server/routes/api/groups/groups.ts` |
| Model group users separately as `GroupUser` with userId, groupId, composite string id, nested user, and `member`/`admin` group permission. Fix group list/membership/add/update response item types and missing permission parameters. Group membership request id is a UUID. | `server/presenters/groupUser.ts`; `server/routes/api/groups/groups.ts`; `server/routes/api/groups/schema.ts` |
| Collection add_group/group_memberships return `data.groupMemberships`, not `collectionGroupMemberships`. These remain `GroupMembership`, distinct from `GroupUser`. | `server/routes/api/collections/collections.ts`; `server/presenters/groupMembership.ts` |
| Model nullable default collection permission so an update can remove default access. Add sort, commenting, template management, create index, and supported nullable description/data/color/icon fields. Share one CollectionSort schema to avoid duplicate generated names. | `server/routes/api/collections/schema.ts`; `server/presenters/collection.ts` |
| Add collection list `includeListOnly`; remove unsupported Sorting parameters. Remove update deprecatedReason, delete reason, and the collection deprecatedReason response field, which belong to later server versions. | `server/routes/api/collections/schema.ts`; `server/routes/api/collections/collections.ts`; `server/presenters/collection.ts` |

`users.invite.suppressEmail` is already in this upstream pin and works in v1.10.1. No overlay addition is needed. We retain upstream's required IDs on info/delete calls for explicit resource targeting, even where the server allows a self-user default or external group lookup. Generated types describe wire fields, not every server validation rule.

## Calling conventions for resource authors

`internal/provider/apiClient` embeds `*client.ClientWithResponses` and uses the existing origin-restricted bearer transport, provider user agent, timeout, and redirect rejection. Provider Configure only builds this client; it does not call `auth.info` or any other endpoint.

Import `github.com/glitchedmob/terraform-provider-outline/internal/client`. Each RPC is a POST:

```go
response, err := api.UsersInfoWithResponse(ctx, client.UsersInfoJSONRequestBody{Id: userID})
```

`userID` is `github.com/google/uuid.UUID`, also aliased as `github.com/oapi-codegen/runtime/types.UUID`. Most ID fields use that type. GroupUser's returned id is a composite string, not a UUID. Required body fields are values; optional fields are pointers. Use the generated `...JSONRequestBody` type for calls, not a parallel handwritten request struct. Auth info has no body: `api.AuthInfoWithResponse(ctx)`.

Response wrappers expose `StatusCode()`, `HTTPResponse`, raw `Body`, `JSON200`, and documented `JSON400`/`JSON401`/`JSON403`/`JSON404`/`JSON429` variants. HTTP errors do not automatically become Go errors. Check status, the selected JSON variant, and required nested data before using it. Optional response fields stay pointers so missing data is detectable; decoding does not validate required fields or enum values. Use `.Valid()` on enums when validating input. Unknown statuses/content types retain raw Body. Read `Retry-After` from HTTPResponse on 429 responses. No retry or error-to-Terraform-diagnostic policy is added in this step.

Key models are `User`, `Invite`, `Group`, `GroupUser`, `Collection`, `Membership`, `GroupMembership`, `PaginationResponse`, and `Auth`. Use `PermissionRead`, `PermissionReadWrite`, `PermissionAdmin` for collection grants; `GroupPermissionMember`/`GroupPermissionAdmin` for group membership; `UserRoleAdmin`/`UserRoleMember`/`UserRoleViewer`/`UserRoleGuest` for users. Collection defaults and direct grants are separate permissions.

Nullable fields have three states. A zero `nullable.Nullable[T]` omits the field, `nullable.NewNullNullable[T]()` writes JSON null, and `nullable.NewNullableWithValue(value)` writes a value. In particular, `CollectionsUpdateJSONRequestBody.Permission` must send null to make a collection private; omitting it preserves the current default. Direct user/group grants use non-null `*Permission`.

List bodies use `*int` Limit/Offset. Responses return PaginationResponse with total and nextPath. The release middleware produces nextPath even for the last page. Advance offset using the returned page metadata and stop against total; do not rely on nextPath becoming empty or follow it through a different HTTP client. Group list memberships are only a preview. Use paginated `GroupsMembershipsWithResponse` for the full membership set. Users list normally excludes suspended users; request an appropriate status filter when reconciling suspended users. Admin `includeListOnly` can expose otherwise inaccessible collections but does not grant access to their contents.

Do not send X-Client-Version, which can change pagination totals, or X-Api-Version, which switches collection descriptions to rich-text presentation. The provider's default calls preserve the release's markdown description presentation.

## Limits for later IAM work

Use an unrestricted admin-owned Outline API key for these IAM operations. Endpoint-scoped keys and server policies can still reject calls. OIDC users should use invites with suppressEmail and SSO, without passwords or magic-link sign-in. The release inviter ignores existing accounts, reports them in unsent, and returns only new users. It coerces guest invitations to member, so do not assume the requested invitation role became the stored role. Read it back before any role update.

Group description requires an update after creation in this release. Group update cannot clear externalId with null, and externally synchronized groups reject manual name/membership changes. Membership additions are upserts; updates need explicit intended permissions. Collection creation defaults sharing to true and templateManagement to admin at the server, so future resources should set their intended values explicitly. In v1.10.1, updates to a read_write collection can create an admin membership for the caller even when permission is omitted. The handler compares permission to read_write before checking whether the field was supplied.

Safe user suspension, self-admin protection, pagination loops, ownership/import policy, and delete behavior belong to the later resources. None are implemented here. These contracts were checked against source and local HTTP fixtures, not a live deployment. Newer server compatibility is not claimed.
