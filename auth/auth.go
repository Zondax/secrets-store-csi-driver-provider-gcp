// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package auth includes obtains auth tokens for workload identity.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/config"
	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/csrmetrics"
	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/vars"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const cloudScope = "https://www.googleapis.com/auth/cloud-platform"

type Client struct {
	KubeClient     *kubernetes.Clientset
	MetadataClient *metadata.Client
	IAMClient      *credentials.IamCredentialsClient
	HTTPClient     *http.Client
}

// JSON key file types.
const (
	externalAccountKey = "external_account"
)

// credentialsFile is the unmarshalled representation of a credentials file.
type credentialsFile struct {
	Type string `json:"type"`
	// External Account fields
	Audience string `json:"audience"`
}

// TokenSource returns the correct oauth2.TokenSource depending on the auth
// configuration of the MountConfig.
func (c *Client) TokenSource(ctx context.Context, cfg *config.MountConfig) (oauth2.TokenSource, error) {
	allowSecretRef, err := vars.AllowNodepublishSeretRef.GetBooleanValue()
	if err != nil {
		klog.ErrorS(err, "failed to get ALLOW_NODE_PUBLISH_SECRET flag")
		klog.Fatal("failed to get ALLOW_NODE_PUBLISH_SECRET flag")
	}
	if cfg.AuthNodePublishSecret && allowSecretRef {
		creds, err := google.CredentialsFromJSON(ctx, cfg.AuthKubeSecret, cloudScope)
		if err != nil {
			return nil, fmt.Errorf("unable to generate credentials from key.json: %w", err)
		}
		return creds.TokenSource, nil
	}

	if cfg.AuthProviderADC {
		return google.DefaultTokenSource(ctx, cloudScope)
	}

	if cfg.AuthPodADC {
		token, err := c.Token(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("unable to obtain workload identity auth: %v", err)
		}
		return oauth2.StaticTokenSource(token), nil
	}

	if cfg.AuthGenericWIF {
		klog.InfoS("using generic WIF for authentication")
		token, err := c.Token(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("unable to obtain generic workload identity federation auth: %v", err)
		}
		return oauth2.StaticTokenSource(token), nil
	}

	return nil, errors.New("mount configuration has no auth method configured")
}

