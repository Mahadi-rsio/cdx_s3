package cdx_s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (p *StaticPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	path := r.URL.Path

	// Standard SPA rule: serving index.html if empty path or root
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

// serveObject fetches the file from S3 (or cache) and writes it to the response writer.
func (p *StaticPlugin) serveObject(w http.ResponseWriter, r *http.Request, key string, isFallback bool) error {
	// 1. Check Cache first (if enabled)
	if p.cacheTTL > 0 && p.cache != nil {
		if item, ok := p.cache.Get(key); ok {
			if !item.Exists {
				return fmt.Errorf("cached 404 for key: %s", key)
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

	// 2. Build S3 GetObject Input
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

	// 3. Request object from S3
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
