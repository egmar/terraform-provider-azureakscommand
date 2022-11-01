package helpers

import (
	"context"
	"errors"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"
	"os"
	"sync"
	"time"
)

// !! Warning - A regrettable hack !!
//
// MSFT has put their golang sdk consumers in a tough spot. Azure Workload
// Identity is replacing AAD Pod Identity as the solution to provide
// dynamically assigned credentials to workloads in a k8s cluster.
//
// Unfortunately, the azure-go-sdk's identity module simply doesn't
// support this type of authentication and the request to do so has
// been open since 09/2021.
//
//	https://github.com/Azure/azure-sdk-for-go/issues/15615
//
// The contents of this file provides a functional azcore.TokenCredential
// implementation that will work with the assertion provided by Azure
// Workload Identity.
//
// It is very difficult to test this and no care around caching was taken.
// This has worked well in the short time we've used it and the hope is that
// MSFT will add the functionality into their golang sdk in the next months.
//
// A sample of using this alongside the `DefaultAzureCredential` to mimic
// the behavior in other SDKs:
//

func NewAzureCredential() (azcore.TokenCredential, error) {
	if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") != "" {
		return NewWorkloadIdentityClientHack()
	}
	return azidentity.NewDefaultAzureCredential(nil)
}

type WorkloadIdentityClientHack struct {
	tenantId      string
	clientId      string
	authorityUrl  string
	tokenFilePath string
	mu            sync.Mutex
	activeToken   *azcore.AccessToken
	minLifetime   time.Duration
}

func (c *WorkloadIdentityClientHack) readAssertionToken() (string, error) {
	tokenBytes, err := os.ReadFile(c.tokenFilePath)
	if err != nil {
		return "", err
	}
	return string(tokenBytes), nil
}

func (c *WorkloadIdentityClientHack) GetToken(
	ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(c.minLifetime)

	if c.activeToken != nil && c.activeToken.ExpiresOn.After(cutoff) {
		return *c.activeToken, nil
	}
	c.activeToken = nil
	assertionToken, err := c.readAssertionToken()
	if err != nil {
		return azcore.AccessToken{}, err
	}

	cred, err := confidential.NewCredFromAssertion(assertionToken)

	client, err := confidential.New(
		c.clientId,
		cred,
		confidential.WithAuthority(c.authorityUrl),
	)

	result, err := client.AcquireTokenByCredential(ctx, opts.Scopes)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	c.activeToken = &azcore.AccessToken{
		Token: result.AccessToken, ExpiresOn: result.ExpiresOn.UTC()}
	return *c.activeToken, nil
}

func NewWorkloadIdentityClientHack() (*WorkloadIdentityClientHack, error) {
	tenantId := os.Getenv("AZURE_TENANT_ID")
	clientId := os.Getenv("AZURE_CLIENT_ID")
	tokenFilePath := os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
	authorityHost := os.Getenv("AZURE_AUTHORITY_HOST")

	if tenantId == "" {
		return nil, errors.New("AZURE_TENANT_ID must be set")
	}
	if clientId == "" {
		return nil, errors.New("AZURE_CLIENT_ID must be set")
	}
	if tokenFilePath == "" {
		return nil, errors.New("AZURE_FEDERATED_TOKEN_FILE must be set")
	}
	if authorityHost == "" {
		return nil, errors.New("AZURE_AUTHORITY_HOST must be set")
	}

	return &WorkloadIdentityClientHack{
		tenantId:      tenantId,
		clientId:      clientId,
		authorityUrl:  fmt.Sprintf("%s%s/oauth2/token", authorityHost, tenantId),
		tokenFilePath: tokenFilePath,
		mu:            sync.Mutex{},
		minLifetime:   time.Minute * 5,
	}, nil
}
