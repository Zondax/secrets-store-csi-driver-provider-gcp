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
	"os/user"
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
	"golang.org/x/oauth2/google/externalaccount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	cloudScope                 = "https://www.googleapis.com/auth/cloud-platform"
	adcEnvVar                  = "GOOGLE_APPLICATION_CREDENTIALS"
	adcWellKnown               = "application_default_credentials.json"
	adcImpersonationAnnotation = "iam.gke.io/gcp-service-account"
	adcImpersonationURL        = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts"
)

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
	Type           string `json:"type"`
	UniverseDomain string `json:"universe_domain"`

	// External Account fields
	Audience                       string                           `json:"audience"`
	SubjectTokenType               string                           `json:"subject_token_type"`
	TokenURLExternal               string                           `json:"token_url"`
	TokenInfoURL                   string                           `json:"token_info_url"`
	ServiceAccountImpersonationURL string                           `json:"service_account_impersonation_url"`
	Delegates                      []string                         `json:"delegates"`
	CredentialSource               externalaccount.CredentialSource `json:"credential_source"`
	QuotaProjectID                 string                           `json:"quota_project_id"`
	WorkforcePoolUserProject       string                           `json:"workforce_pool_user_project"`

	// External Account Authorized User fields
	RevokeURL string `json:"revoke_url"`

	// Service account impersonation
	SourceCredentials *credentialsFile `json:"source_credentials"`
}

type K8STokenSupplier struct {
	token string
}

func (c *K8STokenSupplier) SubjectToken(ctx context.Context, options externalaccount.SupplierOptions) (string, error) {
	if c.token == "" {
		return "", fmt.Errorf("K8STokenSupplier token is empty")
	}
	return c.token, nil
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
		if cfg.AuthPodADCExternal {
			return c.externalTokenSource(ctx, cfg.PodInfo, cfg.WIFConfig["audience"])
		}

		token, err := c.Token(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("unable to obtain workload identity auth: %v", err)
		}
		return oauth2.StaticTokenSource(token), nil
	}

	return nil, errors.New("mount configuration has no auth method configured")
}

