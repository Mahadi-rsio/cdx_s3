# Caddy static_s3 Plugin

`static_s3` is a middleware plugin for Caddy v2 that serves static files directly from AWS S3 or any S3-compatible storage (like MinIO, Cloudflare R2, DigitalOcean Spaces, Backblaze B2, or Google Cloud Storage).

It is designed for production environment efficiency, security, and scalability, featuring built-in streaming, caching, range requests, SPA routing, **multi-tenant subdomain routing**, and support for AWS credentials chains.

---

## Key Features

- **Multi-Tenant Subdomain Routing:** Route `tenant-a.cloudisy.com` → its own S3 directory, resolved via Redis cache + PostgreSQL. Zero per-tenant config — just insert a row.
- **Universal S3 Compatibility:** Works with any S3-compliant storage by configuring custom endpoints and region settings.
- **Memory-Efficient Streaming:** Streams files directly from S3 to the client. Never loads large files (e.g., video or large images) entirely into Caddy's memory, avoiding Out-Of-Memory (OOM) crashes.
- **High-Performance Caching:** Features a thread-safe LRU Cache with configurable TTL:
  - Caches metadata (ETags, Last-Modified, size) to quickly respond to conditional client requests.
  - Caches actual file content for small files to completely bypass S3 API requests.
  - Caches missing files (negative caching) to prevent hammering S3 on recurrent 404s.
  - In multi-tenant mode, cache keys are scoped per-tenant (`subdomain:s3key`) to prevent cross-tenant collisions.
- **Standard Range Requests:** Supports streaming media ranges (`Content-Range` / `Accept-Ranges`) directly through S3 range headers.
- **Ambient Credentials support:** Access keys and secret keys are optional. If omitted, the plugin defaults to standard AWS credentials chain (supports IAM Roles, EKS Service Accounts, ECS Tasks, EC2 Instance Profiles, or environment variables).
- **Advanced SPA Routing:** Configurable Single Page Application (SPA) fallback (e.g., `index.html`) with customizable file extension exclusions (e.g., return a 404 instead of index.html if a `.png` or `.css` file is missing).

---

## Configuration Reference

Add the `static_s3` directive inside your site block.

```caddy
:8080 {
    # Order the static_s3 directive relative to other handlers (usually before respond/reverse_proxy)
    route {
        static_s3 {
            # --- Connection / Provider Settings ---
            # Bucket name (Required)
            bucket "my-bucket"
            
            # S3 endpoint. Omit for standard AWS S3. (Optional)
            endpoint "https://localhost:9000"
            
            # AWS Region. Defaults to "us-east-1" or "S3_REGION" environment variable. (Optional)
            region "us-east-1"
            
            # Static credentials. Omit to use IAM roles or ambient env vars. (Optional)
            access_key "my-access-key"
            secret_key "my-secret-key"
            
            # Use path-style urls (e.g., host/bucket/key) instead of virtual host (bucket.host/key).
            # Default: true (for backward compatibility and MinIO compatibility)
            use_path_style true

            # --- Multi-Tenant Settings ---
            # Base domain for subdomain extraction (enables multi-tenant mode). (Optional)
            # e.g. "cloudisy.com" → extracts "tenant-a" from "tenant-a.cloudisy.com"
            base_domain "cloudisy.com"
            
            # PostgreSQL DSN for site_id lookups. Falls back to DATABASE_URL env var. (Optional)
            db_dsn "postgres://user:pass@localhost:5432/mydb?sslmode=disable"
            
            # Redis URL for site_id caching. Falls back to REDIS_URL env var. (Optional)
            redis_url "redis://localhost:6379/0"

            # --- Routing & Paths ---
            # Sub-folder prefix inside the S3 bucket. (Optional)
            # In multi-tenant mode, defaults to "tenant" → keys like tenant/{site_id}/index.html.
            # In single-tenant mode, prepended to the request path.
            prefix "public/"
            
            # SPA fallback file. Default: "index.html". Use "none" to disable. (Optional)
            fallback "index.html"
            
            # Extensions that should bypass SPA fallback and return a 404 directly. (Optional)
            fallback_except png jpg jpeg gif ico css js svg webp json xml

            # --- Cache Settings ---
            # TTL for cache entries (e.g., 5m, 1h, 24h). Default: 0 (caching disabled). (Optional)
            cache_ttl 5m
            
            # Max capacity of the LRU cache (number of entries). Default: 1000. (Optional)
            cache_size 1000
            
            # Max size of individual files to cache their body content in memory. (Optional)
            # Supports human-readable formats (e.g., 512KiB, 2MB).
            # If a file is larger than this, only its metadata is cached.
            max_cache_size 512KiB

            # --- Bandwidth Optimization (S3 Redirection) ---
            # Redirect the client directly to the S3 bucket URL to bypass VPS network bandwidth. (Optional)
            # Default: false
            redirect_to_s3 true

            # Generate a temporary pre-signed URL for redirects to keep private buckets secure. (Optional)
            # Default: false (requires redirect_to_s3 to be true)
            presign_redirect true

            # Expiry time for pre-signed redirect URLs (e.g., 10m, 1h). Default: 15m. (Optional)
            presign_lifetime 15m
        }
    }
}
```

