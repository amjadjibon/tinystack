# ElastiCache design

**State:** Engine-backed candidate. **First useful slice:** create a
Redis-compatible cache, receive a usable endpoint, run cache commands, and
delete it through ElastiCache management APIs. The implementation brief is
[../elasticache.md](../elasticache.md).

## Feature checklist

- [x] Management — Create/describe/delete cache clusters (Redis, Valkey, Memcached), subnet groups, parameter groups, tags, and lifecycle events in records mode.
- [x] Runtime — Start and monitor an isolated Redis-compatible process/container, allocate an endpoint, and expose engine health as AWS state.
- [ ] Persistence — State whether cache contents survive TinyStack restart; if persistence is enabled, retain a named volume and reconnect safely.
- [ ] Later features — Multi-node replication, automatic failover, snapshots, scaling, TLS, AUTH, and serverless caches require separate capability gates.

## State and integration

Management metadata is regional and account-scoped. Records mode settles clusters,
subnet groups, and parameter groups in `available` immediately without container dependencies.
Runtime lifecycle belongs to an adapter so local Docker and remote daemons can supply usable endpoints.
Engine commands go directly to Redis/Memcached; TinyStack implements management and discovery,
not a second Redis protocol.

## Acceptance checks

Create cache clusters, subnet groups, and parameter groups through AWS SDK v2, verify
tagging, rebooting, modifying, listing engine versions, event recording, and CloudFormation
resource management.
