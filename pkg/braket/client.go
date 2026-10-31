// Package braket is puff's client layer over the Amazon Braket API: read-only
// interfaces, region discovery, fan-out of list calls to every region, and the
// action JSON builders.
//
// Billable operations are not reachable from the read interfaces. [API] has no
// Create method, and [NewFactory] hands out wrappers rather than SDK clients,
// so a type assertion cannot recover one. [Creator] is the billable interface;
// only pkg/gate holds one (law 1).
package braket

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/spore-host/puffball/pkg/awscfg"
)

// API is the read-only subset of the Braket service puff uses.
type API interface {
	SearchDevices(ctx context.Context, in *braket.SearchDevicesInput, opts ...func(*braket.Options)) (*braket.SearchDevicesOutput, error)
	GetDevice(ctx context.Context, in *braket.GetDeviceInput, opts ...func(*braket.Options)) (*braket.GetDeviceOutput, error)
	SearchQuantumTasks(ctx context.Context, in *braket.SearchQuantumTasksInput, opts ...func(*braket.Options)) (*braket.SearchQuantumTasksOutput, error)
	GetQuantumTask(ctx context.Context, in *braket.GetQuantumTaskInput, opts ...func(*braket.Options)) (*braket.GetQuantumTaskOutput, error)
}

// Creator is the billable subset of the Braket service. Only pkg/gate may hold
// one (law 1).
type Creator interface {
	CreateQuantumTask(ctx context.Context, in *braket.CreateQuantumTaskInput, opts ...func(*braket.Options)) (*braket.CreateQuantumTaskOutput, error)
}

// ParameterReader reads SSM parameters by path; used for the public Braket
// region list.
type ParameterReader interface {
	GetParametersByPath(ctx context.Context, in *ssm.GetParametersByPathInput, opts ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// Factory hands out per-region read clients.
type Factory interface {
	// Seed is the region the first discovery call (the SSM read) goes to.
	Seed() string
	// Braket returns the read client for region.
	Braket(ctx context.Context, region string) (API, error)
	// Parameters returns an SSM reader for region.
	Parameters(ctx context.Context, region string) (ParameterReader, error)
}

// readClient exposes only the read methods of an SDK client.
type readClient struct{ c *braket.Client }

func (r readClient) SearchDevices(ctx context.Context, in *braket.SearchDevicesInput, opts ...func(*braket.Options)) (*braket.SearchDevicesOutput, error) {
	return r.c.SearchDevices(ctx, in, opts...)
}

func (r readClient) GetDevice(ctx context.Context, in *braket.GetDeviceInput, opts ...func(*braket.Options)) (*braket.GetDeviceOutput, error) {
	return r.c.GetDevice(ctx, in, opts...)
}

func (r readClient) SearchQuantumTasks(ctx context.Context, in *braket.SearchQuantumTasksInput, opts ...func(*braket.Options)) (*braket.SearchQuantumTasksOutput, error) {
	return r.c.SearchQuantumTasks(ctx, in, opts...)
}

func (r readClient) GetQuantumTask(ctx context.Context, in *braket.GetQuantumTaskInput, opts ...func(*braket.Options)) (*braket.GetQuantumTaskOutput, error) {
	return r.c.GetQuantumTask(ctx, in, opts...)
}

// awsFactory builds clients from the shared spore.host config, one aws.Config
// per region, cached.
type awsFactory struct {
	seed string
	mu   sync.Mutex
	cfgs map[string]aws.Config
}

// NewFactory returns a Factory backed by the AWS SDK. The seed region is the
// shared config's region (flag > env > config file); it is an error if none
// is set, since discovery has to start somewhere.
func NewFactory(ctx context.Context) (Factory, error) {
	cfg, err := awscfg.Load(ctx, "")
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region configured to start Braket discovery: set --region, AWS_REGION, or the profile's region")
	}
	return &awsFactory{seed: cfg.Region, cfgs: map[string]aws.Config{cfg.Region: cfg}}, nil
}

func (f *awsFactory) Seed() string { return f.seed }

func (f *awsFactory) config(ctx context.Context, region string) (aws.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg, ok := f.cfgs[region]; ok {
		return cfg, nil
	}
	cfg, err := awscfg.Load(ctx, region)
	if err != nil {
		return aws.Config{}, err
	}
	f.cfgs[region] = cfg
	return cfg, nil
}

func (f *awsFactory) Braket(ctx context.Context, region string) (API, error) {
	cfg, err := f.config(ctx, region)
	if err != nil {
		return nil, err
	}
	return readClient{braket.NewFromConfig(cfg)}, nil
}

func (f *awsFactory) Parameters(ctx context.Context, region string) (ParameterReader, error) {
	cfg, err := f.config(ctx, region)
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}