---

## Multi-Tenant Mode

When `base_domain` is set, the plugin switches into **multi-tenant mode**. Each unique subdomain is mapped to its own S3 directory via a UUID stored in PostgreSQL, with Redis acting as a read-through cache.

### How a request is handled

```
tenant-a.cloudisy.com/about
        │
        ▼
  1. Extract subdomain  →  "tenant-a"
        │
        ▼
  2. Redis GET "site:tenant-a"
       hit  → use cached UUID
       miss → SELECT id FROM sites WHERE subdomain='tenant-a' AND active=true
             → cache UUID in Redis (TTL 5 min)
        │
        ▼
  3. Build S3 key  →  "tenant/{UUID}/about/index.html"
        │
        ▼
  4. Serve from S3 (LRU cached as "tenant-a:tenant/{UUID}/about/index.html")
```

### PostgreSQL schema

```sql
CREATE TABLE sites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subdomain   TEXT NOT NULL UNIQUE,
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_sites_subdomain ON sites(subdomain);
```

### S3 directory structure

Each tenant's files must live under `tenant/{site_id}/` in the bucket:

```
my-bucket/
└── tenant/
    ├── 550e8400-e29b-41d4-a716-446655440000/   ← tenant-a's site_id
    │   ├── index.html
    │   ├── about/index.html
    │   └── assets/app.css
    └── 6ba7b810-9dad-11d1-80b4-00c04fd430c8/   ← tenant-b's site_id
        ├── index.html
        └── blog/index.html
```

### Redis cache behaviour

| Scenario | Redis key | Value | TTL |
|---|---|---|---|
| Tenant found | `site:tenant-a` | UUID string | 5 min |
| Tenant not found | `site:ghost` | `NOT_FOUND` | 1 min |

### Managing tenants

```bash
# Add a new tenant
psql -c "INSERT INTO sites (subdomain) VALUES ('tenant-a');"

# Disable a tenant immediately
psql -c "UPDATE sites SET active=false WHERE subdomain='tenant-a';"
redis-cli DEL site:tenant-a

# Force cache refresh (e.g. after re-enabling a tenant)
redis-cli DEL site:tenant-a
```

### Environment variables (multi-tenant)

| Variable | Caddyfile equivalent | Description |
|---|---|---|
| `BASE_DOMAIN` | `base_domain` | Base domain for subdomain extraction |
| `DATABASE_URL` | `db_dsn` | PostgreSQL connection string |
| `REDIS_URL` | `redis_url` | Redis connection URL |
| `S3_ACCESS_KEY` | `access_key` | S3 access key |
| `S3_SECRET_KEY` | `secret_key` | S3 secret key |

