package aws

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	crand "crypto/rand"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/convox/rack/pkg/cache"
	"github.com/convox/rack/pkg/manifest1"
	"github.com/convox/rack/pkg/structs"
	docker "github.com/fsouza/go-dockerclient"
)

type Formation struct {
	Parameters map[string]FormationParameter
	Resources  map[string]FormationResource
}

type FormationParameter struct {
	Default     interface{}
	Description string
	NoEcho      bool
	Type        string
}

type FormationResource struct {
	Type       string
	Properties map[string]interface{}
}

func (p *Provider) accountId() (string, error) {
	res, err := p.sts().GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}

	return *res.Account, nil
}

func awsError(err error) string {
	if ae, ok := err.(awserr.Error); ok {
		return ae.Code()
	}

	return ""
}

func camelize(dasherized string) string {
	tokens := strings.Split(dasherized, "-")

	for i, token := range tokens {
		switch token {
		case "az":
			tokens[i] = "AZ"
		default:
			tokens[i] = strings.Title(token)
		}
	}

	return strings.Join(tokens, "")
}

func certificateFriendlyId(arn string) string {
	ap := strings.Split(arn, ":")

	if len(ap) < 6 {
		return ""
	}

	switch ap[2] {
	case "acm":
		np := strings.Split(ap[5], "-")

		return fmt.Sprintf("acm-%s", np[len(np)-1])
	case "iam":
		np := strings.SplitN(ap[5], "/", 2)

		if len(np) < 2 {
			return ""
		}

		return np[1]
	}

	return ""
}

func cfParams(source map[string]string) map[string]string {
	params := make(map[string]string)

	for key, value := range source {
		var val string
		switch value {
		case "":
			val = "false"
		case "true":
			val = "true"
		default:
			val = value
		}
		params[camelize(key)] = val
	}

	return params
}

func coalesce(s *dynamodb.AttributeValue, def string) string {
	if s != nil {
		return *s.S
	}
	return def
}

func coalesces(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}

	return ""
}

func cb(b *bool, def bool) bool {
	if b != nil {
		return *b
	}
	return def
}

func ci(i *int64, def int64) int64 {
	if i != nil {
		return *i
	}
	return def
}

func cs(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

func ct(t *time.Time, def time.Time) time.Time {
	if t != nil {
		return *t
	}
	return def
}

func generation(g *string) string {
	if g == nil {
		return "2"
	}

	if *g == "" {
		return "2"
	}

	return *g
}

var idAlphabet = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ")

func generateId(prefix string, size int) string {
	b := make([]rune, size)
	for i := range b {
		b[i] = idAlphabet[rand.Intn(len(idAlphabet))]
	}
	return prefix + string(b)
}

func buildTemplate(name, section string, data interface{}) (string, error) {
	d, err := ioutil.ReadFile(fmt.Sprintf("provider/aws/templates/%s.tmpl", name))
	if err != nil {
		return "", err
	}

	tmpl, err := template.New(section).Funcs(templateHelpers()).Parse(string(d))
	if err != nil {
		return "", err
	}

	var formation bytes.Buffer

	err = tmpl.Execute(&formation, data)
	if err != nil {
		return "", err
	}

	return formation.String(), nil
}

func (p *Provider) createdTime() string {
	if p.IsTest() {
		return time.Time{}.Format(sortableTime)
	}

	return time.Now().Format(sortableTime)
}

func formationParameters(data []byte) (map[string]bool, error) {
	f, err := parseFormation(data)
	if err != nil {
		return nil, err
	}

	params := map[string]bool{}

	for key := range f.Parameters {
		params[key] = true
	}

	return params, nil
}

func humanStatus(original string) string {
	switch original {
	case "":
		return "new"
	case "CREATE_IN_PROGRESS":
		return "creating"
	case "CREATE_COMPLETE":
		return "running"
	case "DELETE_FAILED":
		return "running"
	case "DELETE_IN_PROGRESS":
		return "deleting"
	case "ROLLBACK_IN_PROGRESS":
		return "rollback"
	case "ROLLBACK_COMPLETE":
		return "failed"
	case "UPDATE_IN_PROGRESS":
		return "updating"
	case "UPDATE_COMPLETE_CLEANUP_IN_PROGRESS":
		return "updating"
	case "UPDATE_COMPLETE":
		return "running"
	case "UPDATE_ROLLBACK_IN_PROGRESS":
		return "rollback"
	case "UPDATE_ROLLBACK_COMPLETE_CLEANUP_IN_PROGRESS":
		return "rollback"
	case "UPDATE_ROLLBACK_COMPLETE":
		return "running"
	case "UPDATE_ROLLBACK_FAILED":
		return "failed"
	default:
		fmt.Printf("unknown status: %s\n", original)
		return "unknown"
	}
}

func parseFormation(data []byte) (*Formation, error) {
	var f Formation

	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}

	return &f, nil
}

