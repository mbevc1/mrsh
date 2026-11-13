package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// DefaultSSMPrefix is where mrsh stores secrets unless defaults.ssm_prefix
// says otherwise.
const DefaultSSMPrefix = "/mrsh"

// Owners of a stored secret, the middle segment of its default name.
const (
	OwnerCredential = "creds"
	OwnerHost       = "hosts"
	OwnerDefaults   = "defaults"
)

// ErrSecretExists means a parameter already exists and overwriting was not
// asked for.
var ErrSecretExists = errors.New("parameter already exists (use --overwrite to replace it)")

// ssmNameRE is SSM's parameter name alphabet, for a fully qualified name.
var ssmNameRE = regexp.MustCompile(`^(/[a-zA-Z0-9_.-]+)+$`)

// ValidSSMName checks a fully qualified parameter name such as
// /mrsh/creds/routers/pass. SSM reserves names under /aws and /ssm.
func ValidSSMName(name string) error {
	switch {
	case !ssmNameRE.MatchString(name):
		return fmt.Errorf("%q must look like /path/to/name (letters, digits, '_', '.', '-' and '/')", name)
	case len(name) > 1011:
		return fmt.Errorf("%q is longer than SSM's 1011 characters", name)
	}
	first := strings.ToLower(strings.SplitN(name[1:], "/", 2)[0])
	if strings.HasPrefix(first, "aws") || strings.HasPrefix(first, "ssm") {
		return fmt.Errorf("%q: names starting with aws or ssm are reserved by AWS", name)
	}
	return nil
}

// SSMPrefix is the effective prefix for stored secrets.
func (c *Config) SSMPrefix() string {
	if c.Defaults.SSMPrefix != "" {
		return c.Defaults.SSMPrefix
	}
	return DefaultSSMPrefix
}

// SSMName is the default parameter for one field of one owner:
// <prefix>/creds/NAME/pass, <prefix>/hosts/NAME/user or <prefix>/defaults/pass.
func (c *Config) SSMName(owner, name, field string) string {
	if owner == OwnerDefaults {
		return c.SSMPrefix() + "/defaults/" + field
	}
	return c.SSMPrefix() + "/" + owner + "/" + name + "/" + field
}

// SSMParamName returns the hierarchical parameter name an SSM ARN points
// at, e.g. /mrsh/creds/routers/pass.
func SSMParamName(arnValue string) (string, bool) {
	a, err := parseARN(arnValue)
	if err != nil || a.Service != "ssm" {
		return "", false
	}
	return "/" + strings.TrimPrefix(strings.SplitN(a.Base, ":parameter/", 2)[1], "/"), true
}

// ManagedSSMName returns the parameter name of an SSM ARN when it lies under
// the config's prefix, the only parameters mrsh will delete.
func (c *Config) ManagedSSMName(arnValue string) (string, bool) {
	name, ok := SSMParamName(arnValue)
	return name, ok && strings.HasPrefix(name, c.SSMPrefix()+"/")
}

// OwnSSMARNs are the SSM parameters c's user and pass point at.
func OwnSSMARNs(c Credentials) []string {
	var arns []string
	for _, a := range []string{c.UserARN, c.PassARN} {
		if _, ok := SSMParamName(a); ok {
			arns = append(arns, a)
		}
	}
	return arns
}

// SecretUsers lists the entries other than skip ("defaults", "credential
// NAME" or "host NAME") whose own user or pass references arn.
func (c *Config) SecretUsers(arn, skip string) []string {
	var users []string
	uses := func(cr Credentials) bool { return cr.UserARN == arn || cr.PassARN == arn }
	if skip != "defaults" && uses(c.Defaults.Credentials) {
		users = append(users, "defaults")
	}
	for _, name := range sortedKeys(c.Credentials) {
		if who := "credential " + name; who != skip && uses(c.Credentials[name].Credentials) {
			users = append(users, who)
		}
	}
	for _, h := range c.Hosts {
		if who := "host " + h.Name; who != skip && uses(h.Credentials) {
			users = append(users, who)
		}
	}
	return users
}

// SecretDeletes splits the SSM parameters of an entry being removed (who,
// e.g. "host rb02") into those that may be deleted (under the prefix and
// used by nothing else) and notes on those kept.
func (c *Config) SecretDeletes(cr Credentials, who string) (del, kept []string) {
	for _, arn := range OwnSSMARNs(cr) {
		name, managed := c.ManagedSSMName(arn)
		switch users := c.SecretUsers(arn, who); {
		case !managed:
			kept = append(kept, fmt.Sprintf("kept %s: not under %s", arn, c.SSMPrefix()))
		case len(users) > 0:
			kept = append(kept, fmt.Sprintf("kept %s: still used by %s", name, strings.Join(users, ", ")))
		default:
			del = append(del, arn)
		}
	}
	return del, kept
}

