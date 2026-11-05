//go:build tools

// Package deps pins every third-party module listed in ARCHITECTURE.md §1,
// so "go mod tidy" keeps them in go.mod before the packages that use them
// exist. It is never compiled into the binary.
package deps

import (
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/service/iam"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/aws/aws-sdk-go-v2/service/ses"
	_ "github.com/aws/aws-sdk-go-v2/service/sesv2"
	_ "github.com/aws/aws-sdk-go-v2/service/sns"
	_ "github.com/aws/aws-sdk-go-v2/service/sts"
	_ "github.com/danielgtaylor/huma/v2"
	_ "github.com/danielgtaylor/huma/v2/adapters/humago"
	_ "github.com/yuin/goldmark"
	_ "golang.org/x/crypto/acme/autocert"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/net/html"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
