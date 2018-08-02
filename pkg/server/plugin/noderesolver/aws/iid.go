package aws

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/hashicorp/hcl"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/spire/pkg/common/idutil"
	"github.com/spiffe/spire/proto/common"
	spi "github.com/spiffe/spire/proto/common/plugin"
	"github.com/spiffe/spire/proto/server/noderesolver"
	"github.com/zeebo/errs"
)

const (
	pluginName    = "aws_iid"
	defaultRegion = "us-east-1"
)

var (
	iidError = errs.Class("aws-iid")

	reAgentIDPath = regexp.MustCompile(`^/spire/agent/aws_iid/([^/]+)/([^/]+)/([^/]+)$`)

	instanceFilters = []*ec2.Filter{
		{
			Name: aws.String("instance-state-name"),
			Values: []*string{
				aws.String("pending"),
				aws.String("running"),
			},
		},
	}
)

type awsClient interface {
	DescribeInstancesWithContext(aws.Context, *ec2.DescribeInstancesInput, ...request.Option) (*ec2.DescribeInstancesOutput, error)
	GetInstanceProfileWithContext(aws.Context, *iam.GetInstanceProfileInput, ...request.Option) (*iam.GetInstanceProfileOutput, error)
}

type IIDResolverConfig struct {
	AccessKeyID     string `hcl:"access_key_id"`
	SecretAccessKey string `hcl:"secret_access_key"`
}

type IIDResolverPlugin struct {
	mu      sync.RWMutex
	conf    *aws.Config
	clients map[string]awsClient

	hooks struct {
		getenv    func(string) string
		newClient func(conf *aws.Config) (awsClient, error)
	}
}

var _ noderesolver.Plugin = (*IIDResolverPlugin)(nil)

func NewIIDResolverPlugin() *IIDResolverPlugin {
	p := &IIDResolverPlugin{}
	p.hooks.getenv = os.Getenv
	p.hooks.newClient = newAWSClient
	return p
}

func (p *IIDResolverPlugin) Configure(ctx context.Context, req *spi.ConfigureRequest) (*spi.ConfigureResponse, error) {
	// Parse HCL config payload into config struct
	config := new(IIDResolverConfig)
	if err := hcl.Decode(config, req.Configuration); err != nil {
		return nil, iidError.New("unable to decode configuration: %v", err)
	}

	// Set defaults from the environment
	if config.AccessKeyID == "" {
		config.AccessKeyID = p.hooks.getenv("AWS_ACCESS_KEY_ID")
	}
	if config.SecretAccessKey == "" {
		config.SecretAccessKey = p.hooks.getenv("AWS_SECRET_ACCESS_KEY")
	}

	conf := aws.NewConfig()
	switch {
	case config.AccessKeyID != "" && config.SecretAccessKey != "":
		conf.Credentials = credentials.NewStaticCredentials(config.AccessKeyID, config.SecretAccessKey, "")
	case config.AccessKeyID != "" && config.SecretAccessKey == "":
		return nil, iidError.New("configuration missing secret access key")
	case config.AccessKeyID == "" && config.SecretAccessKey != "":
		return nil, iidError.New("configuration missing access key id")
	case config.AccessKeyID == "" && config.SecretAccessKey == "":
		return nil, iidError.New("configuration missing both access key id and secret access key")
	}

	// set the AWS configuration and reset clients
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conf = conf
	p.clients = make(map[string]awsClient)
	return &spi.ConfigureResponse{}, nil
}

func (p *IIDResolverPlugin) GetPluginInfo(context.Context, *spi.GetPluginInfoRequest) (*spi.GetPluginInfoResponse, error) {
	return &spi.GetPluginInfoResponse{}, nil
}

func (p *IIDResolverPlugin) Resolve(ctx context.Context, req *noderesolver.ResolveRequest) (*noderesolver.ResolveResponse, error) {
	resp := &noderesolver.ResolveResponse{
		Map: make(map[string]*common.Selectors),
	}
	for _, spiffeID := range req.BaseSpiffeIdList {
		selectors, err := p.resolveSpiffeID(ctx, spiffeID)
		if err != nil {
			return nil, err
		}
		if selectors != nil {
			resp.Map[spiffeID] = selectors
		}
	}
	return resp, nil
}

