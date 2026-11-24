package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// AWS reads from AWS Secrets Manager. Each path is the name of one secret whose
// SecretString is a JSON object of KEY: value pairs (the "key/value" secret type).
type AWS struct {
	client *secretsmanager.Client
}

// AWSFromEnv builds a client from AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION
// (optionally AWS_ENDPOINT_URL). Like FromEnv, the environment wins over identityFile.
// shortcut: static keys only, no instance roles or SSO; upgrade by loading the SDK default chain.
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
	keyID, secret, region := lookup("AWS_ACCESS_KEY_ID"), lookup("AWS_SECRET_ACCESS_KEY"), lookup("AWS_REGION")
	var missing []string
	for _, need := range []struct{ name, value string }{
		{"AWS_ACCESS_KEY_ID", keyID}, {"AWS_SECRET_ACCESS_KEY", secret}, {"AWS_REGION", region},
	} {
		if need.value == "" {
			missing = append(missing, need.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("aws identity incomplete, missing: %s", strings.Join(missing, ", "))
	}
	opts := secretsmanager.Options{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(keyID, secret, lookup("AWS_SESSION_TOKEN")),
	}
	if endpoint := lookup("AWS_ENDPOINT_URL"); endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return &AWS{client: secretsmanager.New(opts)}, nil
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