// HostSecretDeletes plans the SSM parameters to delete when the named
// hosts are removed together: SecretDeletes for each, judged against the
// config without any of them, so a parameter only they share is deleted.
func (c *Config) HostSecretDeletes(names []string) (del, kept []string) {
	gone := map[string]bool{}
	for _, n := range names {
		gone[n] = true
	}
	rest := *c
	rest.Hosts = nil
	var removed []Host
	for _, h := range c.Hosts {
		if gone[h.Name] {
			removed = append(removed, h)
		} else {
			rest.Hosts = append(rest.Hosts, h)
		}
	}
	seen := map[string]bool{}
	add := func(list *[]string, xs []string) {
		for _, x := range xs {
			if !seen[x] {
				seen[x] = true
				*list = append(*list, x)
			}
		}
	}
	for _, h := range removed {
		d, k := rest.SecretDeletes(h.Credentials, "host "+h.Name)
		add(&del, d)
		add(&kept, k)
	}
	return del, kept
}

// PassSource names where Effective takes h's password from: "host NAME",
// "credential NAME" (the host's set or the defaults' set), "defaults", or
// "" when nothing sets one.
func (c *Config) PassSource(h Host) string {
	switch d := c.Defaults; {
	case h.PassRef().Kind() != KindUnset:
		return "host " + h.Name
	case h.Credential != "" && c.Credentials[h.Credential].PassRef().Kind() != KindUnset:
		return "credential " + h.Credential
	case d.PassRef().Kind() != KindUnset:
		return "defaults"
	case d.Credential != "" && c.Credentials[d.Credential].PassRef().Kind() != KindUnset:
		return "credential " + d.Credential
	}
	return ""
}

// SecretWriter stores and deletes secrets in SSM Parameter Store.
type SecretWriter interface {
	// PutSecret stores value as a SecureString under name, encrypted with
	// the AWS-managed key, and returns the parameter's ARN. An existing
	// parameter is replaced only with overwrite; otherwise the error wraps
	// ErrSecretExists.
	PutSecret(ctx context.Context, name, value string, overwrite bool) (string, error)
	// DeleteSecret removes the parameter an ARN names.
	DeleteSecret(ctx context.Context, arn string) error
}

// NewSecretWriter returns a SecretWriter on the default AWS credential
// chain and region.
func NewSecretWriter() SecretWriter {
	return NewAWSBackend().(*awsBackend)
}

// managedTag marks parameters mrsh created.
var managedTag = ssmtypes.Tag{Key: aws.String("managed-by"), Value: aws.String("mrsh")}

func (b *awsBackend) PutSecret(ctx context.Context, name, value string, overwrite bool) (string, error) {
	if err := ValidSSMName(name); err != nil {
		return "", err
	}
	region, partition, account, err := b.identity(ctx)
	if err != nil {
		return "", err
	}
	c, err := b.ssmClient(ctx, region)
	if err != nil {
		return "", err
	}
	in := &ssm.PutParameterInput{Name: aws.String(name), Value: aws.String(value), Type: ssmtypes.ParameterTypeSecureString, Overwrite: aws.Bool(overwrite)}
	if !overwrite {
		in.Tags = []ssmtypes.Tag{managedTag} // SSM refuses tags together with Overwrite
	}
	if _, err := c.PutParameter(ctx, in); err != nil {
		var exists *ssmtypes.ParameterAlreadyExists
		if errors.As(err, &exists) {
			return "", fmt.Errorf("%s: %w", name, ErrSecretExists)
		}
		return "", fmt.Errorf("store %s in SSM: %w", name, err)
	}
	if overwrite {
		if _, err := c.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
			ResourceType: ssmtypes.ResourceTypeForTaggingParameter, ResourceId: aws.String(name), Tags: []ssmtypes.Tag{managedTag},
		}); err != nil {
			return "", fmt.Errorf("tag %s in SSM: %w", name, err)
		}
	}
	return fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s", partition, region, account, name), nil
}

func (b *awsBackend) DeleteSecret(ctx context.Context, arnValue string) error {
	a, err := parseARN(arnValue)
	if err != nil || a.Service != "ssm" {
		return fmt.Errorf("%q is not an SSM parameter ARN", arnValue)
	}
	c, err := b.ssmClient(ctx, a.Region)
	if err != nil {
		return err
	}
	name := "/" + strings.TrimPrefix(strings.SplitN(a.Base, ":parameter/", 2)[1], "/")
	if _, err := c.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)}); err != nil {
		var missing *ssmtypes.ParameterNotFound
		if errors.As(err, &missing) {
			return nil // already gone: nothing to do
		}
		return fmt.Errorf("delete %s from SSM: %w", name, err)
	}
	return nil
}

// identity returns the region, partition and account a new parameter's
// ARN needs: the region from the AWS config, the rest from STS.
func (b *awsBackend) identity(ctx context.Context) (region, partition, account string, err error) {
	b.mu.Lock()
	cfg, err := b.config(ctx)
	b.mu.Unlock()
	if err != nil {
		return "", "", "", err
	}
	if cfg.Region == "" {
		return "", "", "", errors.New("no AWS region for storing secrets: set AWS_REGION or a region in your AWS profile")
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", "", fmt.Errorf("find the AWS account (sts:GetCallerIdentity): %w", err)
	}
	callerARN := strings.Split(aws.ToString(out.Arn), ":")
	if len(callerARN) < 2 || callerARN[1] == "" {
		return "", "", "", fmt.Errorf("unexpected caller ARN %q", aws.ToString(out.Arn))
	}
	return cfg.Region, callerARN[1], aws.ToString(out.Account), nil
}