func (c *Client) externalTokenSource(ctx context.Context, podInfo *config.PodInfo, audience string) (oauth2.TokenSource, error) {
	saToken, err := c.getPodSAToken(ctx, podInfo, audience)
	if err != nil {
		return nil, err
	}

	saImpersonation, err := c.getPodSAImpersonation(ctx, podInfo)
	if err != nil {
		return nil, err
	}

	// Getting credFile from ENV
	credFile, err := credentialFileFromENV(ctx)
	if err != nil {
		return nil, err
	}

	// Generating external account config with SA token
	config := externalaccount.Config{
		Audience:         credFile.Audience,
		SubjectTokenType: credFile.SubjectTokenType,
		TokenURL:         credFile.TokenURLExternal,
		TokenInfoURL:     credFile.TokenInfoURL,
		//ServiceAccountImpersonationURL: credFile.ServiceAccountImpersonationURL,
		//ServiceAccountImpersonationLifetimeSeconds: credFile.ServiceAccountImpersonationLifetimeSeconds,
		//ClientSecret: credFile.ClientSecret,
		//ClientID: credFile.ClientID,
		//CredentialSource *CredentialSource
		QuotaProjectID:           credFile.QuotaProjectID,
		Scopes:                   []string{cloudScope},
		WorkforcePoolUserProject: credFile.WorkforcePoolUserProject,
		// Setting K8STokenSupplier with SA token
		SubjectTokenSupplier: &K8STokenSupplier{
			token: saToken,
		},
		//AwsSecurityCredentialsSupplier: credFile.TokenURLExternal,
		UniverseDomain: credFile.UniverseDomain,
	}

	if saImpersonation != "" {
		// Generating ServiceAccountImpersonationURL when SA has impersonation annotation
		config.ServiceAccountImpersonationURL = fmt.Sprintf("%s/%s:generateAccessToken", adcImpersonationURL, saImpersonation)
	}

	return externalaccount.NewTokenSource(ctx, config)
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

	klog.InfoS("using standard workload identity mode",
		"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

	idPool, idProvider, err = c.gkeWorkloadIdentity(ctx, cfg)
	if err != nil {
		idPool, idProvider, audience, err = c.fleetWorkloadIdentity(ctx, cfg)
		if err != nil {
			klog.ErrorS(err, "both GKE and fleet workload identity failed",
				"pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
			return nil, err
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
	gcpSA, err := c.getPodSAImpersonation(ctx, cfg.PodInfo)
	if err != nil {
		return nil, err
	}

	saTokenVal, err := c.getSAToken(ctx, cfg.PodInfo, idPool, audience)
	if err != nil {
		return nil, fmt.Errorf("unable to get SA token: %w", err)
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

func (c *Client) getPodSAImpersonation(ctx context.Context, podInfo *config.PodInfo) (string, error) {
	saResp, err := c.KubeClient.
		CoreV1().
		ServiceAccounts(podInfo.Namespace).
		Get(ctx, podInfo.ServiceAccount, v1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "failed to retrieve ServiceAccount information",
			"namespace", podInfo.Namespace,
			"service_account", podInfo.ServiceAccount,
			"pod", klog.ObjectRef{Namespace: podInfo.Namespace, Name: podInfo.Name})
		return "", fmt.Errorf("unable to fetch SA info: %w", err)
	}

	return saResp.Annotations[adcImpersonationAnnotation], nil
}

func (c *Client) getSAToken(ctx context.Context, podInfo *config.PodInfo, idPool, audience string) (string, error) {
	aud := idPool
	if aud == "" {
		aud = audience
	}
	return c.getPodSAToken(ctx, podInfo, aud)
}

func (c *Client) getPodSAToken(ctx context.Context, podinfo *config.PodInfo, audience string) (string, error) {
	if podinfo.ServiceAccountTokens != "" {
		saToken, err := c.extractPodSAToken(podinfo, audience) // calling function to extract token received from driver.
		if err != nil {
			klog.ErrorS(err, "failed to extract ServiceAccount token from driver tokens",
				"pod", klog.ObjectRef{Namespace: podinfo.Namespace, Name: podinfo.Name})
			return "", fmt.Errorf("unable to fetch SA token from driver: %w", err)
		}

		return saToken.Token, nil
	}

	saToken, err := c.generatePodSAToken(ctx, podinfo, audience) // if no token received, provider generates its own token.
	if err != nil {
		klog.ErrorS(err, "failed to generate ServiceAccount token",
			"pod", klog.ObjectRef{Namespace: podinfo.Namespace, Name: podinfo.Name},
			"service_account", podinfo.ServiceAccount)
		return "", fmt.Errorf("unable to generate SA token: %w", err)
	}

	return saToken.Token, nil
}

func (c *Client) extractPodSAToken(podinfo *config.PodInfo, audience string) (*authenticationv1.TokenRequestStatus, error) {
	klog.V(5).InfoS("extracting SA token from driver-provided tokens",
		"pod", klog.ObjectRef{Namespace: podinfo.Namespace, Name: podinfo.Name})

	audienceTokens := map[string]authenticationv1.TokenRequestStatus{}
	if err := json.Unmarshal([]byte(podinfo.ServiceAccountTokens), &audienceTokens); err != nil {
		return nil, err
	}

	if v, ok := audienceTokens[audience]; ok {
		return &v, nil
	}

	return nil, fmt.Errorf("no SA token for pod %s/%s has audience value %s", podinfo.Namespace, podinfo.Name, audience)
}

func (c *Client) generatePodSAToken(ctx context.Context, podinfo *config.PodInfo, audience string) (*authenticationv1.TokenRequestStatus, error) {
	klog.V(5).InfoS("generating SA token from driver-provided tokens",
		"pod", klog.ObjectRef{Namespace: podinfo.Namespace, Name: podinfo.Name})

	ttl := int64((15 * time.Minute).Seconds())
	resp, err := c.KubeClient.CoreV1().
		ServiceAccounts(podinfo.Namespace).
		CreateToken(ctx, podinfo.ServiceAccount,
			&authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					ExpirationSeconds: &ttl,
					Audiences:         []string{audience},
					BoundObjectRef: &authenticationv1.BoundObjectReference{
						Kind:       "Pod", // Pod and secret are the only valid types
						APIVersion: "v1",
						Name:       podinfo.Name,
						UID:        podinfo.UID,
					},
				},
			},
			v1.CreateOptions{},
		)
	if err != nil {
		return nil, fmt.Errorf("unable to generate SA token for pod %s/%s: %w", podinfo.Namespace, podinfo.Name, err)
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
	var jsonData []byte
	var err error
	if filename := os.Getenv(adcEnvVar); filename != "" {
		jsonData, err = os.ReadFile(filepath.Clean(filename))
		if err != nil {
			return "", "", "", fmt.Errorf("google: error getting credentials using %v environment variable: %v", adcEnvVar, err)
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
		"scope":                cloudScope,
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

func wellKnownFile() string {
	home := os.Getenv("HOME")
	if home == "" {
		u, err := user.Current()
		if err != nil {
			return ""
		}
		home = u.HomeDir
	}
	return fmt.Sprintf("%s/.config/gcloud/%s", home, adcWellKnown)
}

func credentialFileFromENV(ctx context.Context) (*credentialsFile, error) {
	filename := os.Getenv(adcEnvVar)
	if filename == "" {
		// Second, try a well-known file.
		filename = wellKnownFile()
		if filename == "" {
			return nil, fmt.Errorf("google: error getting adc well known files")
		}
	}
	jsonData, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("google: error reading credentials from well known files: %v", err)
	}
	return credentialFileFromJSON(ctx, jsonData)
}

func credentialFileFromJSON(ctx context.Context, jsonData []byte) (*credentialsFile, error) {
	credFile := &credentialsFile{}
	if err := json.Unmarshal(jsonData, &credFile); err != nil {
		return nil, fmt.Errorf("google: error parsing credentials from well known files: %v", err)
	}
	return credFile, nil
}