// Token fetches a workload identity auth token for the pod for the MountConfig.
//
// This requires obtaining a ServiceAccount token from the K8S API for the pod,
// trading that token for an identitybindingtoken using the
// securetoken.googleapis.com API, and then trading that token for a GCP
// Service Account token using the iamcredentials.googleapis.com API.
//
// Caveats:
//
// None of the API calls are cached since the plugin binary is executed once per
// mount event. The tokens are to be used immediately so no refresh abilities are
// implemented - blocking Issue #14.
//
// This method requires additional K8S API permission for the CSI driver
// daemonset, including serviceaccounts/token create and serviceaccounts get.
// These permissions could break node isolation and a long term solution is
// tracked by Issue #13.
//
// Token sent by driver is extracted and used. However, if tokenRequests is not set
// in driver spec, the provider does not receive any tokens from driver and generates
// its own token. Token creation can be removed once driver implements the requiresRepublish.
func (c *Client) Token(ctx context.Context, cfg *config.MountConfig) (*oauth2.Token, error) {
	var audience string
	var idPool, idProvider string
	var err error

	klog.InfoS("starting token acquisition process",
		"auth_mode", getAuthMode(cfg),
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	if cfg.AuthGenericWIF {
		klog.InfoS("using generic WIF authentication mode",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

		idPool, idProvider, audience, err = c.genericWIFAuth(ctx, cfg)
		if err != nil {
			klog.ErrorS(err, "generic WIF authentication failed",
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
			return nil, fmt.Errorf("generic WIF authentication failed: %w", err)
		}
		klog.InfoS("generic WIF authentication successful",
			"idPool", idPool,
			"idProvider", idProvider,
			"audience", audience,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	} else {
		klog.InfoS("using standard workload identity mode",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

		idPool, idProvider, err = c.gkeWorkloadIdentity(ctx, cfg)
		if err != nil {
			klog.V(4).InfoS("GKE workload identity failed, trying fleet workload identity",
				"error", err,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

			idPool, idProvider, audience, err = c.fleetWorkloadIdentity(ctx, cfg)
			if err != nil {
				klog.ErrorS(err, "both GKE and fleet workload identity failed",
					"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
				return nil, err
			}
			klog.InfoS("fleet workload identity configuration detected",
				"idPool", idPool,
				"idProvider", idProvider,
				"audience", audience,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		} else {
			klog.InfoS("GKE workload identity configuration detected",
				"idPool", idPool,
				"idProvider", idProvider,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		}
	}

	if audience == "" {
		audience = fmt.Sprintf("identitynamespace:%s:%s", idPool, idProvider)
		klog.V(5).InfoS("workload id configured", "pool", idPool, "provider", idProvider)
	} else {
		klog.V(5).InfoS("workload federation pool audience", audience)
	}

	klog.InfoS("audience determined for token exchange",
		"audience", audience,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	// Get iam.gke.io/gcp-service-account annotation to see if the
	// identitybindingtoken token should be traded for a GCP SA token.
	// See https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity#creating_a_relationship_between_ksas_and_gsas
	klog.InfoS("retrieving ServiceAccount information from Kubernetes",
		"namespace", cfg.PodInfo.Namespace,
		"service_account", cfg.PodInfo.ServiceAccount,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	saResp, err := c.KubeClient.
		CoreV1().
		ServiceAccounts(cfg.PodInfo.Namespace).
		Get(ctx, cfg.PodInfo.ServiceAccount, v1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "failed to retrieve ServiceAccount information",
			"namespace", cfg.PodInfo.Namespace,
			"service_account", cfg.PodInfo.ServiceAccount,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		return nil, fmt.Errorf("unable to fetch SA info: %w", err)
	}

	gcpSA := saResp.Annotations["iam.gke.io/gcp-service-account"]
	klog.InfoS("service account annotation check complete",
		"k8s_service_account", cfg.PodInfo.ServiceAccount,
		"gcp_service_account_annotation", gcpSA,
		"has_gcp_sa_annotation", gcpSA != "",
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	klog.V(5).InfoS("matched service account", "service_account", gcpSA)

	// Obtain a serviceaccount token for the pod.
	klog.InfoS("obtaining Kubernetes ServiceAccount token",
		"service_account", cfg.PodInfo.ServiceAccount,
		"namespace", cfg.PodInfo.Namespace,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
		"has_driver_token", cfg.PodInfo.ServiceAccountTokens != "")

	var saTokenVal string
	if cfg.PodInfo.ServiceAccountTokens != "" {
		klog.V(4).InfoS("extracting ServiceAccount token from driver-provided tokens",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

		saToken, err := c.extractSAToken(cfg, idPool, audience) // calling function to extract token received from driver.
		if err != nil {
			klog.ErrorS(err, "failed to extract ServiceAccount token from driver tokens",
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
			return nil, fmt.Errorf("unable to fetch SA token from driver: %w", err)
		}

		klog.InfoS("successfully extracted ServiceAccount token from driver",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
			"token_expiration", saToken.ExpirationTimestamp)

		saTokenVal = saToken.Token
	} else {
		klog.V(4).InfoS("generating new ServiceAccount token",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
			"service_account", cfg.PodInfo.ServiceAccount,
			"namespace", cfg.PodInfo.Namespace)

		saToken, err := c.generatePodSAToken(ctx, cfg, idPool, audience) // if no token received, provider generates its own token.
		if err != nil {
			klog.ErrorS(err, "failed to generate ServiceAccount token",
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
				"service_account", cfg.PodInfo.ServiceAccount)
			return nil, fmt.Errorf("unable to fetch pod token: %w", err)
		}

		klog.InfoS("successfully generated new ServiceAccount token",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
			"token_expiration", saToken.ExpirationTimestamp)

		saTokenVal = saToken.Token
	}

	// Trade the kubernetes token for an identitybindingtoken token.
	klog.InfoS("exchanging Kubernetes ServiceAccount token for GCP identity token",
		"audience", audience,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	idBindToken, err := tradeIDBindToken(ctx, c.HTTPClient, saTokenVal, audience)
	if err != nil {
		klog.ErrorS(err, "failed to exchange ServiceAccount token for identity token",
			"audience", audience,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		return nil, fmt.Errorf("unable to fetch identitybindingtoken: %w", err)
	}

	klog.InfoS("successfully obtained GCP identity token",
		"token_type", idBindToken.TokenType,
		"token_expiry", idBindToken.Expiry,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	// If no `iam.gke.io/gcp-service-account` annotation is present the
	// identitybindingtoken will be used directly, allowing bindings on secrets
	// of the form "serviceAccount:<project>.svc.id.goog[<namespace>/<sa>]".
	if gcpSA == "" {
		klog.InfoS("no GCP service account annotation found, using identity token directly",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		return idBindToken, nil
	}

	// Exchange identity token for GCP service account token
	klog.InfoS("exchanging identity token for GCP service account token",
		"gcp_service_account", gcpSA,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	gcpSAResp, err := c.IAMClient.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{
		Name:  fmt.Sprintf("projects/-/serviceAccounts/%s", gcpSA),
		Scope: secretmanager.DefaultAuthScopes(),
	}, gax.WithGRPCOptions(grpc.PerRPCCredentials(oauth.TokenSource{TokenSource: oauth2.StaticTokenSource(idBindToken)})))

	if err != nil {
		klog.ErrorS(err, "failed to exchange identity token for GCP service account token",
			"gcp_service_account", gcpSA,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		return nil, fmt.Errorf("unable to fetch gcp service account token: %w", err)
	}

	klog.InfoS("successfully obtained GCP service account token",
		"gcp_service_account", gcpSA,
		"token_expiry", gcpSAResp.GetExpireTime().AsTime(),
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	return &oauth2.Token{AccessToken: gcpSAResp.GetAccessToken()}, nil
}

// Helper function to determine the auth mode string
func getAuthMode(cfg *config.MountConfig) string {
	if cfg.AuthGenericWIF {
		return "generic-wif"
	} else if cfg.AuthPodADC {
		return "pod-adc"
	} else if cfg.AuthProviderADC {
		return "provider-adc"
	} else if cfg.AuthNodePublishSecret {
		return "node-publish-secret"
	}
	return "unknown"
}

func (c *Client) extractSAToken(cfg *config.MountConfig, idPool, audience string) (*authenticationv1.TokenRequestStatus, error) {
	audienceTokens := map[string]authenticationv1.TokenRequestStatus{}
	if err := json.Unmarshal([]byte(cfg.PodInfo.ServiceAccountTokens), &audienceTokens); err != nil {
		return nil, err
	}
	for k, v := range audienceTokens {
		if k == idPool || k == audience { // Only returns the token if the audience is the workload identity. Other tokens cannot be used.
			return &v, nil
		}
	}
	return nil, fmt.Errorf("no token has audience value of idPool")
}

func (c *Client) generatePodSAToken(ctx context.Context, cfg *config.MountConfig, idPool, audience string) (*authenticationv1.TokenRequestStatus, error) {
	ttl := int64((15 * time.Minute).Seconds())
	_audience := idPool
	if _audience == "" {
		_audience = audience
	}
	resp, err := c.KubeClient.CoreV1().
		ServiceAccounts(cfg.PodInfo.Namespace).
		CreateToken(ctx, cfg.PodInfo.ServiceAccount,
			&authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					ExpirationSeconds: &ttl,
					Audiences:         []string{_audience},
					BoundObjectRef: &authenticationv1.BoundObjectReference{
						Kind:       "Pod", // Pod and secret are the only valid types
						APIVersion: "v1",
						Name:       cfg.PodInfo.Name,
						UID:        cfg.PodInfo.UID,
					},
				},
			},
			v1.CreateOptions{},
		)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch pod token: %w", err)
	}
	return &resp.Status, nil
}

func (c *Client) gkeWorkloadIdentity(ctx context.Context, cfg *config.MountConfig) (string, string, error) {
	// Determine Workload ID parameters from the GCE instance metadata.
	projectID, err := c.MetadataClient.ProjectIDWithContext(ctx)
	if err != nil {
		return "", "", fmt.Errorf("unable to get project id: %w", err)
	}
	idPool := fmt.Sprintf("%s.svc.id.goog", projectID)

	clusterLocation, err := c.MetadataClient.InstanceAttributeValueWithContext(ctx, "cluster-location")
	if err != nil {
		return "", "", fmt.Errorf("unable to determine cluster location: %w", err)
	}
	clusterName, err := c.MetadataClient.InstanceAttributeValueWithContext(ctx, "cluster-name")
	if err != nil {
		return "", "", fmt.Errorf("unable to determine cluster name: %w", err)
	}

	gkeWorkloadIdentityProviderEndpoint, err := vars.GkeWorkloadIdentityEndPoint.GetValue()
	if err != nil {
		return "", "", fmt.Errorf("unable to read GKE workload identity provider endpoint: %w", err)
	}
	idProvider := fmt.Sprintf("%s/projects/%s/locations/%s/clusters/%s", gkeWorkloadIdentityProviderEndpoint, projectID, clusterLocation, clusterName)

	return idPool, idProvider, nil
}

func (c *Client) fleetWorkloadIdentity(ctx context.Context, cfg *config.MountConfig) (string, string, string, error) {
	const envVar = "GOOGLE_APPLICATION_CREDENTIALS"
	var jsonData []byte
	var err error
	if filename := os.Getenv(envVar); filename != "" {
		jsonData, err = os.ReadFile(filepath.Clean(filename))
		if err != nil {
			return "", "", "", fmt.Errorf("google: error getting credentials using %v environment variable: %v", envVar, err)
		}
	}

	// Parse jsonData as one of the other supported credentials files.
	var f credentialsFile
	if err := json.Unmarshal(jsonData, &f); err != nil {
		return "", "", "", err
	}

	if f.Type != externalAccountKey {
		return "", "", "", fmt.Errorf("google: unexpected credentials type: %v, expected: %v", f.Type, externalAccountKey)
	}

	split := strings.SplitN(f.Audience, ":", 3)
	if split == nil || len(split) < 3 {
		// If the audience is not in the expected format, return the audience as the audience since this is likely a federated pool.
		return "", "", f.Audience, nil
	}
	idPool := split[1]
	idProvider := split[2]

	return idPool, idProvider, "", nil
}

func tradeIDBindToken(ctx context.Context, client *http.Client, k8sToken, audience string) (*oauth2.Token, error) {
	tokenLen := 0
	if k8sToken != "" {
		tokenLen = len(k8sToken)
		klog.V(4).InfoS("preparing to exchange k8s token for identity token",
			"k8s_token_length", tokenLen,
			"audience", audience)
	} else {
		klog.ErrorS(nil, "k8s token is empty, token exchange will fail")
		return nil, fmt.Errorf("k8s token is empty")
	}

	// Prepare request body for token exchange
	body, err := json.Marshal(map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"subject_token":        k8sToken,
		"audience":             audience,
		"scope":                "https://www.googleapis.com/auth/cloud-platform",
	})
	if err != nil {
		klog.ErrorS(err, "failed to marshal token exchange request body")
		return nil, err
	}

	identityBindingTokenEndPoint, err := vars.IdentityBindingTokenEndPoint.GetValue()
	if err != nil {
		klog.ErrorS(err, "failed to get identity binding token endpoint")
		return nil, fmt.Errorf("unable to read identity binding token endpoint: %w", err)
	}

	klog.V(4).InfoS("sending token exchange request",
		"endpoint", identityBindingTokenEndPoint,
		"audience", audience,
		"k8s_token_length", tokenLen)

	req, err := http.NewRequestWithContext(ctx, "POST", identityBindingTokenEndPoint, bytes.NewBuffer(body))
	if err != nil {
		klog.ErrorS(err, "failed to create token exchange HTTP request")
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	gcpIamMetricRecorder := csrmetrics.OutboundRPCStartRecorder("gcp_iam_get_id_bind_token_requests")

	// Start timer for measuring token exchange latency
	startTime := time.Now()
	resp, err := client.Do(req)
	exchangeDuration := time.Since(startTime)

	if err != nil {
		klog.ErrorS(err, "token exchange HTTP request failed",
			"duration_ms", exchangeDuration.Milliseconds(),
			"endpoint", identityBindingTokenEndPoint)
		return nil, err
	}

	klog.V(4).InfoS("token exchange HTTP request completed",
		"status_code", resp.StatusCode,
		"duration_ms", exchangeDuration.Milliseconds())

	gcpIamMetricRecorder(csrmetrics.OutboundRPCStatus(strconv.Itoa(resp.StatusCode)))

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		klog.ErrorS(nil, "token exchange request failed with non-200 status code",
			"status_code", resp.StatusCode,
			"response_body", string(respBody),
			"endpoint", identityBindingTokenEndPoint)
		return nil, fmt.Errorf("could not get idbindtoken token, status: %v, body: %s", resp.StatusCode, string(respBody))
	}

	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		klog.ErrorS(err, "failed to read token exchange response body")
		return nil, err
	}

	idBindToken := &oauth2.Token{}
	if err := json.Unmarshal(respBody, idBindToken); err != nil {
		klog.ErrorS(err, "failed to unmarshal token exchange response",
			"response_body_length", len(respBody))
		return nil, err
	}

	klog.V(4).InfoS("token exchange successful",
		"token_type", idBindToken.TokenType,
		"expiry", idBindToken.Expiry,
		"has_refresh_token", idBindToken.RefreshToken != "")

	return idBindToken, nil
}

func (c *Client) genericWIFAuth(ctx context.Context, cfg *config.MountConfig) (string, string, string, error) {
	klog.InfoS("starting generic WIF authentication",
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
		"service_account", cfg.PodInfo.ServiceAccount)

	// Log all available WIF config for easier debugging
	klog.InfoS("generic WIF configuration parameters",
		"available_params", cfg.WIFConfig,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	// Check for required configuration
	audience, ok := cfg.WIFConfig["audience"]
	if !ok {
		klog.ErrorS(nil, "missing required 'wif.audience' configuration for generic WIF",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name},
			"available_params", cfg.WIFConfig)
		return "", "", "", fmt.Errorf("missing required 'wif.audience' configuration for generic WIF")
	}

	klog.InfoS("using WIF audience from configuration",
		"audience", audience,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	// Handle different audience formats
	// Format 1: Direct audience string for Workload Identity Federation pools
	// Format 2: identitynamespace:<idPool>:<idProvider>
	var idPool, idProvider string

	if strings.HasPrefix(audience, "identitynamespace:") {
		// Try to parse the audience string in the standard format
		split := strings.SplitN(audience, ":", 3)
		if len(split) >= 3 {
			idPool = split[1]
			idProvider = split[2]
			klog.InfoS("parsed identity namespace components from audience",
				"idPool", idPool,
				"idProvider", idProvider,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
			return idPool, idProvider, "", nil
		} else {
			klog.ErrorS(nil, "audience has identitynamespace prefix but is malformed",
				"audience", audience,
				"expected_format", "identitynamespace:<idPool>:<idProvider>",
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		}
	}

	// If not in standard format, use it directly as audience
	// Expect format like "//iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL_ID/providers/PROVIDER_ID"
	klog.InfoS("using audience as-is for WIF",
		"audience", audience,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	// Check if the audience matches expected WIF pool format
	if strings.Contains(audience, "workloadIdentityPools") {
		klog.InfoS("audience appears to be a GCP Workload Identity Pool",
			"audience", audience,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	} else {
		klog.V(4).InfoS("audience format doesn't match standard GCP WIF pool format, proceed with caution",
			"audience", audience,
			"expected_to_contain", "workloadIdentityPools",
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	}

	// For debugging/telemetry, log credential source if provided
	if credSource, ok := cfg.WIFConfig["credential_source"]; ok {
		klog.InfoS("credential source specified for generic WIF",
			"credential_source", credSource,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	} else {
		klog.V(4).InfoS("no credential source specified, will use default k8s ServiceAccount token",
			"service_account", cfg.PodInfo.ServiceAccount,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	}

	// Get token URL from config or use default
	tokenURL := cfg.WIFConfig["token_url"]
	if tokenURL == "" {
		tokenURL = "https://sts.googleapis.com/v1/token"
		klog.InfoS("using default token URL for generic WIF",
			"token_url", tokenURL,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	} else {
		klog.InfoS("using configured token URL for generic WIF",
			"token_url", tokenURL,
			"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
	}

	// Override environment variable if provided
	if envVarName, ok := cfg.WIFConfig["env_var"]; ok {
		envVarVal := os.Getenv(envVarName)
		if envVarVal != "" {
			klog.InfoS("found environment variable for generic WIF",
				"env_var", envVarName,
				"env_var_exists", true,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		} else {
			klog.ErrorS(nil, "environment variable specified but not found",
				"env_var", envVarName,
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		}
	}

	// Log the result of audience resolution
	klog.InfoS("generic WIF audience resolution complete",
		"resolved_audience", audience,
		"idPool", idPool,
		"idProvider", idProvider,
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	return "", "", audience, nil
}
