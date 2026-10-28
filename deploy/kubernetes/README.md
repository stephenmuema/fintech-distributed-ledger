# Zirnis on Kubernetes

These manifests run the API (three replicas or more, autoscaled), the web front (console and payer edge), Redis, RabbitMQ and PgBouncer. PostgreSQL itself runs elsewhere, ideally managed. Render them with `kubectl kustomize deploy/kubernetes`; apply them with `kubectl apply -k deploy/kubernetes`.

## Before applying

1. **Push the images** that `docker compose build` makes (`zirnis-api`, `zirnis-web`) and point `images:` in `kustomization.yaml` at them.
2. **Point PgBouncer at PostgreSQL:** set `DB_HOST` in `pgbouncer.yaml`.
3. **Create the secret** the API and PgBouncer read:

   ```bash
   kubectl create namespace zirnis
   kubectl -n zirnis create secret generic zirnis \
     --from-literal=ZIRNIS_DB_PASSWORD=... \
     --from-literal=ZIRNIS_DATABASE_URL='postgres://zirnis:...@pgbouncer:5432/zirnis?sslmode=disable' \
     --from-literal=ZIRNIS_BOOTSTRAP_ADMIN=... \
     --from-literal=ZIRNIS_PUBLIC_URL=https://pay.example.com \
     --from-literal=ZIRNIS_MODULE_MPESA_SECRET=... \
     --from-literal=RABBITMQ_PASSWORD=... \
     --from-literal=ZIRNIS_RABBITMQ_URL='amqp://zirnis:...@rabbitmq:5672/'
   ```

   PgBouncer reads the database password and RabbitMQ its own; the API reads the URLs. Percent-encode the password inside the URL if it has characters other than letters, digits, `-` and `_`.
4. **Set `ZIRNIS_CORS_ORIGINS`** in the `zirnis-api` ConfigMap (`api.yaml`) to the console's address.
5. **Expose the services.** Publish only `web` port 8081, the payer edge, to the internet, behind TLS. Keep `web` port 8080, the console, on the internal network.

## How the pods behave

**The API runs three replicas or more**. The instances coordinate through the database:
- **Close of business** runs on one instance (a lease). After switching the business date, it waits until every live instance has finished the work that could still book to the closing day.
- **Background jobs** run on the instance that holds each job's lease. Another instance takes a job over within three cycles if its holder dies.
- **Webhooks** go through RabbitMQ: a relay on each instance publishes the outbox's events, and consumers on every instance deliver them. Without `ZIRNIS_RABBITMQ_URL`, each event is claimed by one instance's dispatcher instead.
- **Authorised changes** (a provider channel, a module, rates) reach every instance within 3 seconds.
- **Rate limits** are shared through Redis. If Redis is down, each instance counts its own requests until it's back.

**Migrations run in an init container** (`zirnis migrate`) before each pod takes traffic. Pods starting together take turns, so the first migrates and the rest find the schema ready.

**The autoscaler** scales the API from 3 to 10 replicas on CPU (65%). It needs metrics-server in the cluster; without it the API stays at 3. Scale-down waits 5 minutes, and each pod drains on the way out.

**Rolling updates start a new pod first** (`maxSurge: 1, maxUnavailable: 0`), and a `PodDisruptionBudget` lets node drains take one API pod at a time.

**Probes:**

| Probe | Path | Fails when |
|---|---|---|
| Startup | `/health/live` | the process isn't up 5 minutes after the init container finished |
| Readiness | `/health/ready` | the database is unreachable, or the pod is draining |
| Liveness | `/health/live` | the process stops answering |

**Shutdown drains.** On SIGTERM, the API:
1. fails readiness and keeps serving for `ZIRNIS_SHUTDOWN_DRAIN` (5s), so traffic moves away first;
2. stops accepting requests, and finishes the ones in flight for up to `ZIRNIS_SHUTDOWN_TIMEOUT` (20s);
3. lets the workers finish their current cycle;
4. closes the database.

`terminationGracePeriodSeconds: 45` covers all four. A card payment already sent to an acquirer finishes even if its client has gone. If the process is killed anyway, recovery settles the payment.

**Web and PgBouncer are stateless:** two replicas each, spread across nodes. A `PodDisruptionBudget` lets node drains take only one pod of each at a time. Redis runs one replica: losing it costs only the shared rate-limit counts.

**RabbitMQ runs one node** (a StatefulSet with a 5 GiB volume), so queued messages survive a restart. Losing it delays webhooks, never loses them: events wait in the outbox and are published again. For high availability, run three nodes with the RabbitMQ Cluster Operator and point `ZIRNIS_RABBITMQ_URL` at it. Other services can bind their own queues to the exchange `zirnis.events`.

**Settlement reports and nostro statements are uploaded through the API.** Nothing is kept on the pod. To use the inbox directories instead (`ZIRNIS_SETTLEMENT_INBOX`, `ZIRNIS_STATEMENT_INBOX`), mount a `ReadWriteMany` volume at `/data`. A `ReadWriteOnce` volume can block the surge pod during a rollout.

## PgBouncer

PgBouncer runs in transaction mode. Zirnis's PostgreSQL driver prepares statements, so `MAX_PREPARED_STATEMENTS` must be set (PgBouncer 1.21+). Without it, units of work fail.

**The connection budget.** One API instance opens up to 34 client connections: 16 writers (`ZIRNIS_DB_MAX_WRITE_CONNS`), 16 readers and 2 for reference blocks. At the autoscaler's maximum of 10, that's 340, well within PgBouncer's `MAX_CLIENT_CONN`. PgBouncer turns them into `DEFAULT_POOL_SIZE` (40) server connections per replica, so keep `PgBouncer replicas × 40` within PostgreSQL's `max_connections`. PostgreSQL's CPU is the real ceiling: raise `maxReplicas` only once the database has room.

To try it without Kubernetes, use `deploy/compose/pgbouncer.yml`.

## Not here yet

- **The sandbox's simulated providers across replicas.** They keep their state in each API process, so in a multi-replica sandbox a callback can reach a pod that never sent the request. Real provider modules are separate services and aren't affected.
- **PostgreSQL itself.** Use a managed, highly available PostgreSQL. Backups are covered in [backup-restore.md](../../docs/runbooks/backup-restore.md).