func (p *IIDResolverPlugin) resolveSpiffeID(ctx context.Context, spiffeID string) (*common.Selectors, error) {
	_, region, instanceID, err := parseAgentID(spiffeID)
	if err != nil {
		logrus.Warnf("unrecognized Agent ID: %s: %v", spiffeID, err)
		return nil, nil
	}

	client, err := p.getRegionClient(region)
	if err != nil {
		return nil, err
	}

	resp, err := client.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
		Filters:     instanceFilters,
	})
	if err != nil {
		return nil, iidError.Wrap(err)
	}

	selectorSet := map[string]bool{}
	addSelectors := func(values []string) {
		for _, value := range values {
			selectorSet[value] = true
		}
	}

	for _, reservation := range resp.Reservations {
		for _, instance := range reservation.Instances {
			addSelectors(resolveTags(instance.Tags))
			addSelectors(resolveSecurityGroups(instance.SecurityGroups))
			if instance.IamInstanceProfile != nil && instance.IamInstanceProfile.Arn != nil {
				output, err := client.GetInstanceProfileWithContext(ctx, &iam.GetInstanceProfileInput{
					InstanceProfileName: instance.IamInstanceProfile.Arn,
				})
				if err != nil {
					return nil, iidError.Wrap(err)
				}
				addSelectors(resolveInstanceProfile(output.InstanceProfile))
			}
		}
	}

	// sort and dedup selectors
	values := make([]string, 0, len(selectorSet))
	for value := range selectorSet {
		values = append(values, value)
	}
	sort.Strings(values)

	selectors := new(common.Selectors)
	for _, value := range values {
		selectors.Entries = append(selectors.Entries, &common.Selector{
			Type:  pluginName,
			Value: value,
		})
	}

	return selectors, nil
}

func (p *IIDResolverPlugin) getRegionClient(region string) (awsClient, error) {
	// do an initial check to see if p client for this region already exists
	p.mu.RLock()
	client, ok := p.clients[region]
	p.mu.RUnlock()
	if ok {
		return client, nil
	}

	// no client for this region. make one.
	p.mu.Lock()
	defer p.mu.Unlock()

	// more than one thread could be racing to create p client (since we had
	// to drop the read lock to take the write lock), so double check somebody
	// hasn't beat us to it.
	client, ok = p.clients[region]
	if ok {
		return client, nil
	}

	if p.conf == nil {
		return nil, iidError.New("not configured")
	}
	p.conf.Region = aws.String(region)

	client, err := p.hooks.newClient(p.conf)
	if err != nil {
		return nil, err
	}

	p.clients[region] = client
	return client, nil
}

func resolveTags(tags []*ec2.Tag) []string {
	values := make([]string, 0, len(tags))
	for _, tag := range tags {
		values = append(values, fmt.Sprintf("tag:%s:%s", aws.StringValue(tag.Key), aws.StringValue(tag.Value)))
	}
	return values
}

func resolveSecurityGroups(sgs []*ec2.GroupIdentifier) []string {
	values := make([]string, 0, len(sgs)*2)
	for _, sg := range sgs {
		values = append(values,
			fmt.Sprintf("sg:id:%s", aws.StringValue(sg.GroupId)),
			fmt.Sprintf("sg:name:%s", aws.StringValue(sg.GroupName)),
		)
	}
	return values
}

func resolveInstanceProfile(instanceProfile *iam.InstanceProfile) []string {
	values := make([]string, 0, len(instanceProfile.Roles))
	for _, role := range instanceProfile.Roles {
		if role.Arn != nil {
			values = append(values, fmt.Sprintf("iamrole:%s", aws.StringValue(role.Arn)))
		}
	}
	return values
}

func parseAgentID(spiffeID string) (accountID, region, instanceId string, err error) {
	u, err := idutil.ParseSpiffeID(spiffeID, idutil.AllowAnyTrustDomainAgent())
	if err != nil {
		return "", "", "", errs.New("unable to parse agent id %q: %v", spiffeID, err)
	}
	m := reAgentIDPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", "", errs.New("malformed agent id %q", spiffeID)
	}
	return m[1], m[2], m[3], nil
}

func newAWSClient(conf *aws.Config) (awsClient, error) {
	sess, err := session.NewSession(conf)
	if err != nil {
		return nil, iidError.Wrap(err)
	}

	return struct {
		*iam.IAM
		*ec2.EC2
	}{
		IAM: iam.New(sess),
		EC2: ec2.New(sess),
	}, nil
}