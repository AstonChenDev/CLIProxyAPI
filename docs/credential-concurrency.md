# Standalone credential concurrency

Standalone CPA supports an optional `max_in_flight` integer in each persisted credential's metadata. The default, `0`, or an absent/null value means unlimited. Positive values up to 1,000,000 limit simultaneous requests across all models for that credential.

Open `/credential-concurrency.html` or use the shortcut in the existing `/management.html` panel. Sign in with the existing management key, edit a credential's limit, and save that row. The page never stores the management key in browser storage. Credential enablement, priority, model aliases, image generation, and image storage keep their existing behavior.

The existing authenticated management API also accepts:

```http
PATCH /v0/management/auth-files/fields
Content-Type: application/json
X-Management-Key: <management-key>

{"name":"credential-file.json","max_in_flight":2}
```

Use the credential ID or file name for `name`. `GET /v0/management/auth-files` includes `max_in_flight` and `admitted_in_flight` for each credential. Limits use the existing file/PostgreSQL credential store; token refresh preserves the policy. Config-defined API-key providers and virtual plugin credentials cannot be edited through this auth-file setting.

## Admission and release

- Admission atomically checks the latest limit before credential preparation or upstream execution. Saturated credentials are skipped without spending the upstream credential retry budget, recording an upstream failure, or applying quota cooldown.
- If no selected credential has capacity, CPA returns HTTP 429 and `Retry-After: 1`. Calls are not held in an unbounded waiting queue.
- Non-streaming calls, count-token calls, image requests, streaming calls (including the normal downstream WebSocket execution path), and Antigravity credits fallback share credential admission.
- Streaming admission lasts through the response lifecycle. If cancellation or bootstrap failure starts asynchronous draining, the raw upstream channel retains the slot until it closes. A client disconnect alone cannot release capacity while its upstream is still running.
- Unlimited requests are counted too. Lowering a limit immediately accounts for existing requests, without terminating them. Reloading or re-registering the same credential ID does not reset active counts.

The counter is **per CPA process**, not a distributed limit across replicas. Home mode continues to use Home's authoritative limiter and does not apply a second local limit. Idle reusable WebSocket connections are not active model requests. Management probes, OAuth refresh operations, and plugin-owned execution outside the credential manager are not included.
