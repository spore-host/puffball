// Package results finds or creates the Braket results bucket for a region:
// the SDK's default name, amazon-braket-<region>-<account>, with puffball's
// results under puffball/. It never creates any other bucket, and it creates
// the default one only when asked (puff does so only on a --real run).
//
// The account ID is used to name the bucket and is never printed: Display
// shows "<account>" in its place (law 7).
package results

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/spore-host/puffball/internal/scrub"
	"github.com/spore-host/puffball/pkg/awscfg"
)

// Prefix is where puffball's results go in the bucket.
const Prefix = "puffball/"

// Store is what puff needs from STS and S3 for results.
type Store interface {
	// Account returns the caller's account ID.
	Account(ctx context.Context) (string, error)
	// Exists reports whether bucket exists and is owned by account.
	Exists(ctx context.Context, region, bucket, account string) (bool, error)
	// Create creates bucket in region.
	Create(ctx context.Context, region, bucket string) error
	// PutNew writes key in bucket, owned by account, only if no object is
	// there yet, so a retry never replaces what the first attempt wrote.
	// created is false when one was already there.
	PutNew(ctx context.Context, region, bucket, key, account string, body []byte) (created bool, err error)
}

// BucketName is the SDK's default results bucket for region and account.
func BucketName(region, account string) string {
	return fmt.Sprintf("amazon-braket-%s-%s", region, account)
}

// Display is the bucket name with the account hidden.
func Display(region string) string { return BucketName(region, "<account>") }

// Ensure returns the default results bucket for region. If it doesn't exist
// it is created only when create is true; created reports that it was.
func Ensure(ctx context.Context, s Store, region string, create bool) (bucket string, created bool, err error) {
	acct, err := s.Account(ctx)
	if err != nil {
		return "", false, scrub.Error(fmt.Errorf("account for the results bucket: %w", err))
	}
	if acct == "" {
		return "", false, errors.New("account for the results bucket: STS returned none")
	}
	bucket = BucketName(region, acct)
	ok, err := s.Exists(ctx, region, bucket, acct)
	if err != nil {
		return "", false, scrub.Error(fmt.Errorf("results bucket %s: %w", Display(region), err))
	}
	if ok {
		return bucket, false, nil
	}
	if !create {
		return "", false, fmt.Errorf("results bucket %s does not exist; puff creates it on a --real run", Display(region))
	}
	if err := s.Create(ctx, region, bucket); err != nil {
		return "", false, scrub.Error(fmt.Errorf("create results bucket %s: %w", Display(region), err))
	}
	return bucket, true, nil
}

// AWS is the Store backed by STS and S3, through the shared spore.host config.
type AWS struct {
	// Endpoint, if set, sends every call there instead of AWS (tests).
	Endpoint string
}

func (a AWS) config(ctx context.Context, region string) (aws.Config, error) {
	cfg, err := awscfg.Load(ctx, region)
	if err != nil {
		return aws.Config{}, err
	}
	if a.Endpoint != "" {
		cfg.BaseEndpoint = aws.String(a.Endpoint)
	}
	return cfg, nil
}

func (a AWS) s3(cfg aws.Config) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = a.Endpoint != "" })
}

// Account calls STS GetCallerIdentity.
func (a AWS) Account(ctx context.Context) (string, error) {
	cfg, err := a.config(ctx, "")
	if err != nil {
		return "", err
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("GetCallerIdentity: %w", err)
	}
	return aws.ToString(out.Account), nil
}

// Exists calls S3 HeadBucket with ExpectedBucketOwner, so a same-named
// bucket in another account is never taken for ours (S3 answers 403). A 404
// is false (measured: G0(c), #4, and the 2026-11-01 us-west-2 run created
// the bucket through this path); any other error is an error.
func (a AWS) Exists(ctx context.Context, region, bucket, account string) (bool, error) {
	cfg, err := a.config(ctx, region)
	if err != nil {
		return false, err
	}
	_, err = a.s3(cfg).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(account)})
	var nf *s3types.NotFound
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &nf):
		return false, nil
	default:
		return false, fmt.Errorf("HeadBucket: %w", err)
	}
}

// Create calls S3 CreateBucket with the region's location constraint
// (us-east-1 takes none), as G0(c) did (#4). BucketAlreadyOwnedByYou is
// success: another run created it first.
func (a AWS) Create(ctx context.Context, region, bucket string) error {
	cfg, err := a.config(ctx, region)
	if err != nil {
		return err
	}
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraint(region)}
	}
	_, err = a.s3(cfg).CreateBucket(ctx, in)
	var mine *s3types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &mine) {
		return fmt.Errorf("CreateBucket: %w", err)
	}
	return nil
}

// Reader reads result objects, such as a task's results.json.
type Reader interface {
	// Read returns bucket/key from region, refusing a bucket account doesn't
	// own and an object larger than limit bytes ([ErrTooLarge]).
	Read(ctx context.Context, region, bucket, key, account string, limit int64) (Object, error)
}

// Object is a result object as read: its bytes as stored, and its ETag.
type Object struct {
	Body []byte
	ETag string
}

// ErrNotFound is returned by Read when there is no such object.
var ErrNotFound = errors.New("no such object")

// ErrTooLarge is returned by Read for an object over its limit.
var ErrTooLarge = errors.New("result object is larger than puff reads")

// Read calls S3 GetObject with ExpectedBucketOwner, so an object in a
// same-named bucket of another account is never read (S3 answers 403).
func (a AWS) Read(ctx context.Context, region, bucket, key, account string, limit int64) (Object, error) {
	cfg, err := a.config(ctx, region)
	if err != nil {
		return Object{}, err
	}
	out, err := a.s3(cfg).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ExpectedBucketOwner: aws.String(account)})
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, fmt.Errorf("GetObject: %w", err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return Object{}, fmt.Errorf("GetObject: read body: %w", err)
	}
	if int64(len(b)) > limit {
		return Object{}, fmt.Errorf("%w (%d bytes)", ErrTooLarge, limit)
	}
	return Object{Body: b, ETag: aws.ToString(out.ETag)}, nil
}

// PutNew calls S3 PutObject with If-None-Match: * and ExpectedBucketOwner.
// S3 answers 412 PreconditionFailed when the key exists; that is
// created=false, not an error.
func (a AWS) PutNew(ctx context.Context, region, bucket, key, account string, body []byte) (bool, error) {
	cfg, err := a.config(ctx, region)
	if err != nil {
		return false, err
	}
	_, err = a.s3(cfg).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: bytes.NewReader(body), ContentType: aws.String("application/json"),
		IfNoneMatch: aws.String("*"), ExpectedBucketOwner: aws.String(account)})
	var re interface{ HTTPStatusCode() int }
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed:
		return false, nil
	default:
		return false, fmt.Errorf("PutObject: %w", err)
	}
}
