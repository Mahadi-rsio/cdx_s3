package cdx_s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/redis/go-redis/v9"
)

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (p *StaticPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	urlPath := r.URL.Path

	// ── Multi-tenant mode ──────────────────────────────────────────────────
	if p.BaseDomain != "" {
		// 1. Extract subdomain from Host header
		host := r.Host
		// Strip port if present
		if h, _, err := splitHostPort(host); err == nil {
			host = h
		}

		suffix := "." + p.BaseDomain
		if !strings.HasSuffix(host, suffix) {
			return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("host %q does not match base domain", host))
		}
		subdomain := strings.TrimSuffix(host, suffix)
		if subdomain == "" || strings.Contains(subdomain, ".") {
			return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("invalid subdomain %q", subdomain))
		}

		// 2. Resolve site_id via Redis → PostgreSQL
		siteID, err := p.resolveSiteID(r.Context(), subdomain)
		if err != nil {
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}
		if siteID == "" {
			return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("tenant %q not found", subdomain))
		}

		// 3. Build S3 key: {site_id}/{path}
		cleanPath := strings.TrimPrefix(urlPath, "/")
		var s3Key string
		if urlPath == "/" || urlPath == "" || cleanPath == "" {
			s3Key = siteID + "/index.html"
		} else {
			s3Key = siteID + "/" + cleanPath
		}

		// 4. SPA fallback: paths without extension (and not excluded) serve index.html
		isFallbackRequest := false
		if p.Fallback != "" {
			if urlPath == "/" || urlPath == "" || (!hasExtension(urlPath) && !p.isExcludedFromFallback(urlPath)) {
				s3Key = siteID + "/index.html"
				isFallbackRequest = true
			}
		}

		// 5. Scope LRU cache key per tenant to avoid cross-tenant collisions
		cacheKey := subdomain + ":" + s3Key

		err = p.serveObjectWithCacheKey(w, r, s3Key, cacheKey, isFallbackRequest)
		if err != nil {
			if p.Fallback != "" && !isFallbackRequest && p.isNotFoundError(err) && !p.isExcludedFromFallback(urlPath) {
				fallbackKey := siteID + "/index.html"
				fallbackCacheKey := subdomain + ":" + fallbackKey
				return p.serveObjectWithCacheKey(w, r, fallbackKey, fallbackCacheKey, true)
			}
			return caddyhttp.Error(http.StatusNotFound, err)
		}
		return nil
	}

	// ── Single-tenant mode (existing behaviour) ────────────────────────────
	path := urlPath
	key := strings.TrimPrefix(path, "/")
	if p.Prefix != "" {
		// Clean up the key prefix combination
		key = filepath.Join(p.Prefix, key)
		// Ensure standard forward slashes for S3 keys on all platforms
		key = filepath.ToSlash(key)
	}

	// SPA Routing Check
	isFallbackRequest := false
	if p.Fallback != "" {
		// If requesting root/empty path, or path has no extension and is not excluded
		if path == "/" || path == "" || (!hasExtension(path) && !p.isExcludedFromFallback(path)) {
			key = p.Fallback
			isFallbackRequest = true
		}
	}

	err := p.serveObject(w, r, key, isFallbackRequest)
	if err != nil {
		// If object not found and we haven't already tried to serve the fallback
		if p.Fallback != "" && !isFallbackRequest && p.isNotFoundError(err) && !p.isExcludedFromFallback(path) {
			// Fallback to SPA entrypoint
			return p.serveObject(w, r, p.Fallback, true)
		}
		return caddyhttp.Error(http.StatusNotFound, err)
	}

	return nil
}

