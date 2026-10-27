# ADR 0030 — Several instances on one database

**Status:** Accepted. It removes the last "Not yet" of [ADR 0027](0027-parallel-writers.md): close of business now waits for every instance, not only its own.

## Context

ADR 0027 made one instance write in parallel safely, and ADR 0028 split the busy accounts, so the ledger itself was already safe with several writers. What still assumed a single process sat around the ledger:
- **Close of business** was guarded by an in-process mutex. After switching the date, it waited only for its own instance's postings.
- **Every instance ran every job.** That includes scheduled COB, the recovery and status sweeps, validation (guarded only by an in-process mutex) and partition upkeep.
- **The outbox dispatcher read events without claiming them,** so N instances delivered each webhook N times.
- **An authorised change reached only the instance that authorised it:** the provider registry, the extensions' COB jobs and table rates.
- **Migrations took no lock.** Pods starting together raced, and the loser crash-looped.
- **Rate limits were counted per process,** so N instances allowed N times the limit.

## Decision

**Coordination stays in the database**, because it must agree with what the database committed. Redis is used only for the shared rate limiter, and is optional.

- **Close of business waits for every live instance.**
  - Each instance records a heartbeat (`instances`) every 3 seconds, whether or not it runs workers. One silent for 30 seconds is gone. Its units of work ended with it, so close of business doesn't wait for it.
  - After committing the date switch, close of business waits until every live instance has acknowledged the run (`cob_acks`).
  - An instance acknowledges once its own units of work that began before it *saw* the switch have finished (`WaitForWriters`). Any unit of work that read the old date began before the switch committed, and so before the instance saw it. That makes the barrier exact without comparing clocks across machines.
  - The acknowledgement runs in its own loop, so a long wait never stops the heartbeat and makes the instance look gone.
  - A stopping instance deregisters itself.
- **One instance at a time runs a singleton job, under a lease** (`job_leases`, `Store.WithLease`):
  - close of business itself (`cob`), however it's started, scheduled or by an operator;
  - the COB scheduler, the operations sweeps, validation and partition upkeep.

  A lease lasts three of its job's cycles. It's renewed while the job runs and released after, so another instance takes the job over only if its holder died. Leases run on the wall clock.
- **Webhooks are claimed.**
  - A dispatcher claims a batch (`claimed_by`, `claimed_until`, 2 minutes) and delivers only what it claimed. It stops short of the claim's end.
  - What it didn't deliver goes back to the queue when the claim lapses. Delivery stays at-least-once.
  - Dispatchers run on every instance, so delivery scales out.
- **Authorised changes reach every instance.**
  - Authorising a parameter change bumps `config_versions` in the same commit.
  - Every instance checks the version every 3 seconds and reloads when it moves (`App.Reload`): the provider registry, the extensions' COB jobs and table rates.
- **Instances starting together take turns to migrate and bootstrap.**
  - The lock is a row in `schema_migrations`. That table exists before any migration on every engine, and the row's insert commits on its own even where DDL commits implicitly (MySQL, Oracle). A holder that died is taken over after 10 minutes.
  - `zirnis migrate` migrates and exits. On Kubernetes it runs as each pod's init container.
- **Rate limits are shared through Redis when `ZIRNIS_REDIS_URL` is set.** Each limit is one counter per client per minute.
  - If Redis doesn't answer within 100 ms, the instance counts its own requests and tries Redis again 5 seconds later. Redis is never needed to take a request.
- **Circuit breakers stay per instance** ([ADR 0003](0003-provider-orchestration-and-resilience.md)). Each instance learns a provider is down within a few calls; sharing that state isn't worth a dependency.

## Consequences

- The API can run as many instances as the database can serve. On Kubernetes that's 3 to 10 replicas behind a Horizontal Pod Autoscaler ([deploy/kubernetes](../../deploy/kubernetes/README.md)). PostgreSQL's CPU and connections are the ceiling.
- Close of business takes a moment longer: each instance acknowledges within a heartbeat, after its in-flight work finishes.
- Webhooks can arrive twice if a dispatcher dies mid-batch, as before. Receivers de-duplicate on `Zirnis-Event-Id`.
- The sandbox's simulated providers keep their state in each process. In a multi-instance sandbox, a callback can reach an instance that never sent the request. Real provider modules are separate services.
