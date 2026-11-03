# Runbook — Deploying the core

This runbook releases a new version of the core (`zirnis-api`) and the web front (`zirnis-web`), or changes their settings. It covers the production path on Kubernetes ([deploy/kubernetes](../../deploy/kubernetes/README.md)) and a single host with compose. Provider modules are released separately: see [deploying provider modules](deploy-modules.md).

The examples call the API with an operator's key in `$KEY` at `$ZIRNIS`, the internal API address.

## What a release does

- **Migrations run before a pod takes traffic.** The `migrate` init container runs `zirnis migrate`, which migrates and bootstraps, then exits. Pods starting together take turns, so the first migrates and the rest find the schema ready.
- **Migrations only go forward.** There are no down migrations. An older binary ignores migrations it doesn't know.
- **Old and new pods overlap.** The rolling update starts one new pod before stopping an old one (`maxSurge: 1, maxUnavailable: 0`). Old pods keep serving on the new schema for a few minutes, so a release's migrations must only *add*: new tables, new nullable or defaulted columns, new indexes. Removing or renaming something takes two releases: stop using it, then drop it.
- **Pods drain on the way out.** An old pod fails readiness first, then finishes its requests in flight, then its workers' current cycle. A card payment already sent to an acquirer finishes even if its client has gone.

## Before the release

1. **Read what changes.**
   ```bash
   git diff --stat <current>..<new> -- internal/store/migrations .env.example deploy/
   ```
   - **New migrations:** check that they only add (see above). If one doesn't, plan the release as a [restore-capable](#roll-back) one.
   - **New settings in `.env.example`:** add them to the `zirnis-api` ConfigMap or the `zirnis` Secret first.
2. **Pick a window.**
   - Stay clear of Close of Business, `ZIRNIS_AUTO_COB_AT` (23:00 by default). No COB run may be in progress:
     ```bash
     curl -s -H "Authorization: Bearer $KEY" "$ZIRNIS/ops/cob/runs" | jq '.[0] | {status, businessDate}'
     ```
   - Avoid the day's peak. On a single compose host, the API is down for a few seconds while it restarts.
3. **Check the system is healthy now**, so anything wrong afterwards belongs to the release:

   | Check | Expect |
   |---|---|
   | `GET /health/ready` | `Healthy` (`Degraded` means a provider circuit is open: note which) |
   | `GET /ops/audit/verify` | `valid: true` |
   | `GET /ops/gl/integrity` | `healthy: true` |
   | `GET /ops/suspense/integrity` | `[]` |
   | `GET /ops/providers` | every circuit `Closed` |
   | `GET /ops/outbox?status=DeadLettered` | note the count |

4. **Back up** when the release has migrations. Use a managed snapshot or a point-in-time marker, or `pg_dump` on compose ([backup and restore](backup-restore.md)). That backup is the rollback for a migration that goes wrong.
5. **Test the exact commit** on PostgreSQL:
   ```bash
   go vet ./... && ZIRNIS_TEST_DATASOURCE=postgres ZIRNIS_TEST_DATABASE_URL='postgres://…/postgres?sslmode=disable' go test ./...
   (cd web && npm ci && npm run lint && npm run build)
   ```
6. **Build and push the images**, tagged with the version. Never use `latest`: rollbacks need the previous tag.
   ```bash
   V=$(git rev-parse --short HEAD)
   docker build -t registry.example.internal/zirnis-api:$V .
   docker build -t registry.example.internal/zirnis-web:$V ./web
   docker push registry.example.internal/zirnis-api:$V
   docker push registry.example.internal/zirnis-web:$V
   ```

## Deploy on Kubernetes

1. **Point the manifests at the new images:**
   ```bash
   cd deploy/kubernetes
   kustomize edit set image zirnis-api=registry.example.internal/zirnis-api:$V zirnis-web=registry.example.internal/zirnis-web:$V
   ```
2. **Preview, then apply:**
   ```bash
   kubectl diff -k .     # only the image tags, plus any setting you meant to change
   kubectl apply -k .
   ```