func taskStatus(original string) string {
	return strings.ToLower(original)
}

func lastline(data []byte) string {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return lines[len(lines)-1]
}

var randomAlphabet = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

func randomString(size int) string {
	b := make([]rune, size)
	for i := range b {
		b[i] = randomAlphabet[rand.Intn(len(randomAlphabet))]
	}
	return string(b)
}

func recoverWith(f func(err error)) {
	if r := recover(); r != nil {
		// coerce r to error type
		err, ok := r.(error)
		if !ok {
			err = fmt.Errorf("%v", r)
		}

		f(err)
	}
}

func remarshal(v interface{}, w interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	return json.Unmarshal(data, &w)
}

func retry(times int, interval time.Duration, fn func() error) error {
	i := 0

	for {
		err := fn()
		if err == nil {
			return nil
		}

		// add 20% jitter
		time.Sleep(interval + time.Duration(rand.Intn(int(interval/20))))

		i++

		if i > times {
			return err
		}
	}
}

func stackName(app *structs.App) string {
	if _, ok := app.Tags["Rack"]; ok {
		return fmt.Sprintf("%s-%s", app.Tags["Rack"], app.Name)
	}

	return app.Name
}

func (p *Provider) rackStack(name string) string {
	return fmt.Sprintf("%s-%s", p.Rack, name)
}

func stackParameters(stack *cloudformation.Stack) map[string]string {
	parameters := make(map[string]string)

	for _, parameter := range stack.Parameters {
		parameters[*parameter.ParameterKey] = *parameter.ParameterValue
	}

	return parameters
}

func stackOutputs(stack *cloudformation.Stack) map[string]string {
	outputs := make(map[string]string)

	for _, output := range stack.Outputs {
		outputs[*output.OutputKey] = *output.OutputValue
	}

	return outputs
}

func stackTags(stack *cloudformation.Stack) map[string]string {
	tags := make(map[string]string)

	for _, tag := range stack.Tags {
		tags[*tag.Key] = *tag.Value
	}

	return tags
}

func templateHelpers() template.FuncMap {
	return template.FuncMap{
		"safe": func(s string) template.HTML {
			return template.HTML(fmt.Sprintf("%q", s))
		},
		"upper": func(s string) string {
			return upperName(s)
		},
		"value": func(s string) template.HTML {
			return template.HTML(fmt.Sprintf("%q", s))
		},
	}
}

func volumeFrom(app, s string) string {
	parts := strings.SplitN(s, ":", 2)

	switch v := parts[0]; v {
	case "/cgroup/":
		return v
	case "/dev/log":
		return v
	case "/etc/passwd":
		return v
	case "/proc/":
		return v
	case "/sys/fs/cgroup/":
		return v
	case "/sys/kernel/debug/":
		return v
	case "/var/log/audit/":
		return v
	case "/var/run/":
		return v
	case "/var/run/docker.sock":
		return v
	default:
		return path.Join("/volumes", app, v)
	}
}

func volumeTo(s string) string {
	parts := strings.SplitN(s, ":", 2)
	switch len(parts) {
	case 1:
		return s
	case 2:
		return parts[1]
	}
	return fmt.Sprintf("invalid volume %q", s)
}

func dashName(name string) string {
	// Myapp -> myapp; MyApp -> my-app
	re := regexp.MustCompile("([a-z])([A-Z])") // lower case letter followed by upper case

	k := re.ReplaceAllString(name, "${1}-${2}")
	return strings.ToLower(k)
}

