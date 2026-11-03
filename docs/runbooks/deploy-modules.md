# Runbook — Deploying provider modules

Every payment provider runs as its own module service, built from [`modules/`](../../modules): `mpesa`, `kcb`, `equity` and `verto`. This runbook covers the following tasks:
- [putting a module into service](#put-a-new-module-into-service);
- [releasing a new version](#release-a-new-version);
- [changing its address](#change-the-modules-address);
- [rotating secrets and provider credentials](#rotate-the-shared-secret);
- [retiring a module](#retire-a-module).

The core is deployed separately: see [deploying the core](deploy-core.md). The examples call the API with an operator's key in `$KEY` at `$ZIRNIS`.

## What to know first

- **Modules hold no state.** Everything is in the core's database, so a module can be replaced at any time and can run more than one replica. The only exception is a cached OAuth token.
- **A module is reachable from the core only.** Providers call Zirnis's public URL, and the core relays each call to the module. Never give a module an ingress or a public port. The exception is `verto`, see [below](#verto).
- **The core and the module share one secret.** The module reads it as `ZIRNIS_MODULE_SECRET`. The core reads it from the variable its endpoint names in `secretEnv`, e.g. `ZIRNIS_MODULE_MPESA_SECRET`. Every call is HMAC-signed with it, and the module checks the signature before it touches the provider.
- **A module that's down costs availability, never money.** The core classifies each failure:

  | What happened | The core treats it as | So |
  |---|---|---|
  | The module was unreachable, or refused the signature | unavailable | the call is retried and fails over to another channel; the provider's circuit opens and readiness reports *Degraded* |
  | The module took the request, and the answer was lost | ambiguous, after one retry with the same idempotency key | it's never re-sent and never failed over. The sweeper asks the provider through the module once the module is back |

- **Each module drains on SIGTERM.** It stops accepting requests and finishes the ones in flight for up to 15 seconds. The core waits up to the endpoint's `timeoutMs` (10 seconds) for each call. Give a module's pod at least 20 seconds to terminate, so a call in flight finishes instead of becoming ambiguous.
- **Two records in the core bind a module, and each needs maker-checker:**
  - the **module endpoint** says where the module is and which secret signs its calls;
  - the **provider channel** says which module serves a provider and what the provider may do.
- **Capabilities are fixed when the channel is authorised.** The core asks the module what the provider can do (`describe`), and the checker authorises exactly that. A module release that adds a capability changes nothing until an operator amends the channel.

## Build

From the repository root, one image per module, tagged with its version:

```bash
M=mpesa
V=$(git rev-parse --short HEAD)
(cd modules/$M && go vet ./... && go test ./...)    # includes the end-to-end tests against a fake provider
docker build -f modules/Dockerfile --build-arg MODULE=$M -t registry.example.internal/zirnis-$M-module:$V .
docker push registry.example.internal/zirnis-$M-module:$V
```

The image holds only the module and the public contracts in `pkg/`, none of the core. It runs as UID 65532 on distroless and checks its own health with `/module healthcheck`.

## Configure

Each module reads its environment. The full list is in the header of `modules/<module>/cmd/<module>-module/main.go`, and examples are in [`deploy/modules`](../../deploy/modules).

| Variable | Every module | Notes |
|---|---|---|
| `ZIRNIS_MODULE_SECRET` | yes | At least 32 random characters (`openssl rand -hex 32`). The same value goes in the core's `ZIRNIS_MODULE_<MODULE>_SECRET` |
| `MODULE_ADDR` | yes | Listen address, `:8090` by default |
| `ZIRNIS_PUBLIC_URL` | yes | Zirnis's public https address. The module builds the callback URLs it gives the provider from it |
| Provider credentials | yes | `MPESA_*`, `KCB_*`, `JENGA_*`: keys, shortcodes, passkeys, signing keys and callback tokens |
| `*_ENV` | yes | `sandbox`/`uat` or `production`. Set `production` only with production credentials |

- **Provider credentials belong to the module only.** Never put them in the core's environment. On Kubernetes, keep them in a Secret per module. On compose, use `deploy/modules/<module>.env` and never commit it.
- **A module refuses to start half-configured,** for example without an https public URL, with a short callback token, or with missing shortcodes or keys. A pod in `CrashLoopBackOff` right after a change is usually this: read its log.

## Run it

**Compose** (one host): the modules are in the `providers` profile.

```bash
cp deploy/modules/mpesa.env.example deploy/modules/mpesa.env           # then fill it in
docker compose --profile providers up -d --build mpesa-module
docker compose --profile providers ps mpesa-module                     # healthy
```

**Kubernetes:** the repository has no module manifests yet. Use this pattern, one set per module, in the `zirnis` namespace.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: mpesa-module, labels: { app: mpesa-module } }
spec:
  replicas: 2
  strategy: { type: RollingUpdate, rollingUpdate: { maxSurge: 1, maxUnavailable: 0 } }
  selector: { matchLabels: { app: mpesa-module } }
  template:
    metadata: { labels: { app: mpesa-module } }
    spec:
      terminationGracePeriodSeconds: 30   # the module drains for up to 15s
      securityContext: { runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: module
          image: registry.example.internal/zirnis-mpesa-module:<version>
          ports: [{ name: http, containerPort: 8090 }]
          envFrom: [{ secretRef: { name: mpesa-module } }]   # ZIRNIS_MODULE_SECRET, ZIRNIS_PUBLIC_URL, MPESA_*
          readinessProbe: { httpGet: { path: /healthz, port: http }, periodSeconds: 5 }
          livenessProbe: { httpGet: { path: /healthz, port: http }, periodSeconds: 10, failureThreshold: 3 }
          resources: { requests: { cpu: 50m, memory: 32Mi }, limits: { memory: 128Mi } }
          securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
---
apiVersion: v1
kind: Service
metadata: { name: mpesa-module }
spec:
  selector: { app: mpesa-module }
  ports: [{ name: http, port: 8090, targetPort: http }]
---
# Only the core may call the module.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: mpesa-module }
spec:
  podSelector: { matchLabels: { app: mpesa-module } }
  policyTypes: [Ingress]
  ingress:
    - from: [{ podSelector: { matchLabels: { app: zirnis-api } } }]
      ports: [{ port: 8090 }]
```

The module also needs outbound access to its provider's API (Daraja, KCB Buni or Jenga). If the cluster restricts egress, allow those hosts.

## Put a new module into service

1. **Run the module** (above) and check its health from inside the network:
   ```bash
   kubectl -n zirnis run probe --rm -it --restart=Never --image=busybox -- wget -qO- http://mpesa-module:8090/healthz
   # {"status":"ok","module":"mpesa","providers":"mpesa"}
   ```
2. **Give the core the shared secret.** Add `ZIRNIS_MODULE_MPESA_SECRET` to the `zirnis` Secret, then roll the core with `kubectl -n zirnis rollout restart deploy/api`. On compose, put it in `.env`, which passes it to both containers.
3. **Register the module endpoint** (maker), in the console under **Extensions** or through the API:
   ```bash
   curl -s -X POST "$ZIRNIS/ops/parameters/hooks/mpesa" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
     -d '{"url":"http://mpesa-module:8090/","hooks":["provider"],"secretEnv":"ZIRNIS_MODULE_MPESA_SECRET","timeoutMs":10000}'
   ```
   A second operator authorises it under **Pending changes** (`POST /ops/parameters/pending/{id}/authorise`).
4. **Onboard each provider's channel** (maker):
   ```bash
   curl -s -X POST "$ZIRNIS/ops/channels/MPESA" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
     -d '{"provider":"mpesa","module":"mpesa","priority":10}'
   ```
   The core asks the module what the provider can do, and the pending change lists it. The checker compares that list with what was agreed with the provider, then authorises. The channel's inward and outward books and its float open, and routing starts.

   | Module | Channels |
   |---|---|
   | `mpesa` | `MPESA` (`mpesa`) |
   | `kcb` | `KCB` (`kcb`), `KCBMPESA` (`kcb-mpesa`) |
   | `equity` | `EQUITY` (`equity`); with `JENGA_CARDS=true` also `JENGAPGW` (`jenga-pgw`) |

5. **Point the provider at Zirnis.** Register the callback URLs in [integrations.md](../integrations.md#callback-urls) with the provider, and run any setup action the module offers. For M-Pesa that's registering the paybill URLs:
   ```bash
   curl -s -X POST "$ZIRNIS/ops/channels/MPESA/actions/register_c2b" -H "Authorization: Bearer $KEY"
   ```
6. **Prove it with a small live transaction:** one collection and, where the channel pays out, one payout of the smallest amount. Follow each with `GET /ops/trace/{ref}`: it shows the request, the provider's answer, the callback and its validation, and the ledger entries.

## Release a new version

1. **Read what changed** in `modules/<module>`, `modules/kit` and `pkg/provapi`. Does the release add or remove a capability, or need a new variable? Add new variables to the module's Secret before rolling.
2. **Check the module is healthy now:**
   - in `GET /core/extensions`, the endpoint's circuit is `Closed`;
   - in `GET /ops/providers`, its providers' circuits are `Closed`;
   - `GET /core/extensions/calls?extension=mpesa` shows recent successful calls.
3. **Roll it:**
   ```bash
   kubectl -n zirnis set image deploy/mpesa-module module=registry.example.internal/zirnis-mpesa-module:$V
   kubectl -n zirnis rollout status deploy/mpesa-module
   ```
   With two replicas and `maxUnavailable: 0`, a new pod is ready before an old one drains, so the core never sees the module down. With one replica on compose (`docker compose --profile providers up -d --build mpesa-module`), calls fail for a few seconds. They fail over or are retried, so do it outside the provider's peak.
4. **Verify:**
   - the same three checks as step 2;
   - the next real transactions on the channel succeed (`GET /ops/channels/MPESA/transfers`);
   - nothing new in `Processing` beyond the usual (see [stuck payouts](stuck-payouts.md)).
5. **Take up new capabilities**, if the release added any. Amend the channel: send its current definition again (from `GET /ops/channels/MPESA`). The core asks the module again, and the pending change lists what's added or removed. A second operator authorises it.

**Roll back** with `kubectl -n zirnis rollout undo deploy/mpesa-module`, or by setting the previous image tag.
- **If you amended the channel for a new capability,** amend it back first. The older module would otherwise answer that capability's calls as unsupported. That's safe, but those calls fail.
- **Money in flight** is never re-sent by either version. Both answer status queries for it.

## Change the module's address

Amend the endpoint: post its full definition with the new `url` to `POST /ops/parameters/hooks/<module>`, and have it authorised. Run the module at both addresses until the amendment is authorised. It reaches every core instance within 3 seconds.

## Rotate the shared secret

The module accepts one secret at a time, so rotate in a quiet window by moving to a new variable name:
1. **Add the new secret to the core** under a new name, e.g. `ZIRNIS_MODULE_MPESA_SECRET_2`, and roll the core. The old name stays.
2. **Restart the module** with the new value as `ZIRNIS_MODULE_SECRET`.
3. **At once, amend the endpoint** to `"secretEnv": "ZIRNIS_MODULE_MPESA_SECRET_2"`, and have it authorised.
4. **Remove the old variable** from the core at its next restart.

Between steps 2 and 3 the module refuses the core's calls. It refuses them before it contacts the provider, so they count as unavailable: they're retried and fail over, and no money moves twice. Keep the window short.

## Rotate provider credentials

Provider keys, passkeys, certificates and IPN credentials live only in the module's environment:
1. Update the module's Secret, or `deploy/modules/<module>.env`.
2. Roll the module.
3. Verify as for a release.

If the provider issues the new credential alongside the old one, there's no gap. If the old one stops working at once, coordinate the time with the provider.

## Retire a module

Stop routing first, and stop the module last. The sweeper needs the module to settle anything still in flight with the provider.
1. **Deactivate each of its channels:** amend with the channel's current definition and `"active": false`, then have it authorised. Payments and payouts stop routing to it, and the other channels take over.
2. **Wait until nothing is in flight:**
   - `GET /ops/channels/<code>/transfers` shows nothing `Pending`;
   - no payment or payout on its providers is still `Processing`, and no deposit is `PendingValidation`;
   - the channel's books are empty (**Console → Provider channels**).

   A deactivated channel can't be asked about its transactions. If anything is still open, reactivate the channel and let it settle.
3. **Tell the provider** to stop sending callbacks, and remove the URLs registered when the module went into service.
4. **Disable the endpoint** (`"enabled": false` with its full definition), stop the module and delete its Secret.

## Verto

`verto` is built differently: it's a remote extension rather than a provider-protocol module.
- **The core calls it** at `http://<host>:8090/hooks`, on the `suspense.opened` hook. Register it with `"hooks": ["suspense.opened"]` and `"secretEnv": "ZIRNIS_HOOK_SECRET"`.
- **Verto calls it directly** at `https://<public host>/verto/webhooks`. So it needs a public, TLS-terminated route for that one path only, and nothing else of it may be exposed.

Its settings are listed in `modules/verto/cmd/verto-module/main.go`, and [adding a provider](../guides/adding-a-provider.md) walks through it.
