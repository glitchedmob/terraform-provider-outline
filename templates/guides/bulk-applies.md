---
page_title: "Bulk applies"
subcategory: ""
description: |-
  Plan batches and bounded quota waits for Outline 1.10.1.
---

# Bulk applies

Outline's production quotas are lower than the provider's five-attempts-per-second pacing. A large apply can stop partway through even when every resource configuration is valid. Plan around the quota window, not just Terraform parallelism.

## Outline 1.10.1 quotas

These are the released route quotas with `RATE_LIMITER_ENABLED=true` and `RATE_LIMITER_MULTIPLIER=1`:

| API operation | Requests | Window | Provider use |
| --- | --- | --- | --- |
| `groups.create` | 10 | 60 seconds | New groups |
| `users.invite` | 50 | 3600 seconds | New users |
| `collections.add_user` | 100 | 3600 seconds | Direct user-grant creates and permission updates |
| `users.delete` | 10 | 3600 seconds | User destroy with `delete_permanently = true` |

Quotas count requests, not Terraform resources. Rejected validation or authorization attempts can consume quota too. The default bucket for paths without a registered route quota allows 1000 requests per 60 seconds. The first request to a route can consume both default and route quota as the server registers its limiter. See the [pinned server contracts](https://github.com/glitchedmob/terraform-provider-outline/blob/main/openapi/README.md#verified-pre-mutation-rate-limit-retries) for sources and middleware order.

For API keys, Outline consumes an IP-address bucket and a credential-derived bucket. Both need capacity. Different keys behind the same NAT or proxy can share the IP limit; one key used from several IPs still shares its credential limit. Other Terraform processes, provider aliases, scripts, and users can spend the same quota. Each configured provider has its own pacing and retry budget, not a reservation or a shared quota cache. Changing keys or increasing Terraform parallelism does not create more capacity for a shared IP.

## Choose a bounded wait

The default configuration allows each API call a total quota-wait budget of 120 seconds and a separate 30-second timeout for each HTTP attempt:

```terraform
provider "outline" {
  timeout_seconds         = 30
  rate_limit_wait_seconds = 120
}
```

`rate_limit_wait_seconds` accepts integers from `0` through `3600`. Zero returns a 429 immediately without automatic retries. A larger budget is an operator choice, not a guarantee that the apply will finish. Each API call allows at most three retries, regardless of its budget. The budget counts quota waits for that call, not elapsed time for a resource or the whole apply. Concurrent calls each have their own budget. Repeated calls in one resource operation can each use a budget.

Quota waits happen outside `timeout_seconds`. Increasing the network timeout does not let an hourly quota fit a 120-second wait budget. Conversely, a valid minute-quota wait can exceed the 30-second network timeout without failing. Terraform cancellation or a context deadline stops attempts and waits sooner. Cancellation does not roll back requests that Outline already completed.

Only `groups.create`, `users.invite`, `collections.add_user`, and `users.delete` can retry, and only after a verified Outline 1.10.1 pre-mutation 429. The provider requires the released JSON error envelope, positive `Retry-After`, and consistent depleted-bucket headers. It never replays an unknown or proxy 429, transport error, timeout, malformed response, or 5xx. Those failures may leave a completed remote write whose response was lost.

## Batch and resume

With otherwise unused default quotas, eleven new groups can finish after a minute-window wait. Eleven invitations may fit an hourly bucket, but 51 invitation requests cannot assume they will. Direct collection-user permission updates consume the same 100-per-hour quota as adds. The provider does not wait an hour by default: it fails immediately when a verified `Retry-After` exceeds the call's remaining budget.

Choose between smaller batches and an explicit longer budget. For hourly quotas, waiting for the reset and applying the remaining configuration later is often simpler than keeping Terraform running for an hour. A `3600` budget can allow an hourly wait when the reported delay fits, but new contention, timer overshoot, repeated rejections, or cancellation can still stop the call. Set your CLI or automation deadline to match your choice. Do not disable server rate limiting just to complete a bulk apply.

Use a smaller intended configuration batch, then expand it after the quota resets. Lowering `-parallelism` reduces bursts but does not bypass a fixed hourly quota. Remember to leave capacity for unrelated writers. Do not remove already-managed resources from configuration merely to omit them from a later batch, since Terraform may plan to destroy them.

An apply is not a transaction. After failure, inspect the saved state, Outline, and a fresh plan before resuming:

- Successful earlier resources remain managed, even if a later call exhausted its budget.
- A verified pre-mutation rejection did not perform that endpoint's resource mutation. It does not undo earlier calls in the operation.
- A failure after creation can retain a UUID and mark the resource tainted. A plain apply can then replace it. Follow the resource's recovery instructions and confirm the remote object before using `terraform untaint`.
- If an ambiguous failure left a remote object without a state identity, reconcile or [import it](https://registry.terraform.io/providers/glitchedmob/outline/latest/docs/guides/import) instead of blindly creating another one.

## Server quota changes

Self-hosted operators can change `RATE_LIMITER_MULTIPLIER` to scale route counts. Outline rounds each multiplied count to the nearest integer, with a minimum of one; the route window stays the same. `RATE_LIMITER_REQUESTS` and `RATE_LIMITER_DURATION_WINDOW` configure the default bucket, not the hard-coded route counts. Hosted users may not control any of these settings.

Increasing server quotas is not the default recommendation. First coordinate writers, estimate request counts, and decide whether to batch or wait. If an operator changes quotas, review the instance's capacity and abuse protections, then verify the effective headers. The provider accepts a valid server-supplied limit rather than assuming it is always the table value. It cannot change server quota settings.