func upperName(name string) string {
	if name == "" {
		return ""
	}

	// replace underscores with dashes
	name = strings.Replace(name, "_", "-", -1)

	// myapp -> Myapp; my-app -> MyApp
	us := strings.ToUpper(name[0:1]) + name[1:]

	for {
		i := strings.Index(us, "-")

		if i == -1 {
			break
		}

		s := us[0:i]

		if len(us) > i+1 {
			s += strings.ToUpper(us[i+1 : i+2])
		}

		if len(us) > i+2 {
			s += us[i+2:]
		}

		us = s
	}

	return us
}

/****************************************************************************
 * AWS API HELPERS
 ****************************************************************************/

func (p *Provider) createStack(name string, body []byte, params map[string]string, tags map[string]string) error {
	req := &cloudformation.CreateStackInput{
		Capabilities:     []*string{aws.String("CAPABILITY_IAM")},
		StackName:        aws.String(name),
		TemplateBody:     aws.String(string(body)),
		NotificationARNs: []*string{aws.String(p.CloudformationTopic)},
	}

	for key, value := range params {
		req.Parameters = append(req.Parameters, &cloudformation.Parameter{
			ParameterKey:   aws.String(key),
			ParameterValue: aws.String(value),
		})
	}

	for key, value := range tags {
		req.Tags = append(req.Tags, &cloudformation.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}

	_, err := p.cloudformation().CreateStack(req)
	if err != nil {
		return err
	}

	return nil
}

func (p *Provider) dynamoBatchDeleteItems(wrs []*dynamodb.WriteRequest, tableName string) error {

	if len(wrs) > 0 {

		if len(wrs) <= 25 {
			_, err := p.dynamodb().BatchWriteItem(&dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]*dynamodb.WriteRequest{
					tableName: wrs,
				},
			})
			if err != nil {
				return err
			}

		} else {

			// if more than 25 items to delete, we have to make multiple calls
			maxLen := 25
			for i := 0; i < len(wrs); i += maxLen {
				high := i + maxLen
				if high > len(wrs) {
					high = len(wrs)
				}

				_, err := p.dynamodb().BatchWriteItem(&dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]*dynamodb.WriteRequest{
						tableName: wrs[i:high],
					},
				})
				if err != nil {
					return err
				}

			}
		}
	} else {
		fmt.Println("ns=api fn=dynamoBatchDeleteItems level=info msg=\"no builds to delete\"")
	}

	return nil
}

// listAndDescribeContainerInstances lists and describes all the ECS instances.
// It handles pagination for clusters > 100 instances.
func (p *Provider) listAndDescribeContainerInstances() (*ecs.DescribeContainerInstancesOutput, error) {
	instances := []*ecs.ContainerInstance{}
	var nextToken string

	for {
		res, err := p.listContainerInstances(&ecs.ListContainerInstancesInput{
			Cluster:   aws.String(p.Cluster),
			NextToken: &nextToken,
		})
		if ae, ok := err.(awserr.Error); ok && ae.Code() == "ClusterNotFoundException" {
			return nil, fmt.Errorf("cluster not found: %s", p.Cluster)
		}
		if err != nil {
			return nil, err
		}

		ci, err := p.describeContainerInstances(&ecs.DescribeContainerInstancesInput{
			Cluster:            aws.String(p.Cluster),
			ContainerInstances: res.ContainerInstanceArns,
		})
		if err != nil {
			return nil, err
		}

		instances = append(instances, ci.ContainerInstances...)

		// No more container results
		if res.NextToken == nil {
			break
		}

		// set the nextToken to be used for the next iteration
		nextToken = *res.NextToken
	}

	return &ecs.DescribeContainerInstancesOutput{
		ContainerInstances: instances,
	}, nil
}

