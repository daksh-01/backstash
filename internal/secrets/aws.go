package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// AWS reads from AWS Secrets Manager. Each path is the name of one secret whose
// SecretString is a JSON object of KEY: value pairs (the "key/value" secret type).
type AWS struct {
	client   *secretsmanager.Client
	cacheKey string
}

// CacheKey seals the local secrets cache; it is the credential this client signs with.
func (src *AWS) CacheKey() string { return src.cacheKey }

// AWSFromEnv builds a client. With AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (optionally
// AWS_SESSION_TOKEN) in the environment or identityFile it uses those static keys; with neither it
// uses the AWS default chain (instance or task role, SSO, ~/.aws), so no key has to be on disk.
// AWS_REGION is required either way; AWS_ENDPOINT_URL is optional. The environment wins over the file.
func AWSFromEnv(identityFile string) (*AWS, error) {
	var fileVars map[string]string
	if identityFile != "" {
		vars, err := ReadEnvFile(identityFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		fileVars = vars
	}
	lookup := func(name string) string {
		if val := os.Getenv(name); val != "" {
			return val
		}
		return fileVars[name]
	}
	keyID, secret := lookup("AWS_ACCESS_KEY_ID"), lookup("AWS_SECRET_ACCESS_KEY")
	if (keyID == "") != (secret == "") {
		return nil, errors.New("aws identity incomplete: set both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or neither to use the AWS default credential chain")
	}
	opts := secretsmanager.Options{Region: lookup("AWS_REGION")}
	cacheKey := secret
	if keyID != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(keyID, secret, lookup("AWS_SESSION_TOKEN"))
	} else {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(opts.Region))
		if err != nil {
			return nil, fmt.Errorf("aws credential chain: %w", err)
		}
		opts.Credentials, opts.Region = cfg.Credentials, cfg.Region
		// Role credentials rotate, so there is no stable secret to seal a cache with.
		cacheKey = ""
	}
	if opts.Region == "" {
		return nil, errors.New("aws identity incomplete, missing: AWS_REGION")
	}
	if endpoint := lookup("AWS_ENDPOINT_URL"); endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return &AWS{client: secretsmanager.New(opts), cacheKey: cacheKey}, nil
}

// Read implements Source.
func (src *AWS) Read(ctx context.Context, path string) (map[string]string, error) {
	out, err := src.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(path)})
	if err != nil {
		return nil, fmt.Errorf("aws secrets manager: %w", err)
	}
	if out.SecretString == nil {
		return nil, errors.New("aws secrets manager: secret has no SecretString (binary secrets are not supported)")
	}
	vals := map[string]string{}
	if err := json.Unmarshal([]byte(*out.SecretString), &vals); err != nil {
		return nil, errors.New("aws secrets manager: SecretString must be a JSON object of string values")
	}
	return vals, nil
}
