// Package s3: client.go owns the AWS SDK v2 client construction. The
// SDK is intentionally flexible — three credential sources, two
// addressing styles, optional endpoint override — so we centralise
// the translation here. Backend.Put/Get/etc just call into the
// returned client.
//
// Credential precedence:
//  1. Explicit Options.AccessKeyID + SecretAccessKey (wins; same
//     pattern as VORNIK_DATABASE_PASSWORD overriding cfg.yaml).
//  2. SDK default chain (env, ~/.aws, IRSA, EC2 IMDS) — used when
//     Options leaves the credentials empty.
//
// Endpoint precedence:
//  1. Options.Endpoint when non-empty (BaseEndpoint on the SDK
//     client). MinIO + Ceph RGW are configured here.
//  2. SDK default (regional AWS S3 endpoint) — used when Endpoint
//     is empty.
//
// UsePathStyle and ForceSSL surface on the s3.Client's Options
// callback.
package s3

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// buildClient constructs an *s3.Client from Options. Surfaced as a
// package-level var so tests can stub it without hitting the network;
// callers go through New which wires this in.
var buildClient = defaultBuildClient

func defaultBuildClient(ctx context.Context, opts Options) (*s3.Client, error) {
	loadOpts := []func(*awscfg.LoadOptions) error{
		awscfg.WithRegion(opts.Region),
	}
	if opts.AccessKeyID != "" && opts.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		))
	}
	// ForceSSL=false implies the operator wants HTTP — usually MinIO
	// localhost. The SDK trusts the endpoint scheme, so we just need
	// to make sure we don't force TLS verification on a plaintext URL.
	// Skipping cert verification on https endpoints is NOT done here;
	// that's an explicit operator choice they'd configure on the OS.
	if !opts.ForceSSL && strings.HasPrefix(opts.Endpoint, "http://") {
		// Use a non-default HTTP client that does not upgrade scheme.
		// awshttp.NewBuildableClient defaults to keep-alive + 10s timeouts.
		httpClient := awshttp.NewBuildableClient().
			WithTimeout(30 * time.Second).
			WithTransportOptions(func(tr *http.Transport) {
				tr.TLSClientConfig = &tls.Config{}
			})
		loadOpts = append(loadOpts, awscfg.WithHTTPClient(httpClient))
	}

	cfg, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("artifacts/s3: load aws config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if opts.Endpoint != "" {
		ep := opts.Endpoint
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
		})
	}
	if opts.UsePathStyle {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}
	// Force a non-empty region — the SDK refuses to sign requests
	// without one and MinIO accepts whatever we pass.
	clientOpts = append(clientOpts, func(o *s3.Options) {
		if o.Region == "" {
			o.Region = opts.Region
		}
	})

	client := s3.NewFromConfig(cfg, clientOpts...)
	return client, nil
}