func (p *Provider) describeContainerInstances(input *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
	res, ok := cache.Get("describeContainerInstances", input).(*ecs.DescribeContainerInstancesOutput)
	if ok {
		return res, nil
	}

	res, err := p.ecs().DescribeContainerInstances(input)

	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeContainerInstances", input, res, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) describeServices(input *ecs.DescribeServicesInput) (*ecs.DescribeServicesOutput, error) {
	res, ok := cache.Get("describeServices", input.Services).(*ecs.DescribeServicesOutput)

	if ok {
		return res, nil
	}

	res, err := p.ecs().DescribeServices(input)

	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeServices", input.Services, res, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) describeStacks(input *cloudformation.DescribeStacksInput) ([]*cloudformation.Stack, error) {
	var stacks []*cloudformation.Stack
	stacks, ok := cache.Get("describeStacks", input.StackName).([]*cloudformation.Stack)

	if ok {
		return stacks, nil
	}

	err := p.cloudformation().DescribeStacksPages(input,
		func(page *cloudformation.DescribeStacksOutput, lastPage bool) bool {
			for _, stack := range page.Stacks {
				stacks = append(stacks, stack)
			}
			return true
		},
	)

	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeStacks", input.StackName, stacks, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return stacks, nil
}

func (p *Provider) describeStack(name string) (*cloudformation.Stack, error) {
	stacks, err := p.describeStacks(&cloudformation.DescribeStacksInput{
		StackName: aws.String(name),
	})
	if ae, ok := err.(awserr.Error); ok && ae.Code() == "ValidationError" {
		return nil, fmt.Errorf("stack not found: %s", name)
	}
	if err != nil {
		return nil, err
	}
	if len(stacks) != 1 {
		return nil, fmt.Errorf("could not load stack: %s", name)
	}

	return stacks[0], nil
}

func (p *Provider) describeStackEvents(input *cloudformation.DescribeStackEventsInput) (*cloudformation.DescribeStackEventsOutput, error) {
	res, ok := cache.Get("describeStackEvents", input.StackName).(*cloudformation.DescribeStackEventsOutput)

	if ok {
		return res, nil
	}

	res, err := p.cloudformation().DescribeStackEvents(input)
	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeStackEvents", input.StackName, res, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) describeStackResource(input *cloudformation.DescribeStackResourceInput) (*cloudformation.DescribeStackResourceOutput, error) {
	key := fmt.Sprintf("%s.%s", *input.StackName, *input.LogicalResourceId)

	res, ok := cache.Get("describeStackResource", key).(*cloudformation.DescribeStackResourceOutput)

	if ok {
		return res, nil
	}

	res, err := p.cloudformation().DescribeStackResource(input)
	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeStackResource", key, res, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) describeStackResources(input *cloudformation.DescribeStackResourcesInput) (*cloudformation.DescribeStackResourcesOutput, error) {
	res, ok := cache.Get("describeStackResources", input.StackName).(*cloudformation.DescribeStackResourcesOutput)

	if ok {
		return res, nil
	}

	res, err := p.cloudformation().DescribeStackResources(input)
	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeStackResources", input.StackName, res, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) stackTemplate(stack string) ([]byte, error) {
	res, err := p.cloudformation().GetTemplate(&cloudformation.GetTemplateInput{
		StackName: aws.String(stack),
	})
	if err != nil {
		return nil, err
	}
	if res.TemplateBody == nil {
		return nil, fmt.Errorf("no template body")
	}

	return []byte(*res.TemplateBody), nil
}

func (p *Provider) listStackResources(stack string) ([]*cloudformation.StackResourceSummary, error) {
	res, ok := cache.Get("listStackResources", stack).([]*cloudformation.StackResourceSummary)
	if ok {
		return res, nil
	}

	req := &cloudformation.ListStackResourcesInput{
		StackName: aws.String(stack),
	}

	srs := []*cloudformation.StackResourceSummary{}

	for {
		res, err := p.cloudformation().ListStackResources(req)
		if err != nil {
			return nil, err
		}

		srs = append(srs, res.StackResourceSummaries...)

		if res.NextToken == nil {
			break
		}

		req.NextToken = res.NextToken
	}

	if !p.SkipCache {
		if err := cache.Set("listStackResources", stack, srs, 5*time.Second); err != nil {
			return nil, err
		}
	}

	return srs, nil
}

func (p *Provider) appOutput(app, output string) (string, error) {
	s, err := p.describeStack(p.rackStack(app))
	if err != nil {
		return "", err
	}

	for k, v := range stackOutputs(s) {
		if k == output {
			return v, nil
		}
	}

	return "", nil
}

func (p *Provider) rackResource(resource string) (string, error) {
	res, err := p.stackResource(p.Rack, resource)
	if err != nil {
		return "", err
	}

	return *res.PhysicalResourceId, nil
}

