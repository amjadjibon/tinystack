# ElastiCache design

**State:** Engine-backed candidate. **First useful slice:** create a
Redis-compatible cache, receive a usable endpoint, run cache commands, and
delete it through ElastiCache management APIs. The implementation brief is
[../elasticache.md](../elasticache.md).

## Feature checklist

- [x] Management — Create/describe/delete cache clusters (Redis, Valkey, Memcached), subnet groups, parameter groups, tags, and lifecycle events in records mode.
- [x] Runtime — Start and monitor an isolated Redis-compatible process/container, allocate an endpoint, and expose engine health as AWS state.
- [x] Replication Groups — Single-shard replication groups (cluster mode disabled), primary/reader endpoints, horizontal scale up/down, manual and test failover via `REPLICAOF`, Redis `AUTH` token rotation and deletion, and `AWS::ElastiCache::ReplicationGroup` in CloudFormation.
- [ ] Serverless caches — Serverless cache lifecycle and endpoint management.
- [ ] Persistence — State whether cache contents survive TinyStack restart; if persistence is enabled, retain a named volume and reconnect safely.
- [ ] Later features — Multi-shard cluster mode enabled, snapshots/backups require separate capability gates.

## State and integration

Management metadata is regional and account-scoped. Records mode settles clusters,
replication groups, subnet groups, and parameter groups in `available` immediately without container dependencies.
Runtime lifecycle belongs to an adapter so local Docker and remote daemons can supply usable endpoints.
Engine commands go directly to Redis/Memcached; TinyStack implements management and discovery,
not a second Redis protocol.

## Acceptance checks

Create cache clusters, replication groups, subnet groups, and parameter groups through AWS SDK v2, verify
tagging, rebooting, modifying, listing engine versions, failovers, scale-out/in, AUTH tokens, event recording, and CloudFormation
resource management.
