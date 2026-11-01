package results

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct {
	account   string
	accErr    error
	exists    bool
	existsErr error
	createErr error
	created   []string
}

func (f *fakeStore) Account(context.Context) (string, error) { return f.account, f.accErr }
func (f *fakeStore) Exists(context.Context, string, string, string) (bool, error) {
	return f.exists, f.existsErr
}
func (f *fakeStore) Create(_ context.Context, region, bucket string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, region+" "+bucket)
	return nil
}

func TestEnsure(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{account: "123456789012", exists: true}
	b, created, err := Ensure(ctx, f, "us-west-1", false)
	if err != nil || created || b != "amazon-braket-us-west-1-123456789012" {
		t.Fatalf("existing bucket: %s %v %v", b, created, err)
	}
	f = &fakeStore{account: "123456789012"}
	if _, _, err := Ensure(ctx, f, "us-west-1", false); err == nil || !strings.Contains(err.Error(), "<account>") || strings.Contains(err.Error(), "123456789012") {
		t.Errorf("missing, no create: %v (must name the bucket without the account)", err)
	}
	b, created, err = Ensure(ctx, f, "us-west-1", true)
	if err != nil || !created || len(f.created) != 1 || f.created[0] != "us-west-1 "+b {
		t.Errorf("missing, create: %s %v %v %v", b, created, err, f.created)
	}
	for name, f := range map[string]*fakeStore{
		"account error": {accErr: errors.New("AccessDenied for arn:aws:iam::123456789012:user/a")},
		"no account":    {},
		"head error":    {account: "123456789012", existsErr: errors.New("403 amazon-braket-us-west-1-123456789012")},
		"create error":  {account: "123456789012", createErr: errors.New("BucketAlreadyOwnedByYou amazon-braket-us-west-1-123456789012")},
	} {
		_, _, err := Ensure(ctx, f, "us-west-1", true)
		if err == nil || strings.Contains(err.Error(), "123456789012") {
			t.Errorf("%s: %v (want an error with no account ID)", name, err)
		}
	}
	if Display("eu-north-1") != "amazon-braket-eu-north-1-<account>" {
		t.Error("Display")
	}
}

// awsServer stands in for STS and S3. headStatus is HeadBucket's status;
// createCode, if set, is CreateBucket's S3 error code.
func awsServer(t *testing.T, headStatus int, createCode string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" owner="+r.Header.Get("X-Amz-Expected-Bucket-Owner")+" body="+string(body))
		switch {
		case strings.Contains(string(body), "Action=GetCallerIdentity"):
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/x</Arn><UserId>X</UserId></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
		case r.Method == http.MethodHead:
			w.WriteHeader(headStatus)
		case r.Method == http.MethodPut && createCode != "":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `<Error><Code>`+createCode+`</Code><Message>m</Message></Error>`)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func offlineAWS(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "us-west-1",
		"AWS_PROFILE": "", "SPORE_PROFILE": "", "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null"} {
		t.Setenv(k, v)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestAWSStore(t *testing.T) {
	offlineAWS(t)
	ctx := context.Background()
	srv, seen := awsServer(t, http.StatusNotFound, "")
	a := AWS{Endpoint: srv.URL}
	if acct, err := a.Account(ctx); err != nil || acct != "123456789012" {
		t.Fatalf("Account = %q, %v", acct, err)
	}
	if ok, err := a.Exists(ctx, "us-west-1", "b", "123456789012"); err != nil || ok {
		t.Errorf("404 HeadBucket: %v %v; want false, nil", ok, err)
	}
	if err := a.Create(ctx, "us-west-1", "b"); err != nil {
		t.Errorf("Create: %v", err)
	}
	if err := a.Create(ctx, "us-east-1", "b"); err != nil {
		t.Errorf("Create us-east-1: %v", err)
	}
	all := strings.Join(*seen, "\n")
	if !strings.Contains(all, "HEAD /b owner=123456789012") {
		t.Errorf("HeadBucket without ExpectedBucketOwner:\n%s", all)
	}
	if !strings.Contains(all, "<LocationConstraint>us-west-1</LocationConstraint>") {
		t.Errorf("us-west-1 CreateBucket without its location constraint:\n%s", all)
	}
	last := (*seen)[len(*seen)-1]
	if strings.Contains(last, "LocationConstraint") {
		t.Errorf("us-east-1 CreateBucket sent a location constraint: %s", last)
	}

	srv200, _ := awsServer(t, http.StatusOK, "BucketAlreadyOwnedByYou")
	b := AWS{Endpoint: srv200.URL}
	if ok, err := b.Exists(ctx, "us-west-1", "b", "123456789012"); err != nil || !ok {
		t.Errorf("200 HeadBucket: %v %v", ok, err)
	}
	if err := b.Create(ctx, "us-west-1", "b"); err != nil {
		t.Errorf("BucketAlreadyOwnedByYou must be success: %v", err)
	}
	srv403, _ := awsServer(t, http.StatusForbidden, "BucketAlreadyExists")
	c := AWS{Endpoint: srv403.URL}
	if _, err := c.Exists(ctx, "us-west-1", "b", "123456789012"); err == nil {
		t.Error("403 HeadBucket (another account's bucket) was not an error")
	}
	if err := c.Create(ctx, "us-west-1", "b"); err == nil {
		t.Error("BucketAlreadyExists (another account) was not an error")
	}
}