func (p *Provider) appResource(app, resource string) (string, error) {
	res, err := p.stackResource(fmt.Sprintf("%s-%s", p.Rack, app), resource)
	if err != nil {
		return "", err
	}

	return *res.PhysicalResourceId, nil
}

func (p *Provider) stackResource(stack, resource string) (*cloudformation.StackResourceSummary, error) {
	log := Logger.At("stackResource").Namespace("stack=%s resource=%s", stack, resource).Start()

	srs, err := p.listStackResources(stack)
	if err != nil {
		return nil, log.Error(err)
	}

	for _, sr := range srs {
		if *sr.LogicalResourceId == resource && sr.PhysicalResourceId != nil {
			return sr, log.Successf("physical=%s", *sr.PhysicalResourceId)
		}
	}

	return nil, fmt.Errorf("resource not found: %s", resource)
}

func (p *Provider) appResources(app string) (map[string]string, error) {
	srs, err := p.listStackResources(p.rackStack(app))
	if err != nil {
		return nil, err
	}

	rs := map[string]string{}

	for _, sr := range srs {
		if sr.LogicalResourceId != nil && sr.PhysicalResourceId != nil {
			rs[*sr.LogicalResourceId] = *sr.PhysicalResourceId
		}
	}

	return rs, nil
}

func (p *Provider) stackParameter(stack, param string) (string, error) {
	res, err := p.describeStack(stack)
	if err != nil {
		return "", err
	}

	for _, p := range res.Parameters {
		if *p.ParameterKey == param {
			return *p.ParameterValue, nil
		}
	}

	return "", fmt.Errorf("parameter not found: %s", param)
}

func (p *Provider) dockerContainerFromPid(pid string) (*docker.Container, error) {
	dc, err := p.dockerClientFromPid(pid)
	if err != nil {
		return nil, err
	}

	arn, err := p.taskArnFromPid(pid)
	if err != nil {
		return nil, err
	}

	tries := 0

	var cs []docker.APIContainers

	for {
		tries += 1
		time.Sleep(1 * time.Second)

		cs, err = dc.ListContainers(docker.ListContainersOptions{
			All: true,
			Filters: map[string][]string{
				"label": {
					fmt.Sprintf("com.amazonaws.ecs.task-arn=%s", arn),
					"convox.release",
				},
			},
		})
		if err != nil {
			return nil, err
		}
		if len(cs) != 1 {
			if tries < 20 {
				continue
			}
			return nil, fmt.Errorf("could not find container for task: %s", arn)
		}

		c, err := dc.InspectContainer(cs[0].ID)
		if err != nil {
			return nil, err
		}

		return c, nil
	}

	return nil, fmt.Errorf("could not find container for task: %s", arn)
}

func (p *Provider) dockerClientFromPid(pid string) (*docker.Client, error) {
	arn, err := p.taskArnFromPid(pid)
	if err != nil {
		return nil, err
	}

	task, err := p.describeTask(arn)
	if err != nil {
		return nil, err
	}
	if len(task.Containers) < 1 {
		return nil, fmt.Errorf("no running container for process: %s", pid)
	}

	if task.ContainerInstanceArn == nil {
		return nil, fmt.Errorf("could not find instance for process: %s", pid)
	}

	cires, err := p.describeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String(p.Cluster),
		ContainerInstances: []*string{task.ContainerInstanceArn},
	})
	if err != nil {
		return nil, err
	}
	if len(cires.ContainerInstances) < 1 {
		return nil, fmt.Errorf("could not find instance for process: %s", pid)
	}

	dc, err := p.dockerInstance(*cires.ContainerInstances[0].Ec2InstanceId)
	if err != nil {
		return nil, err
	}

	return dc, nil
}