// resolveSiteID looks up the site UUID for a given subdomain, using Redis as a
// read-through cache backed by PostgreSQL.
func (p *StaticPlugin) resolveSiteID(ctx context.Context, subdomain string) (string, error) {
	redisKey := "site:" + subdomain

	// 1. Redis cache lookup
	if p.redisClient != nil {
		val, err := p.redisClient.Get(ctx, redisKey).Result()
		if err == nil {
			// Cache hit
			if val == "NOT_FOUND" {
				return "", nil // tenant does not exist
			}
			return val, nil
		}
		if !errors.Is(err, redis.Nil) {
			// Unexpected Redis error — log and fall through to DB
			_ = err
		}
	}

	// 2. PostgreSQL lookup
	if p.db == nil {
		return "", fmt.Errorf("static_s3: multi-tenant mode requires db_dsn")
	}

	var siteID string
	row := p.db.QueryRowContext(ctx,
		"SELECT id FROM sites WHERE subdomain = $1 AND active = true LIMIT 1",
		subdomain,
	)
	err := row.Scan(&siteID)
	if err != nil {
		if errors.Is(err, errNoRows) {
			// Tenant not found — cache negative result for 1 minute
			if p.redisClient != nil {
				_ = p.redisClient.Set(ctx, redisKey, "NOT_FOUND", 1*time.Minute).Err()
			}
			return "", nil
		}
		return "", fmt.Errorf("static_s3: db query error: %w", err)
	}

	// Cache the found site_id for 5 minutes
	if p.redisClient != nil {
		_ = p.redisClient.Set(ctx, redisKey, siteID, 5*time.Minute).Err()
	}

	return siteID, nil
}

