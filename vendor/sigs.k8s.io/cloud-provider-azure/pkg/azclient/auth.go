/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azclient

import (
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/crypto/pkcs12"
)

var (
	// ErrorNoAuth indicates that no credentials are provided.
	ErrorNoAuth = fmt.Errorf("no credentials provided for Azure cloud provider")
)

type AuthProvider struct {
	FederatedIdentityCredential   azcore.TokenCredential
	ManagedIdentityCredential     azcore.TokenCredential
	ClientSecretCredential        azcore.TokenCredential
	NetworkClientSecretCredential azcore.TokenCredential
	MultiTenantCredential         azcore.TokenCredential
	ClientCertificateCredential   azcore.TokenCredential
}

func NewAuthProvider(armConfig *ARMClientConfig, config *AzureAuthConfig, clientOptionsMutFn ...func(option *policy.ClientOptions)) (*AuthProvider, error) {
	clientOption, err := GetAzCoreClientOption(armConfig)
	if err != nil {
		return nil, err
	}
	for _, fn := range clientOptionsMutFn {
		fn(clientOption)
	}
	// federatedIdentityCredential is used for workload identity federation
	var federatedIdentityCredential azcore.TokenCredential
	if aadFederatedTokenFile, enabled := config.GetAzureFederatedTokenFile(); enabled {
		federatedIdentityCredential, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: *clientOption,
			ClientID:      config.GetAADClientID(),
			TenantID:      armConfig.GetTenantID(),
			TokenFilePath: aadFederatedTokenFile,
		})
		if err != nil {
			return nil, err
		}
	}

	// managedIdentityCredential is used for managed identity extension
	var managedIdentityCredential azcore.TokenCredential
	if config.UseManagedIdentityExtension {
		credOptions := &azidentity.ManagedIdentityCredentialOptions{
			ClientOptions: *clientOption,
		}
		if len(config.UserAssignedIdentityID) > 0 {
			if strings.Contains(strings.ToUpper(config.UserAssignedIdentityID), "/SUBSCRIPTIONS/") {
				credOptions.ID = azidentity.ResourceID(config.UserAssignedIdentityID)
			} else {
				credOptions.ID = azidentity.ClientID(config.UserAssignedIdentityID)
			}
		}
		managedIdentityCredential, err = azidentity.NewManagedIdentityCredential(credOptions)
		if err != nil {
			return nil, err
		}
	}

	// ClientSecretCredential is used for client secret
	var clientSecretCredential azcore.TokenCredential
	var networkClientSecretCredential azcore.TokenCredential
	var multiTenantCredential azcore.TokenCredential
	if len(config.GetAADClientSecret()) > 0 {
		credOptions := &azidentity.ClientSecretCredentialOptions{
			ClientOptions: *clientOption,
		}
		clientSecretCredential, err = azidentity.NewClientSecretCredential(armConfig.GetTenantID(), config.GetAADClientID(), config.GetAADClientSecret(), credOptions)
		if err != nil {
			return nil, err
		}
		if len(armConfig.NetworkResourceTenantID) > 0 && !strings.EqualFold(armConfig.NetworkResourceTenantID, armConfig.GetTenantID()) {
			credOptions := &azidentity.ClientSecretCredentialOptions{
				ClientOptions: *clientOption,
			}
			networkClientSecretCredential, err = azidentity.NewClientSecretCredential(armConfig.NetworkResourceTenantID, config.GetAADClientID(), config.AADClientSecret, credOptions)
			if err != nil {
				return nil, err
			}

			credOptions = &azidentity.ClientSecretCredentialOptions{
				ClientOptions:              *clientOption,
				AdditionallyAllowedTenants: []string{armConfig.NetworkResourceTenantID},
			}
			multiTenantCredential, err = azidentity.NewClientSecretCredential(armConfig.GetTenantID(), config.GetAADClientID(), config.GetAADClientSecret(), credOptions)
			if err != nil {
				return nil, err
			}

		}
	}

	// ClientCertificateCredential is used for client certificate
	var clientCertificateCredential azcore.TokenCredential
	if len(config.AADClientCertPath) > 0 && len(config.AADClientCertPassword) > 0 {
		credOptions := &azidentity.ClientCertificateCredentialOptions{
			ClientOptions:        *clientOption,
			SendCertificateChain: true,
		}
		certData, err := os.ReadFile(config.AADClientCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the client certificate from file %s: %w", config.AADClientCertPath, err)
		}
		certificate, privateKey, err := decodePkcs12(certData, config.AADClientCertPassword)
		if err != nil {
			return nil, fmt.Errorf("decoding the client certificate: %w", err)
		}
		clientCertificateCredential, err = azidentity.NewClientCertificateCredential(armConfig.GetTenantID(), config.GetAADClientID(), []*x509.Certificate{certificate}, privateKey, credOptions)
		if err != nil {
			return nil, err
		}
	}

	return &AuthProvider{
		FederatedIdentityCredential:   federatedIdentityCredential,
		ManagedIdentityCredential:     managedIdentityCredential,
		ClientSecretCredential:        clientSecretCredential,
		ClientCertificateCredential:   clientCertificateCredential,
		NetworkClientSecretCredential: networkClientSecretCredential,
		MultiTenantCredential:         multiTenantCredential,
	}, nil
}

func (factory *AuthProvider) GetAzIdentity() (azcore.TokenCredential, error) {
	switch true {
	case factory.FederatedIdentityCredential != nil:
		return factory.FederatedIdentityCredential, nil
	case factory.ManagedIdentityCredential != nil:
		return factory.ManagedIdentityCredential, nil
	case factory.ClientSecretCredential != nil:
		return factory.ClientSecretCredential, nil
	case factory.ClientCertificateCredential != nil:
		return factory.ClientCertificateCredential, nil
	default:
		return nil, ErrorNoAuth
	}
}

// decodePkcs12 decodes a PKCS#12 client certificate by extracting the public certificate and
// the private RSA key
func decodePkcs12(pkcs []byte, password string) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, certificate, err := pkcs12.Decode(pkcs, password)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the PKCS#12 client certificate: %w", err)
	}
	rsaPrivateKey, isRsaKey := privateKey.(*rsa.PrivateKey)
	if !isRsaKey {
		return nil, nil, fmt.Errorf("PKCS#12 certificate must contain a RSA private key")
	}

	return certificate, rsaPrivateKey, nil
}

func (factory *AuthProvider) GetNetworkAzIdentity() (azcore.TokenCredential, error) {
	if factory.NetworkClientSecretCredential != nil {
		return factory.NetworkClientSecretCredential, nil
	}
	return nil, ErrorNoAuth
}

func (factory *AuthProvider) GetMultiTenantIdentity() (azcore.TokenCredential, error) {
	if factory.MultiTenantCredential != nil {
		return factory.MultiTenantCredential, nil
	}
	return nil, ErrorNoAuth
}

func (factory *AuthProvider) IsMultiTenantModeEnabled() bool {
	return factory.MultiTenantCredential != nil
}
