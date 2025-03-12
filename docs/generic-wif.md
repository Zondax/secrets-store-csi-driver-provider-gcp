# Generic Workload Identity Federation Support

This document outlines how to use the Generic Workload Identity Federation authentication method with the Secrets Store CSI Driver Provider for GCP.

## Overview

Generic Workload Identity Federation (WIF) allows non-GKE and non-Fleet-managed Kubernetes clusters to authenticate with GCP services using Workload Identity Federation. This enables secure access to Google Cloud services without the need for service account keys.

## Configuration

### SecretProviderClass Configuration

To use Generic WIF, specify `auth: "generic-wif"` in your SecretProviderClass configuration along with additional WIF parameters:

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: gcp-secrets
spec:
  provider: gcp
  parameters:
    auth: "generic-wif"
    # Required - The audience string for your WIF configuration
    wif.audience: "//iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL_ID/providers/PROVIDER_ID"
    # Optional - Override the default token URL
    wif.token_url: "https://sts.googleapis.com/v1/token"
    # Optional - Specify credential source information for debugging
    wif.credential_source: "file:/var/run/secrets/tokens/gcp-ksa/token"
    # Optional - Specify an environment variable name that contains configuration
    wif.env_var: "GOOGLE_APPLICATION_CREDENTIALS"
    # Secret configuration as usual
    secrets: |
      - resourceName: "projects/PROJECT_ID/secrets/SECRET_NAME/versions/latest"
        path: "secret.txt"
```

### Required Parameters

- `wif.audience`: The audience string for your WIF configuration. This could be:
  - A standard GCP Workload Identity Pool audience like: `//iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL_ID/providers/PROVIDER_ID`
  - A namespaced identity format: `identitynamespace:POOL:PROVIDER`

### Optional Parameters

- `wif.token_url`: The token URL to use for token exchange. Defaults to `https://sts.googleapis.com/v1/token`.
- `wif.credential_source`: Information about the credential source, helpful for debugging.
- `wif.env_var`: An environment variable name that contains WIF configuration.

## Diagnostic Logs

The provider includes detailed logging to help diagnose issues with Generic WIF authentication. These logs can be viewed in the provider's pod logs.

Key log events include:
- Authentication method selection
- WIF parameter parsing
- Audience configuration
- Token exchange attempts
- Error conditions

## Example Configuration

Here's a complete example for setting up Generic WIF with GitHub Actions as the identity provider:

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: gcp-secrets
spec:
  provider: gcp
  parameters:
    auth: "generic-wif"
    wif.audience: "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/my-pool/providers/github-provider"
    secrets: |
      - resourceName: "projects/my-project/secrets/my-secret/versions/latest"
        path: "secret.txt"
```

## Troubleshooting

If you encounter issues when using Generic WIF authentication:

1. Check the provider pod logs for detailed error information
2. Verify your WIF configuration parameters are correct
3. Ensure your Kubernetes ServiceAccount has been properly configured with WIF
4. Confirm that IAM permissions are properly set for the target GCP service account 