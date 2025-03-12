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

### Log Verbosity Levels

The logging verbosity can be controlled to provide different levels of detail:

- **Standard logs (level 0-2)**: Shows the main authentication flow steps and critical errors
- **Detailed logs (level 3-4)**: Shows more information about token exchange, configuration details, and non-critical warnings
- **Debug logs (level 5+)**: Shows verbose information useful for deep troubleshooting, including detailed token information

### Key Log Points

The provider logs detailed information at each stage of the authentication process:

1. **Authentication Method Detection**:
   ```
   "starting token acquisition process" auth_mode="generic-wif" ...
   ```

2. **Configuration Parsing**:
   ```
   "generic WIF configuration parameters" available_params=map[audience:...] ...
   ```

3. **Audience Resolution**:
   ```
   "using audience as-is for WIF" audience="//iam.googleapis.com/..." ...
   ```

4. **Token Exchange Process**:
   ```
   "exchanging Kubernetes ServiceAccount token for GCP identity token" ...
   "successfully obtained GCP identity token" token_expiry=... ...
   ```

5. **Service Account Impersonation** (if applicable):
   ```
   "exchanging identity token for GCP service account token" gcp_service_account="..." ...
   ```

### Troubleshooting with Logs

When diagnosing issues, pay attention to these log patterns:

- **Error logs**: Look for log entries with `level=error` which indicate failures in the authentication process
- **Token exchange failures**: Check for HTTP status codes in the logs when token exchange fails
- **Configuration issues**: The provider logs all available configuration parameters which can help identify missing or incorrect settings

To enable more verbose logging, adjust the verbosity level when deploying the provider:

```yaml
# In the provider deployment
spec:
  containers:
  - name: provider
    args:
    - "--v=4" # Increase this number for more detailed logs
```

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