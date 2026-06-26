package cdx_s3

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func init() {
	caddy.RegisterModule(StaticPlugin{})
	httpcaddyfile.RegisterHandlerDirective("static_s3", parseCaddyfile)
}

type StaticPlugin struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Region    string `json:"region"`

	s3Client *s3.Client
}

func (StaticPlugin) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.static_s3",
		New: func() caddy.Module { return new(StaticPlugin) },
	}
}

func (p *StaticPlugin) Provision(ctx caddy.Context) error {
	// এনভায়রনমেন্ট ভেরিয়েবল থেকে ক্রেডেনশিয়াল নেওয়া (যদি Caddyfile-এ না থাকে)
	if p.AccessKey == "" {
		p.AccessKey = os.Getenv("S3_ACCESS_KEY")
	}
	if p.SecretKey == "" {
		p.SecretKey = os.Getenv("S3_SECRET_KEY")
	}
	if p.Region == "" {
		p.Region = "us-east-1"
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(p.Region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(p.AccessKey, p.SecretKey, ""),
		),
	)
	if err != nil {
		return fmt.Errorf("static_s3: aws config failed: %w", err)
	}

	// MinIO/S3 এর জন্য ক্লায়েন্ট তৈরি
	p.s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(p.Endpoint)
		o.UsePathStyle = true
	})

	return nil
}

func (p *StaticPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	path := r.URL.Path
	
	// SPA রাউটিং: রুট পাথ অথবা কোনো এক্সটেনশন ছাড়া পাথ হলে index.html দেখাবে
	if path == "/" || path == "" || !hasExtension(path) {
		return p.serveObject(w, r, "index.html")
	}
	
	return p.serveObject(w, r, strings.TrimPrefix(path, "/"))
}

func (p *StaticPlugin) serveObject(w http.ResponseWriter, r *http.Request, key string) error {
	result, err := p.s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(p.Bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		// ফাইল না পেলে SPA এর জন্য index.html এ ফলব্যাক
		if key != "index.html" && (strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "404")) {
			return p.serveObject(w, r, "index.html")
		}
		return caddyhttp.Error(http.StatusNotFound, err)
	}
	defer result.Body.Close()

	// S3 এর Body (io.ReadCloser) কে মেমোরিতে এনে bytes.Reader (io.ReadSeeker) বানানো
	data, err := io.ReadAll(result.Body)
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	reader := bytes.NewReader(data)

	// MIME টাইপ নির্ধারণ
	contentType := "application/octet-stream"
	if result.ContentType != nil {
		contentType = *result.ContentType
	} else {
		contentType = mime.TypeByExtension(filepath.Ext(key))
	}
	w.Header().Set("Content-Type", contentType)

	// Last-Modified টাইম সেট করা (যদি S3 তে না থাকে তবে বর্তমান সময়)
	lastModified := time.Now()
	if result.LastModified != nil {
		lastModified = *result.LastModified
	}

	// Range request সাপোর্ট সহ ফাইল সার্ভ করা
	http.ServeContent(w, r, key, lastModified, reader)
	return nil
}

func hasExtension(path string) bool {
	return filepath.Ext(path) != ""
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var p StaticPlugin
	err := p.UnmarshalCaddyfile(h.Dispenser)
	return &p, err
}

func (p *StaticPlugin) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "endpoint":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.Endpoint = d.Val()
			case "bucket":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.Bucket = d.Val()
			case "access_key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.AccessKey = d.Val()
			case "secret_key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.SecretKey = d.Val()
			case "region":
				if !d.NextArg() {
					return d.ArgErr()
				}
				p.Region = d.Val()
			}
		}
	}
	return nil
}