func (p *Provider) describeTaskDefinition(input *ecs.DescribeTaskDefinitionInput) (*ecs.DescribeTaskDefinitionOutput, error) {
	td, ok := cache.Get("describeTaskDefinition", input).(*ecs.DescribeTaskDefinitionOutput)
	if ok {
		return td, nil
	}

	res, err := p.ecs().DescribeTaskDefinition(input)
	if ae, ok := err.(awserr.Error); ok && ae.Code() == "ValidationError" {
		return nil, fmt.Errorf("task definition not found: %s", *input.TaskDefinition)
	}
	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("describeTaskDefinition", input, res, 24*time.Hour); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) describeTasks(input *ecs.DescribeTasksInput) (*ecs.DescribeTasksOutput, error) {
	res, ok := cache.Get("describeTasks", input).(*ecs.DescribeTasksOutput)

	if ok {
		return res, nil
	}

	res, err := p.ecs().DescribeTasks(input)

	if err != nil {
		return nil, err
	}

	if !p.SkipCache && len(res.Failures) == 0 {
		if err := cache.Set("describeTasks", input, res, 10*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) listContainerInstances(input *ecs.ListContainerInstancesInput) (*ecs.ListContainerInstancesOutput, error) {
	res, ok := cache.Get("listContainerInstances", input).(*ecs.ListContainerInstancesOutput)

	if ok {
		return res, nil
	}

	res, err := p.ecs().ListContainerInstances(input)

	if err != nil {
		return nil, err
	}

	if !p.SkipCache {
		if err := cache.Set("listContainerInstances", input, res, 10*time.Second); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (p *Provider) objectURL(ou string) (string, error) {
	u, err := url.Parse(ou)
	if err != nil {
		return "", err
	}

	if u.Scheme != "object" {
		return "", fmt.Errorf("only supports object:// urls")
	}

	return fmt.Sprintf("https://s3.%s.amazonaws.com/%s%s", p.Region, p.SettingsBucket, u.Path), nil
}

func (p *Provider) putLogEvents(req *cloudwatchlogs.PutLogEventsInput) (string, error) {
	attempts := 0

	for {
		res, err := p.cloudwatchlogs().PutLogEvents(req)
		if err == nil {
			return *res.NextSequenceToken, nil
		}
		if err != nil {
			attempts++
			if attempts > 3 {
				return "", err
			}
		}
		if awsError(err) == "ResourceNotFoundException" {
			_, err := p.cloudwatchlogs().CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
				LogGroupName:  req.LogGroupName,
				LogStreamName: req.LogStreamName,
			})
			if err != nil {
				return "", err
			}

			continue
		}
		if awsError(err) == "InvalidSequenceTokenException" {
			sres, err := p.cloudwatchlogs().DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
				LogGroupName:        req.LogGroupName,
				LogStreamNamePrefix: req.LogStreamName,
			})
			if err != nil {
				return "", err
			}
			if len(sres.LogStreams) != 1 {
				return "", fmt.Errorf("could not describe log stream: %s/%s\n", *req.LogGroupName, *req.LogStreamName)
			}

			req.SequenceToken = sres.LogStreams[0].UploadSequenceToken

			continue
		}

		return "", err
	}
}

func (p *Provider) serviceArn(app, service string) (string, error) {
	sarn, err := p.appOutput(app, fmt.Sprintf("Service%sService", upperName(service)))
	if err != nil {
		return "", err
	}
	if sarn != "" {
		return sarn, nil
	}

	sarn, err = p.appResource(app, fmt.Sprintf("Service%sService", upperName(service)))
	if err != nil && !strings.HasPrefix(err.Error(), "resource not found") {
		return "", err
	}
	if sarn != "" {
		return sarn, nil
	}

	return "", nil
}