// serveObjectWithCacheKey is a thin wrapper around serveObject that uses an
// explicit cache key (e.g. "subdomain:s3key") instead of the raw S3 key, so
// that cross-tenant cache collisions are impossible.
func (p *StaticPlugin) serveObjectWithCacheKey(w http.ResponseWriter, r *http.Request, s3Key, cacheKey string, isFallback bool) error {
	// 1. Check LRU cache (using the scoped cache key)
	if p.cacheTTL > 0 && p.cache != nil {
		if item, ok := p.cache.Get(cacheKey); ok {
			if !item.Exists {
				return fmt.Errorf("cached 404 for key: %s", s3Key)
			}
			if p.RedirectToS3 {
				return p.redirectToS3(w, r, s3Key)
			}
			if p.checkConditionalHeaders(w, r, item) {
				return nil
			}
			if item.Content != nil {
				return p.serveCachedContent(w, r, item)
			}
		}
	}

	// 2. Redirect path
	if p.RedirectToS3 {
		headResult, err := p.s3Client.HeadObject(r.Context(), &s3.HeadObjectInput{
			Bucket: aws.String(p.Bucket),
			Key:    aws.String(s3Key),
		})
		if err != nil {
			if p.isNotFoundError(err) && p.cacheTTL > 0 && p.cache != nil {
				p.cache.Set(cacheKey, &CacheItem{Key: cacheKey, Exists: false}, 1*time.Minute)
			}
			return err
		}
		if p.cacheTTL > 0 && p.cache != nil {
			etag := ""
			if headResult.ETag != nil {
				etag = *headResult.ETag
			}
			lastModified := time.Now()
			if headResult.LastModified != nil {
				lastModified = *headResult.LastModified
			}
			contentType := "application/octet-stream"
			if headResult.ContentType != nil && *headResult.ContentType != "" {
				contentType = *headResult.ContentType
			}
			p.cache.Set(cacheKey, &CacheItem{
				Key:          cacheKey,
				ETag:         etag,
				LastModified: lastModified,
				Size:         *headResult.ContentLength,
				ContentType:  contentType,
				Exists:       true,
			}, p.cacheTTL)
		}
		return p.redirectToS3(w, r, s3Key)
	}

	// 3. Normal streaming / proxy path
	input := &s3.GetObjectInput{
		Bucket: aws.String(p.Bucket),
		Key:    aws.String(s3Key),
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		input.Range = aws.String(rangeHeader)
	}
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		input.IfNoneMatch = aws.String(ifNoneMatch)
	}
	if ifModifiedSince := r.Header.Get("If-Modified-Since"); ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil {
			input.IfModifiedSince = &t
		}
	}

	result, err := p.s3Client.GetObject(r.Context(), input)
	if err != nil {
		if p.isNotModifiedError(err) {
			if p.cacheTTL > 0 && p.cache != nil {
				if item, ok := p.cache.Get(cacheKey); ok {
					p.cache.Set(cacheKey, item, p.cacheTTL)
				}
			}
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
		if p.isNotFoundError(err) && p.cacheTTL > 0 && p.cache != nil {
			p.cache.Set(cacheKey, &CacheItem{Key: cacheKey, Exists: false}, 1*time.Minute)
		}
		return err
	}
	defer result.Body.Close()

	// 4. Extract headers & metadata
	etag := ""
	if result.ETag != nil {
		etag = *result.ETag
	}
	lastModified := time.Now()
	if result.LastModified != nil {
		lastModified = *result.LastModified
	}
	contentType := "application/octet-stream"
	if result.ContentType != nil && *result.ContentType != "" {
		contentType = *result.ContentType
	} else {
		contentType = mime.TypeByExtension(filepath.Ext(s3Key))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	isRangeResponse := result.ContentRange != nil && *result.ContentRange != ""
	size := int64(0)
	if result.ContentLength != nil {
		size = *result.ContentLength
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if isRangeResponse {
		w.Header().Set("Content-Range", *result.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
	}

	// 5. Cache & stream
	canCacheContent := p.cacheTTL > 0 && p.maxCacheSize > 0 && size <= p.maxCacheSize && !isRangeResponse
	if canCacheContent {
		data, readErr := io.ReadAll(result.Body)
		if readErr != nil {
			return readErr
		}
		p.cache.Set(cacheKey, &CacheItem{
			Key:          cacheKey,
			ETag:         etag,
			LastModified: lastModified,
			Size:         size,
			ContentType:  contentType,
			Content:      data,
			Exists:       true,
		}, p.cacheTTL)
		_, writeErr := w.Write(data)
		return writeErr
	}
	if p.cacheTTL > 0 && !isRangeResponse {
		p.cache.Set(cacheKey, &CacheItem{
			Key:          cacheKey,
			ETag:         etag,
			LastModified: lastModified,
			Size:         size,
			ContentType:  contentType,
			Exists:       true,
		}, p.cacheTTL)
	}
	_, writeErr := io.Copy(w, result.Body)
	return writeErr
}

// splitHostPort splits host and port, tolerating missing port.
func splitHostPort(hostport string) (host, port string, err error) {
	host = hostport
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		// Only treat it as host:port if the part after ":" looks like a port number
		possiblePort := hostport[i+1:]
		allDigits := true
		for _, c := range possiblePort {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && len(possiblePort) > 0 {
			host = hostport[:i]
			port = possiblePort
			return host, port, nil
		}
	}
	return host, "", fmt.Errorf("no port")
}

// errNoRows is a package-level alias so handler.go does not import database/sql.
var errNoRows = errSQLNoRows()


// serveObject fetches the file from S3 (or cache) and writes it to the response writer, or redirects the client.
func (p *StaticPlugin) serveObject(w http.ResponseWriter, r *http.Request, key string, isFallback bool) error {
	// 1. Check Cache first (if enabled)
	if p.cacheTTL > 0 && p.cache != nil {
		if item, ok := p.cache.Get(key); ok {
			if !item.Exists {
				return fmt.Errorf("cached 404 for key: %s", key)
			}

			// If redirection is enabled, redirect directly to S3 since we verified the key exists
			if p.RedirectToS3 {
				return p.redirectToS3(w, r, key)
			}

			// Validate conditional headers against cached metadata
			if p.checkConditionalHeaders(w, r, item) {
				return nil
			}

			// If the content is cached in memory, serve it directly
			if item.Content != nil {
				return p.serveCachedContent(w, r, item)
			}
		}
	}

	// 2. Handle S3 Redirection path (avoids downloading file body)
	if p.RedirectToS3 {
		// Verify if the key actually exists in S3 before redirecting (important for SPA fallbacks)
		headResult, err := p.s3Client.HeadObject(r.Context(), &s3.HeadObjectInput{
			Bucket: aws.String(p.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			if p.isNotFoundError(err) {
				if p.cacheTTL > 0 && p.cache != nil {
					// Cache the negative lookup for 1 minute
					p.cache.Set(key, &CacheItem{Key: key, Exists: false}, 1*time.Minute)
				}
			}
			return err
		}

		// Cache file metadata
		if p.cacheTTL > 0 && p.cache != nil {
			etag := ""
			if headResult.ETag != nil {
				etag = *headResult.ETag
			}
			lastModified := time.Now()
			if headResult.LastModified != nil {
				lastModified = *headResult.LastModified
			}
			contentType := "application/octet-stream"
			if headResult.ContentType != nil && *headResult.ContentType != "" {
				contentType = *headResult.ContentType
			}
			p.cache.Set(key, &CacheItem{
				Key:          key,
				ETag:         etag,
				LastModified: lastModified,
				Size:         *headResult.ContentLength,
				ContentType:  contentType,
				Exists:       true,
			}, p.cacheTTL)
		}

		return p.redirectToS3(w, r, key)
	}

	// 3. Normal streaming / proxy path
	input := &s3.GetObjectInput{
		Bucket: aws.String(p.Bucket),
		Key:    aws.String(key),
	}

	// Pass HTTP Range header to S3 if requested
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		input.Range = aws.String(rangeHeader)
	}

	// Map conditional request headers to S3 to save bandwidth
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		input.IfNoneMatch = aws.String(ifNoneMatch)
	}
	if ifModifiedSince := r.Header.Get("If-Modified-Since"); ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil {
			input.IfModifiedSince = &t
		}
	}

	// Request object from S3
	result, err := p.s3Client.GetObject(r.Context(), input)
	if err != nil {
		// Handle 304 Not Modified from S3
		if p.isNotModifiedError(err) {
			// Refresh cache entry expiry
			if p.cacheTTL > 0 && p.cache != nil {
				if item, ok := p.cache.Get(key); ok {
					p.cache.Set(key, item, p.cacheTTL)
				}
			}
			w.WriteHeader(http.StatusNotModified)
			return nil
		}

		// Handle 404 Not Found from S3
		if p.isNotFoundError(err) {
			if p.cacheTTL > 0 && p.cache != nil {
				// Cache the 404 (negative cache) for 1 minute to protect S3
				p.cache.Set(key, &CacheItem{
					Key:    key,
					Exists: false,
				}, 1*time.Minute)
			}
		}
		return err
	}
	defer result.Body.Close()

	// 4. Extract headers & metadata from S3 response
	etag := ""
	if result.ETag != nil {
		etag = *result.ETag
	}
	lastModified := time.Now()
	if result.LastModified != nil {
		lastModified = *result.LastModified
	}

	contentType := "application/octet-stream"
	if result.ContentType != nil && *result.ContentType != "" {
		contentType = *result.ContentType
	} else {
		contentType = mime.TypeByExtension(filepath.Ext(key))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	// Check if this is a range response
	isRangeResponse := result.ContentRange != nil && *result.ContentRange != ""
	size := int64(0)
	if result.ContentLength != nil {
		size = *result.ContentLength
	}

	// Set headers
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")

	if isRangeResponse {
		w.Header().Set("Content-Range", *result.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
	}

	// 5. Caching and streaming
	// Cache full responses if they are small enough
	canCacheContent := p.cacheTTL > 0 && p.maxCacheSize > 0 && size <= p.maxCacheSize && !isRangeResponse

	if canCacheContent {
		data, readErr := io.ReadAll(result.Body)
		if readErr != nil {
			return readErr
		}

		// Set in cache
		item := &CacheItem{
			Key:          key,
			ETag:         etag,
			LastModified: lastModified,
			Size:         size,
			ContentType:  contentType,
			Content:      data,
			Exists:       true,
		}
		p.cache.Set(key, item, p.cacheTTL)

		// Write to response
		_, writeErr := w.Write(data)
		return writeErr
	}

	// Store metadata only (if caching enabled)
	if p.cacheTTL > 0 && !isRangeResponse {
		p.cache.Set(key, &CacheItem{
			Key:          key,
			ETag:         etag,
			LastModified: lastModified,
			Size:         size,
			ContentType:  contentType,
			Exists:       true,
		}, p.cacheTTL)
	}

	// Stream file to response writer (uses constant memory buffer)
	_, writeErr := io.Copy(w, result.Body)
	return writeErr
}

// redirectToS3 redirects the client to the S3 URL (optionally pre-signed).
func (p *StaticPlugin) redirectToS3(w http.ResponseWriter, r *http.Request, key string) error {
	var redirectURL string

	if p.PresignRedirect && p.s3PresignClient != nil {
		// Generate pre-signed URL (local operation)
		presignedReq, err := p.s3PresignClient.PresignGetObject(r.Context(), &s3.GetObjectInput{
			Bucket: aws.String(p.Bucket),
			Key:    aws.String(key),
		}, func(opts *s3.PresignOptions) {
			opts.Expires = p.presignLifetime
		})
		if err != nil {
			return fmt.Errorf("failed to presign s3 redirect URL: %w", err)
		}
		redirectURL = presignedReq.URL
	} else {
		// Format S3 public URL
		if p.Endpoint != "" {
			if p.UsePathStyle != nil && *p.UsePathStyle {
				redirectURL = fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(p.Endpoint, "/"), p.Bucket, key)
			} else {
				u, err := url.Parse(p.Endpoint)
				if err == nil {
					redirectURL = fmt.Sprintf("%s://%s.%s/%s", u.Scheme, p.Bucket, u.Host, key)
				} else {
					redirectURL = fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(p.Endpoint, "/"), p.Bucket, key)
				}
			}
		} else {
			redirectURL = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", p.Bucket, p.Region, key)
		}
	}

	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
	return nil
}

// checkConditionalHeaders checks client headers against cached item. Returns true if 304 served.
func (p *StaticPlugin) checkConditionalHeaders(w http.ResponseWriter, r *http.Request, item *CacheItem) bool {
	// If-None-Match ETag check
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		if ifNoneMatch == "*" || strings.Contains(ifNoneMatch, item.ETag) {
			w.Header().Set("ETag", item.ETag)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}

	// If-Modified-Since check
	if ifModifiedSince := r.Header.Get("If-Modified-Since"); ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil {
			if item.LastModified.Truncate(time.Second).Before(t.Add(1 * time.Second)) {
				w.Header().Set("Last-Modified", item.LastModified.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// serveCachedContent serves cached file content with full Range support using bytes.Reader.
func (p *StaticPlugin) serveCachedContent(w http.ResponseWriter, r *http.Request, item *CacheItem) error {
	w.Header().Set("ETag", item.ETag)
	w.Header().Set("Last-Modified", item.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		// Use http.ServeContent to handle partial range requests on cached memory content safely
		reader := bytes.NewReader(item.Content)
		http.ServeContent(w, r, item.Key, item.LastModified, reader)
		return nil
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", item.Size))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(item.Content)
	return err
}

// isNotFoundError returns true if the error represents a missing object in S3.
func (p *StaticPlugin) isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	errStr := err.Error()
	return strings.Contains(errStr, "NoSuchKey") || strings.Contains(errStr, "404") || strings.Contains(errStr, "StatusCode: 404")
}

// isNotModifiedError returns true if the error is 304 Not Modified from S3.
func (p *StaticPlugin) isNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NotModified" || code == "304"
	}
	errStr := err.Error()
	return strings.Contains(errStr, "304") || strings.Contains(errStr, "NotModified") || strings.Contains(errStr, "StatusCode: 304")
}

// isExcludedFromFallback checks if the file extension should skip fallback routing.
func (p *StaticPlugin) isExcludedFromFallback(path string) bool {
	ext := filepath.Ext(path)
	if ext == "" {
		return false
	}
	for _, excluded := range p.FallbackExcept {
		// support both "png" and ".png"
		cleanEx := strings.TrimPrefix(excluded, ".")
		cleanExt := strings.TrimPrefix(ext, ".")
		if strings.EqualFold(cleanExt, cleanEx) {
			return true
		}
	}
	return false
}

func hasExtension(path string) bool {
	return filepath.Ext(path) != ""
}