3. **Watch the first pod migrate:**
   ```bash
   kubectl -n zirnis get pods -l app=zirnis-api -w
   kubectl -n zirnis logs <new-pod> -c migrate -f
   ```
   A migration that fails stops the pod in `Init:Error`. The old pods keep serving, and nothing else rolls. Go to [roll back](#roll-back).
4. **Wait for the rollout:**
   ```bash
   kubectl -n zirnis rollout status deploy/api --timeout=15m
   kubectl -n zirnis rollout status deploy/web --timeout=5m
   ```
   The autoscaler and the `PodDisruptionBudget` stay in force during the rollout. Rolling 3 pods takes a few minutes.

**The first deployment** creates the schema and the first administrator. If `ZIRNIS_BOOTSTRAP_ADMIN_PASSWORD` wasn't set, the one-time password is in the migrate log of the first pod:

```bash
kubectl -n zirnis logs <first-pod> -c migrate | grep "one-time password"
```

## Deploy on one host (compose)

```bash
git checkout <new> && docker compose build api web
docker compose up -d api web                       # migrations run as the API starts
docker compose ps                                  # api: healthy
docker compose logs --since 5m api | grep -iE 'error|migrat'
```

Add the overlays you run (for example `-f deploy/compose/rabbitmq.yml`) to every command. Keep the previous images: tag them before building, `docker tag zirnis-api:local zirnis-api:previous`.

## Verify

Within 15 minutes of the rollout:
1. **Every pod is ready:** `kubectl -n zirnis get pods -l app=zirnis-api`, and `GET /health/ready` returns `Healthy`, or the same `Degraded` you noted before.
2. **The books still prove themselves:** `/ops/audit/verify`, `/ops/gl/integrity` and `/ops/suspense/integrity` answer as before. `GET /ops/gl/trial-balance` balances in every currency.
3. **The schema is current:**
   ```sql
   SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 3;
   ```
   The newest file in `internal/store/migrations` is listed. A `lock:setup` row appears only while an instance is migrating.
4. **Money moves:** watch the transactions that arrive (`GET /transactions`) succeed, or make a small own-account transfer.
5. **Events go out:** `GET /ops/outbox?status=Pending` drains, and the dead-letter count hasn't grown. With RabbitMQ, the `zirnis.webhooks` queue isn't growing (`rabbitmqctl list_queues name messages`, or the management UI).
6. **Errors:**
   ```bash
   kubectl -n zirnis logs -l app=zirnis-api --since=15m | grep -E '"level":"ERROR"'
   ```

## Roll back

Decide within the verification window. Roll back when money doesn't move, an integrity check fails, or errors climb.

- **The release added only (the usual case):** roll back the images. The older binary runs on the newer schema.
  ```bash
  kubectl -n zirnis rollout undo deploy/api
  kubectl -n zirnis rollout undo deploy/web
  kubectl -n zirnis rollout status deploy/api
  ```
  Then set the previous tags in `kustomization.yaml`, so the next `apply` doesn't bring the release back. On compose, `docker tag zirnis-api:previous zirnis-api:local && docker compose up -d api`.
- **A migration damaged data, or the old binary can't run on the new schema:** restore.
  1. Stop the core: `kubectl -n zirnis scale deploy/api --replicas=0`. The autoscaler can't scale up from 0.
  2. Restore the pre-release backup, following [backup and restore](backup-restore.md).
  3. Deploy the previous images.
  4. Follow *After a real restore* in that runbook: whatever happened at the providers since the backup isn't in the books.

  This loses data, so it needs the incident lead's decision (SEV1).

## Changing settings only

- **ConfigMap or Secret:** pods read their environment at start, so apply and then restart:
  ```bash
  kubectl apply -k deploy/kubernetes
  kubectl -n zirnis rollout restart deploy/api
  ```
- **Business parameters** (charges, interest, channels, module endpoints, operators) aren't deployments. They are maker-checker records in the console, and they reach every instance within 3 seconds of authorisation.
- **A new module secret** (`ZIRNIS_MODULE_<MODULE>_SECRET`) is a Secret change plus a restart. Its rotation is covered in [deploying provider modules](deploy-modules.md#rotate-the-shared-secret).

## Monthly partitions (PostgreSQL, once)

`zirnis partition` converts the journal lines and the audit trail to monthly partitions. Run it once, with the API stopped:
1. `kubectl -n zirnis scale deploy/api --replicas=0`, after a backup.
2. Run the release's image with the argument `partition` and the same environment (ConfigMap `zirnis-api`, Secret `zirnis`), for example as a one-off Job. On compose: `docker compose stop api && docker compose run --rm api partition`.
3. Scale back to 3. The workers keep the partitions ahead of the calendar from then on.

If partitioning fails, nothing changes. It reports the error and you can retry.