func (p *Provider) s3Exists(bucket, key string) (bool, error) {
	_, err := p.s3().HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		if aerr, ok := err.(awserr.RequestFailure); ok && aerr.StatusCode() == 404 {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (p *Provider) s3Get(bucket, key string) ([]byte, error) {
	req := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	res, err := p.s3().GetObject(req)

	if err != nil {
		return nil, err
	}

	return ioutil.ReadAll(res.Body)
}

func (p *Provider) s3Delete(bucket, key string) error {
	req := &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	_, err := p.s3().DeleteObject(req)

	return err
}

func (p *Provider) s3Put(bucket, key string, data []byte, public bool) error {
	req := &s3.PutObjectInput{
		Body:          bytes.NewReader(data),
		Bucket:        aws.String(bucket),
		ContentLength: aws.Int64(int64(len(data))),
		Key:           aws.String(key),
	}

	if public {
		req.ACL = aws.String("public-read")
	}

	_, err := p.s3().PutObject(req)

	return err
}

func (p *Provider) taskRelease(id string) (string, error) {
	if release, ok := cache.Get("taskRelease", id).(string); ok {
		return release, nil
	}

	t, err := p.describeTasks(&ecs.DescribeTasksInput{
		Cluster: aws.String(p.Cluster),
		Tasks:   []*string{aws.String(id)},
	})
	if err != nil {
		return "", err
	}
	if len(t.Tasks) < 1 {
		return "", fmt.Errorf("task not found: %s", id)
	}

	release, err := p.taskDefinitionRelease(*t.Tasks[0].TaskDefinitionArn)
	if err != nil {
		return "", err
	}

	if err := cache.Set("taskRelease", id, release, 24*time.Hour); err != nil {
		return "", err
	}

	return release, nil
}

func (p *Provider) taskDefinitionRelease(arn string) (string, error) {
	if release, ok := cache.Get("taskDefinitionRelease", arn).(string); ok {
		return release, nil
	}

	td, err := p.describeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(arn),
	})
	if err != nil {
		return "", err
	}
	if len(td.TaskDefinition.ContainerDefinitions) < 0 {
		return "", fmt.Errorf("no container definitions for task definition: %s", arn)
	}

	release, ok := td.TaskDefinition.ContainerDefinitions[0].DockerLabels["convox.release"]
	if !ok || release == nil {
		return "", fmt.Errorf("no convox.release label for task definition: %s", arn)
	}

	if err := cache.Set("taskDefinitionRelease", arn, *release, 24*time.Hour); err != nil {
		return "", err
	}

	return *release, nil
}

// updateStack updates a stack
//   template is url to a template or empty string to reuse previous
//   changes is a list of parameter changes to make (does not need to include every param)
func (p *Provider) updateStack(name string, template []byte, changes map[string]string, tags map[string]string, id string) error {
	cache.Clear("describeStacks", nil)
	cache.Clear("describeStacks", name)

	req := &cloudformation.UpdateStackInput{
		Capabilities:     []*string{aws.String("CAPABILITY_IAM")},
		StackName:        aws.String(name),
		NotificationARNs: []*string{aws.String(p.CloudformationTopic)},
	}

	if id != "" {
		req.ClientRequestToken = aws.String(id)
	}

	params := map[string]bool{}
	pexisting := map[string]bool{}

	stack, err := p.describeStack(name)
	if err != nil {
		return err
	}

	for _, p := range stack.Parameters {
		pexisting[*p.ParameterKey] = true
	}

	if template != nil {
		key := ""

		if p.IsTest() {
			key = "test-key"
		}

		ou, err := p.ObjectStore("", key, bytes.NewReader(template), structs.ObjectStoreOptions{})
		if err != nil {
			return err
		}

		tu, err := p.objectURL(ou.Url)
		if err != nil {
			return err
		}

		req.TemplateURL = aws.String(tu)

		fp, err := formationParameters(template)
		if err != nil {
			return err
		}

		for p := range fp {
			params[p] = true
		}
	} else {
		req.UsePreviousTemplate = aws.Bool(true)

		for param := range pexisting {
			params[param] = true
		}
	}

	sorted := []string{}

	for param := range params {
		sorted = append(sorted, param)
	}

	// sort params for easier testing
	sort.Strings(sorted)

	for _, param := range sorted {
		if value, ok := changes[param]; ok {
			req.Parameters = append(req.Parameters, &cloudformation.Parameter{
				ParameterKey:   aws.String(param),
				ParameterValue: aws.String(value),
			})
		} else if pexisting[param] {
			req.Parameters = append(req.Parameters, &cloudformation.Parameter{
				ParameterKey:     aws.String(param),
				UsePreviousValue: aws.Bool(true),
			})
		}
	}

	req.Tags = stack.Tags

	tks := []string{}

	for key := range tags {
		tks = append(tks, key)
	}

	sort.Strings(tks)

	for _, key := range tks {
		req.Tags = append(req.Tags, &cloudformation.Tag{
			Key:   aws.String(key),
			Value: aws.String(tags[key]),
		})
	}

	_, err = p.cloudformation().UpdateStack(req)

	cache.Clear("describeStacks", nil)
	cache.Clear("describeStacks", name)

	return err
}