### Full multi-tenant Caddyfile example

```caddy
{
    order static_s3 before respond
}

*.cloudisy.com {
    static_s3 {
        endpoint        https://s3.dianahost.com
        bucket          cloudisy-sites
        access_key      {env.S3_ACCESS_KEY}
        secret_key      {env.S3_SECRET_KEY}
        use_path_style  true

        base_domain     cloudisy.com
        db_dsn          {env.DATABASE_URL}
        redis_url       {env.REDIS_URL}

        cache_ttl       10m
        cache_size      2000
        max_cache_size  5MB
        fallback_except png css js svg ico woff woff2 ttf map
    }
}
```

---

## 🚀 S3 Redirection (Bandwidth Optimization / BDIX)

For platforms with high traffic or hosting massive static media files, routing all traffic through your VPS can consume excessive bandwidth and cause latency.

By enabling `redirect_to_s3 true`, Caddy will:
1. Validate the file existence locally (checking the cache or running a cheap `HeadObject` request).
2. Catch missing files and run local SPA index fallback routing.
3. Redirect the client's browser (HTTP `307 Temporary Redirect`) directly to the S3 provider.

This shifts **100% of the download bandwidth** to your S3 provider. If your S3 provider has unlimited bandwidth (e.g., via BDIX or direct peering), this results in completely free egress and high download speeds while keeping VPS resource usage at zero.

### Private Buckets Security
If your S3 bucket is private, enable `presign_redirect true`. Caddy will generate a temporary pre-signed S3 URL on the fly (locally, using your access/secret keys with no S3 API network calls) and redirect the client to that secure URL.


---

## Provider Examples

### 1. AWS S3 (Using Ambient IAM Roles)
When deploying to AWS (EKS, ECS, EC2), you do not need to hardcode keys:
```caddy
static_s3 {
    bucket "my-production-bucket"
    region "us-west-2"
    use_path_style false # AWS standard
    cache_ttl 10m
    max_cache_size 1MB
}
```

### 2. Cloudflare R2
Cloudflare R2 uses virtual host style endpoints by default, and region is always `auto`:
```caddy
static_s3 {
    bucket "my-r2-bucket"
    endpoint "https://<account-id>.r2.cloudflarestorage.com"
    region "auto"
    access_key "r2-access-key-id"
    secret_key "r2-secret-access-key"
    use_path_style false
    cache_ttl 1h
    max_cache_size 512KiB
}
```

### 3. MinIO (Local Development)
```caddy
static_s3 {
    bucket "dev-bucket"
    endpoint "http://localhost:9000"
    access_key "admin"
    secret_key "StrongPassword123"
    use_path_style true
    cache_ttl 10s # Short TTL for dev
}
```

---

## Project Structure

```
.
├── Caddyfile          # Development Caddy configuration file
├── go.mod             # Go module file
├── go.sum             # Go dependencies checksum file
├── cache.go           # LRU cache implementation with TTL
├── cache_test.go      # LRU cache unit tests
├── handler.go         # Core middleware, S3 requests, multi-tenant routing, and streaming logic
├── plugin.go          # Caddy registration, configuration parser, PostgreSQL & Redis setup
├── plugin_test.go     # Plugin & parser unit tests
├── sql_helpers.go     # Internal sql.ErrNoRows bridge
└── README.md          # Project documentation
```

---

## How to Build

Use [xcaddy](https://github.com/caddyserver/xcaddy) to build Caddy with this plugin included.

### Installing xcaddy
```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
```

### Building Caddy with the Plugin
```bash
xcaddy build --with github.com/Mahadi-rsio/cdx_s3=. --output ./caddy
```

### Verify the plugin is embedded
```bash
./caddy list-modules | grep static_s3
# http.handlers.static_s3
```

---

## Running Tests

Run the unit tests using `go test`:
```bash
go test -v ./...
```
