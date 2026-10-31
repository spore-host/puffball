package braket

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"

	"github.com/spore-host/puffball/pkg/awscfg"
)

// TestReadClientDelegates runs every read method against a local server and
// checks the request reached the right operation. No AWS endpoint is called.
func TestReadClientDelegates(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := readClient{braket.New(braket.Options{
		Region:       "us-west-1",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
	})}
	ctx := context.Background()
	if _, err := c.SearchDevices(ctx, &braket.SearchDevicesInput{Filters: []types.SearchDevicesFilter{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDevice(ctx, &braket.GetDeviceInput{DeviceArn: aws.String("arn:aws:braket:::device/quantum-simulator/amazon/sv1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchQuantumTasks(ctx, &braket.SearchQuantumTasksInput{Filters: []types.SearchQuantumTasksFilter{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetQuantumTask(ctx, &braket.GetQuantumTaskInput{QuantumTaskArn: aws.String("arn:aws:braket:us-west-1:000000000000:quantum-task/x")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /devices", "GET /device/", "POST /quantum-tasks", "GET /quantum-task/"}
	if len(paths) != len(want) {
		t.Fatalf("requests = %v", paths)
	}
	for i, w := range want {
		if !strings.HasPrefix(paths[i], w) {
			t.Errorf("request %d = %q, want prefix %q", i, paths[i], w)
		}
	}
}

// TestFactoryHandsOutWrappers: the factory never returns a raw SDK client, so
// a type assertion can't reach CreateQuantumTask (law 1). Building clients
// makes no network call.
func TestFactoryHandsOutWrappers(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("SPORE_PROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Cleanup(func() { awscfg.SetFlags("", "") })
	awscfg.SetFlags("", "")
	ctx := context.Background()
	f, err := NewFactory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Seed() != "us-west-2" {
		t.Errorf("Seed() = %q", f.Seed())
	}
	api, err := f.Braket(ctx, "us-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := api.(Creator); ok {
		t.Fatal("Factory.Braket returned something that satisfies Creator")
	}
	if _, ok := api.(*braket.Client); ok {
		t.Fatal("Factory.Braket returned the raw SDK client")
	}
	again, _ := f.Braket(ctx, "us-west-1")
	if again == nil {
		t.Fatal("second Braket call returned nil")
	}
	if _, err := f.Parameters(ctx, "us-west-2"); err != nil {
		t.Fatal(err)
	}
}

func TestNewFactoryNeedsARegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("SPORE_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("SPORE_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	awscfg.SetFlags("", "")
	if _, err := NewFactory(context.Background()); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("err = %v, want a missing-region error", err)
	}
}