var (
	serverCertificateWaitConfirmations = 3
	serverCertificateWaitTick          = 5 * time.Second
	serverCertificateWaitTimeout       = 2 * time.Minute
)

// wait for a few successful certificate refreshes in a row
func (p *Provider) waitForServerCertificate(name string) error {
	confirmations := 0
	done := time.Now().Add(serverCertificateWaitTimeout)

	for {
		if time.Now().After(done) {
			return fmt.Errorf("timeout")
		}

		if confirmations >= serverCertificateWaitConfirmations {
			return nil
		}

		time.Sleep(serverCertificateWaitTick)

		res, err := p.iam().GetServerCertificate(&iam.GetServerCertificateInput{
			ServerCertificateName: &name,
		})
		if err != nil {
			confirmations = 0
			continue
		}
		if res.ServerCertificate == nil || res.ServerCertificate.ServerCertificateMetadata == nil || res.ServerCertificate.ServerCertificateMetadata.ServerCertificateName == nil {
			confirmations = 0
			continue
		}
		if *res.ServerCertificate.ServerCertificateMetadata.ServerCertificateName != name {
			confirmations = 0
			continue
		}

		confirmations++
	}

	return fmt.Errorf("can't get here")
}

func generateSelfSignedCertificate(host string) ([]byte, []byte, error) {
	rkey, err := rsa.GenerateKey(crand.Reader, 2048)

	if err != nil {
		return nil, nil, err
	}

	serial, err := crand.Int(crand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   host,
			Organization: []string{"convox"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}

	data, err := x509.CreateCertificate(crand.Reader, &template, &template, &rkey.PublicKey, rkey)

	if err != nil {
		return nil, nil, err
	}

	pub := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: data})
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rkey)})

	return pub, key, nil
}

type CronJob struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Command  string `yaml:"command"`
	Service  *manifest1.Service
	App      *structs.App
}

type CronJobs []CronJob

func appCronJobs(a *structs.App, m *manifest1.Manifest) CronJobs {
	cronjobs := []CronJob{}

	if m == nil {
		return cronjobs
	}

	for _, entry := range m.Services {
		labels := entry.LabelsByPrefix("convox.cron")
		for key, value := range labels {
			cronjob := NewCronJobFromLabel(key, value)
			e := entry
			cronjob.Service = &e
			cronjob.App = a
			cronjobs = append(cronjobs, cronjob)
		}
	}

	return cronjobs
}

func (a CronJobs) Len() int           { return len(a) }
func (a CronJobs) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a CronJobs) Less(i, j int) bool { return a[i].Name < a[j].Name }

func NewCronJobFromLabel(key, value string) CronJob {
	keySlice := strings.Split(key, ".")
	name := keySlice[len(keySlice)-1]
	tokens := strings.Fields(value)
	cronjob := CronJob{
		Name:     name,
		Schedule: fmt.Sprintf("cron(%s *)", strings.Join(tokens[0:5], " ")),
		Command:  strings.Join(tokens[5:], " "),
	}
	return cronjob
}

func (cr *CronJob) AppName() string {
	return cr.App.Name
}

func (cr *CronJob) Process() string {
	return cr.Service.Name
}

func (cr *CronJob) ShortName() string {
	shortName := fmt.Sprintf("%s%s", strings.Title(cr.Service.Name), strings.Title(cr.Name))

	reg, err := regexp.Compile("[^A-Za-z0-9]+")
	if err != nil {
		panic(err)
	}

	return reg.ReplaceAllString(shortName, "")
}

func (cr *CronJob) LongName() string {
	prefix := fmt.Sprintf("%s-%s-%s-%s", os.Getenv("RACK"), cr.App.Name, cr.Process(), cr.Name)
	hash := sha256.Sum256([]byte(prefix))
	suffix := "-" + base32.StdEncoding.EncodeToString(hash[:])[:7]

	// $prefix-$suffix-schedule" needs to be <= 64 characters
	if len(prefix) > 55-len(suffix) {
		prefix = prefix[:55-len(suffix)]
	}
	return prefix + suffix
}
