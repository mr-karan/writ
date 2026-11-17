// Package awsrole assumes AWS roles for writ serve's runs and hands the
// credentials to providers. Each run gets temporary credentials from STS,
// with a role session name and a source identity that name who asked, so
// CloudTrail shows them. Providers fetch the credentials from a loopback
// endpoint that speaks the AWS SDKs' container credentials protocol, with
// a token for each run, so the endpoint can assume the role again before
// the credentials expire and refuse the token once the run ends.
package awsrole

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// Session is one AssumeRole request.
type Session struct {
	Role string // the role's ARN
	// Name is the role session name, which CloudTrail shows with each
	// call the session makes.
	Name string
	// SourceIdentity names the principal behind the session. AWS keeps it
	// on every session chained from this one.
	SourceIdentity string
	Duration       time.Duration
}

// AssumedSession is a role session the control plane assumed. From is
// when its first credentials were issued, and Until when its last ones
// expire.
type AssumedSession struct {
	Role, Name, SourceIdentity string
	From, Until                time.Time
}

// Credentials are temporary credentials from STS.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
}

// Assumer assumes roles. *STS is one.
type Assumer interface {
	Assume(ctx context.Context, s Session) (Credentials, error)
}

// Bounds AWS sets on a session.
const (
	MinDuration = 15 * time.Minute
	MaxDuration = 12 * time.Hour
	maxName     = 64
)

// roleARN matches an IAM role's ARN in any partition.
var roleARN = regexp.MustCompile(`^arn:aws(-cn|-us-gov|-iso|-iso-b|-iso-e|-iso-f|-eusc)?:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`)

// CheckRoleARN reports whether s is an IAM role's ARN.
func CheckRoleARN(s string) error {
	if !roleARN.MatchString(s) {
		return fmt.Errorf("%q is not an IAM role's ARN, such as arn:aws:iam::111111111111:role/writ-plan", s)
	}
	return nil
}

// AccountOf returns the account id of a role's ARN.
func AccountOf(role string) string {
	f := strings.Split(role, ":")
	if len(f) < 5 {
		return ""
	}
	return f[4]
}

// badChar matches a character STS refuses in a role session name and a
// source identity.
var badChar = regexp.MustCompile(`[^\w+=,.@-]`)

func clean(s string) string { return badChar.ReplaceAllString(s, "-") }

// SessionName is writ-NAME-RUN, with characters STS refuses replaced by
// "-". STS allows 64 characters, so a long NAME is cut to fit and RUN is
// kept whole.
func SessionName(name, run string) string {
	name, run = clean(name), clean(run)
	room := maxName - len("writ--") - len(run)
	if room < 1 {
		return ("writ-" + run)[:maxName]
	}
	if len(name) > room {
		name = name[:room]
	}
	return "writ-" + name + "-" + run
}

// SourceIdentity is s with characters STS refuses replaced by "-", cut to
// 64 characters.
func SourceIdentity(s string) string {
	s = clean(s)
	if len(s) > maxName {
		s = s[:maxName]
	}
	return s
}

// STS assumes roles through the AWS SDK as the control plane's own
// identity: the SDK's default chain of environment, shared files, and the
// instance's role.
type STS struct {
	client *sts.Client
}

// STSOptions configures NewSTS.
type STSOptions struct {
	Region string
	// Endpoint replaces the regional STS endpoint, for an interface VPC
	// endpoint or a test. "" uses the SDK's.
	Endpoint string
}

// NewSTS loads the control plane's AWS identity. It contacts nothing:
// the SDK resolves credentials on the first call.
func NewSTS(ctx context.Context, o STSOptions) (*STS, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.Region))
	if err != nil {
		return nil, fmt.Errorf("load the control plane's AWS identity: %w", err)
	}
	client := sts.NewFromConfig(cfg, func(so *sts.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
	})
	return &STS{client: client}, nil
}

// DeniedError is STS refusing to assume a role.
type DeniedError struct {
	Role string
	Err  error
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("assume role %s: %v", e.Role, e.Err)
}

func (e *DeniedError) Unwrap() error { return e.Err }

// Assume calls sts:AssumeRole with s.
func (c *STS) Assume(ctx context.Context, s Session) (Credentials, error) {
	in := &sts.AssumeRoleInput{
		RoleArn:         aws.String(s.Role),
		RoleSessionName: aws.String(s.Name),
		DurationSeconds: aws.Int32(int32(s.Duration / time.Second)),
	}
	if s.SourceIdentity != "" {
		in.SourceIdentity = aws.String(s.SourceIdentity)
	}
	out, err := c.client.AssumeRole(ctx, in)
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) {
			err = fmt.Errorf("%s: %s", api.ErrorCode(), api.ErrorMessage())
		}
		return Credentials{}, &DeniedError{Role: s.Role, Err: err}
	}
	cr := out.Credentials
	if cr == nil || cr.AccessKeyId == nil || cr.SecretAccessKey == nil || cr.SessionToken == nil || cr.Expiration == nil {
		return Credentials{}, &DeniedError{Role: s.Role, Err: errors.New("STS answered without credentials")}
	}
	return Credentials{AccessKeyID: *cr.AccessKeyId, SecretAccessKey: *cr.SecretAccessKey, SessionToken: *cr.SessionToken, Expires: *cr.Expiration}, nil
}
