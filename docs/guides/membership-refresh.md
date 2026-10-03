---
page_title: "Membership refresh - Outline"
subcategory: ""
description: |-
  Refresh behavior and request bounds for collection grants and group memberships.
---

# Membership refresh

This guide applies only to refresh of `outline_collection_user`, `outline_collection_group`, and `outline_group_member` on Outline 1.10.1. It does not change other resources, name lookups, mutation preflights, import discovery, or post-removal verification.

## Positive refresh and its audit scope

Each refresh verifies the active admin caller and reads the parents again. API-key-owner refusal for direct collection grants, archived-collection refusal, and external-group restrictions remain in force. There is no authentication, parent, or membership cache.

The provider uses the freshly read principal name as the membership endpoint's `query`. These endpoints search user or group names with a case-insensitive literal substring, not a UUID, email, or exact-name lookup. The server's [QueryHelper](https://github.com/outline/outline/blob/4a5a616a21be800257dc11cef4263d0dd0412156/server/storage/QueryHelper.ts) escapes percent, underscore, and backslash before adding substring wildcards. The provider sends the raw name without SQL escaping. Blank, invalid UTF-8, and NUL-containing names use the unfiltered list instead.

A name is only a way to narrow the list. The provider identifies the target by its exact UUID and accepts it only after every filtered page passes the same envelope, identity, associated-principal, permission, duplicate, pagination, and stable-total checks as the full list. It does not stop on the first matching page or filter by permission. Same-name principals require the entire filtered walk. Collection/principal pairs are not database-unique in this release; a conflicting pair on a later filtered page is an error.

A successful filtered refresh deliberately stops auditing rows with unrelated names. It cannot report malformed or duplicate grants that the server excludes from the filtered result. If the server ignores `query` and returns a broad list, the provider still validates every returned page and discriminates by UUID.

## Absence and writes still need the full list

A valid filtered result without the exact UUID does not prove absence. A principal may have been renamed between the parent read and the query. The provider falls back to the complete unfiltered walker, including all validation, before removing the resource from state. Existing release-verified parent-absence checks remain unchanged.

A filtered error, forbidden response, malformed page, duplicate, or changing total remains a diagnostic and retains state. It does not trigger an absence shortcut. Create, update, delete, import discovery, and post-removal verification always use the unfiltered walk. Keeping their full-list audit avoids narrowing the existing preflight and removal checks.

Neither walk is an atomic snapshot. The server counts and retrieves rows separately, then paginates by creation time with offsets and no unique tiebreaker. Stable totals and duplicate checks catch some movement, not all same-total churn. A concurrent rename during filtered pagination can change which rows qualify without changing the total. Coordinate concurrent writers; this optimization does not establish snapshot isolation or prevent the existing upsert race.

## Request bounds

For a successful stable read with existing parents, let:

- `U = max(1, ceil(parent membership rows / 100))`, the unfiltered page count.
- `F = max(1, ceil(name-matching membership rows / 100))`, the filtered page count.

| Refresh result | Membership requests |
| --- | --- |
| Exact UUID present in a usable-name query | F |
| Valid filtered miss, including a rename or true absence | F + U |
| Blank or unusable name | U |

Collection grants also make four fresh requests per pair: two `auth.info`, one `collections.info`, and one principal info request. Group memberships make three: one `auth.info`, one `groups.info`, and one `users.info`. Parent absence can require additional workspace-list verification. Nothing here removes that overhead.

The unpaced protocol and real Terraform plan fixtures each manage 1,000 pairs under a parent with 1,000 rows:

| Resource | Previous membership / total requests | Unique-name filtered membership / total requests | Same-name membership / total requests |
| --- | --- | --- | --- |
| Collection user | 10,000 / 14,000 | 1,000 / 5,000 | 10,000 / 14,000 |
| Collection group | 10,000 / 14,000 | 1,000 / 5,000 | 10,000 / 14,000 |
| Group member | 10,000 / 13,000 | 1,000 / 4,000 | 10,000 / 13,000 |

The unique-name fixtures each match one row. Unique names alone do not guarantee one row, because one name can be a substring of another. Identical or overlapping names can still require all parent pages for each managed pair, leaving quadratic worst-case work. A valid miss adds filtered pages to the existing full scan.

These counts are mock request measurements, not production speed measurements. The mocks disable pacing and preencode responses. The production client still shares a five-requests-per-second limiter, and real HTTP, database work, contention, server errors, and parent resources add cost. Increasing Terraform parallelism does not remove that client's rate limit.

The pinned primary handlers are [collections.memberships and collections.group_memberships](https://github.com/outline/outline/blob/4a5a616a21be800257dc11cef4263d0dd0412156/server/routes/api/collections/collections.ts) and [groups.memberships](https://github.com/outline/outline/blob/4a5a616a21be800257dc11cef4263d0dd0412156/server/routes/api/groups/groups.ts). The latter searches the joined user name despite its schema comment describing group names.
