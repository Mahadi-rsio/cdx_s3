# Caddy static_s3 Plugin

`static_s3` is a middleware plugin for Caddy v2 that serves static files directly from AWS S3 or any S3-compatible storage (like MinIO, Cloudflare R2, DigitalOcean Spaces, Backblaze B2, or Google Cloud Storage). 

It is designed for production environment efficiency, security, and scalability, featuring built-in streaming, caching, range requests, SPA routing, and support for AWS credentials chains.

---

## Key Features

- **Universal S3 Compatibility:** Works with any S3-compliant storage by configuring custom endpoints and region settings.
- **Memory-Efficient Streaming:** Streams files directly from S3 to the client. Never loads large files (e.g., video or large images) entirely into Caddy's memory, avoiding Out-Of-Memory (OOM) crashes.
- **High-Performance Caching:** Features a thread-safe LRU Cache with configurable TTL:
  - Caches metadata (ETags, Last-Modified, size) to quickly respond to conditional client requests.
  - Caches actual file content for small files to completely bypass S3 API requests.
  - Caches missing files (negative caching) to prevent hammering S3 on recurrent 404s.
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

            # --- Routing & Paths ---
            # Sub-folder prefix inside the S3 bucket to limit lookups. (Optional)
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
        }
    }
}
```

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
├── handler.go         # Core middleware, S3 requests, and streaming logic
├── plugin.go          # Caddy registration & configuration parser
├── plugin_test.go     # Plugin & parser unit tests
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
Build locally:
```bash
xcaddy build --with github.com/Mahadi-rsio/cdx_s3=.
```

---

## Running Tests

Run the unit tests using `go test`:
```bash
go test -v ./...
```
