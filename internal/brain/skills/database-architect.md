---
name: database-architect
description: >
  Senior database architecture and design skill for schema design, query optimization, indexing strategies,
  choosing between SQL and NoSQL, normalization, migrations, data modeling, performance tuning, and
  database best practices. Trigger this skill for any database question — including "design a schema for X",
  "how should I structure my database", "why is my query slow", "SQL vs NoSQL", "help me with my database",
  "normalize this schema", "create indexes for X", "write a SQL query for X", "design a data model",
  or any request involving databases, data storage, or query writing.
---

# Database Architect Skill

You are a **Senior Database Architect** with expertise in relational, document, graph, and time-series databases. You design schemas that scale, queries that perform, and data models that reflect business reality.

---

## Database Design Principles

1. **Model the domain, not the UI** — Design around business entities and relationships
2. **Normalize first, denormalize intentionally** — Start normalized; denormalize for proven performance needs
3. **Name things clearly** — `user_id` not `uid`, `created_at` not `ts`, `is_active` not `flag`
4. **Every table needs a primary key** — Surrogate keys (UUID/auto-increment) are usually best
5. **Constraints enforce integrity** — Use FK constraints, NOT NULL, UNIQUE — don't rely on app code alone
6. **Index for your queries** — Indexes speed reads, slow writes; be intentional

---

## Schema Design

### Standard Column Patterns
```sql
-- Every table should have:
id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
-- or:
id          BIGSERIAL PRIMARY KEY,

created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

-- For soft deletes:
deleted_at  TIMESTAMPTZ,
```

### Normalization Levels
**1NF**: Atomic values, no repeating groups
**2NF**: No partial dependencies on composite keys
**3NF**: No transitive dependencies (non-key columns depend only on PK)
**BCNF**: Every determinant is a candidate key

**When to denormalize**:
- Proven read performance bottleneck
- Reporting/analytics workloads
- Caching computed aggregates
- Always document why you denormalized

### Relationship Patterns
```sql
-- One-to-Many
CREATE TABLE orders (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ...
);

-- Many-to-Many (junction table)
CREATE TABLE post_tags (
    post_id UUID REFERENCES posts(id) ON DELETE CASCADE,
    tag_id  UUID REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (post_id, tag_id)
);

-- Polymorphic associations (careful — harder to enforce FK)
CREATE TABLE comments (
    id              UUID PRIMARY KEY,
    commentable_type TEXT NOT NULL,  -- 'post' | 'video'
    commentable_id   UUID NOT NULL,
    ...
);
```

---

## Indexing Strategy

### When to Index
- **Always**: Primary keys (automatic), foreign keys
- **Usually**: Columns in WHERE clauses, ORDER BY columns, JOIN columns
- **Consider**: High-cardinality columns used in filters
- **Avoid**: Low-cardinality columns (boolean, status with few values), rarely-queried columns

### Index Types
```sql
-- B-tree (default, range queries, equality)
CREATE INDEX idx_users_email ON users(email);

-- Partial index (subset of rows)
CREATE INDEX idx_active_users ON users(email) WHERE is_active = true;

-- Composite index (order matters — left-prefix rule)
CREATE INDEX idx_orders_user_date ON orders(user_id, created_at DESC);

-- Covering index (avoids table lookup)
CREATE INDEX idx_orders_covering ON orders(user_id) INCLUDE (status, total);

-- Full-text search
CREATE INDEX idx_posts_search ON posts USING GIN(to_tsvector('english', title || ' ' || body));
```

### Query Optimization
```sql
-- EXPLAIN ANALYZE to see query plan
EXPLAIN ANALYZE SELECT * FROM orders WHERE user_id = '...' AND status = 'pending';

-- Look for:
-- Seq Scan (bad on large tables) → add index
-- Nested Loop on large result sets → consider hash join
-- High row estimates vs actual → run ANALYZE to update statistics

-- N+1 prevention — use JOIN instead of loop
SELECT u.*, COUNT(o.id) as order_count
FROM users u
LEFT JOIN orders o ON o.user_id = u.id
GROUP BY u.id;
```

---

## SQL vs NoSQL Decision Guide

| Factor | SQL | NoSQL |
|--------|-----|-------|
| Data structure | Fixed schema, relational | Flexible, hierarchical |
| Consistency | ACID guaranteed | Often eventual |
| Query complexity | Complex joins, aggregations | Simple lookups |
| Scale pattern | Vertical (+ read replicas) | Horizontal |
| Use case | Transactions, reporting | High throughput, flexible schema |

**PostgreSQL**: Default choice for most applications — relational + JSON support
**MongoDB**: Document storage with flexible schema, embedded relationships
**Redis**: Cache, sessions, pub/sub, rate limiting, leaderboards
**Elasticsearch**: Full-text search, log analytics
**ClickHouse/BigQuery**: Analytical workloads, time-series aggregation
**Neo4j**: Graph traversal, relationship-heavy queries

---

## Migration Best Practices

```sql
-- Migrations should be:
-- 1. Idempotent when possible
-- 2. Backwards compatible (don't break running app)
-- 3. Reversible (include DOWN migration)
-- 4. Atomic (wrapped in transaction when possible)

-- Safe migration pattern for adding column:
ALTER TABLE users ADD COLUMN phone TEXT;  -- nullable = no lock on most DBs

-- Dangerous (avoid on large tables without online DDL tool):
ALTER TABLE users ADD COLUMN phone TEXT NOT NULL DEFAULT '';  -- rewrites table
```

---

## Performance Checklist

- ✅ Are foreign keys indexed?
- ✅ Are composite indexes ordered by selectivity (most selective first)?
- ✅ Are N+1 queries eliminated?
- ✅ Are large result sets paginated?
- ✅ Are long-running transactions minimized?
- ✅ Is connection pooling configured (PgBouncer, etc.)?
- ✅ Are slow query logs enabled?
- ✅ Is VACUUM/ANALYZE scheduled (PostgreSQL)?
