# Generic Workload Identity Federation Support

This document outlines how to use the Generic Workload Identity Federation authentication method with the Secrets Store CSI Driver Provider for GCP.

## Overview

Generic Workload Identity Federation (WIF) allows non-GKE and non-Fleet-managed Kubernetes clusters to authenticate with GCP services using Workload Identity Federation. This enables secure access to Google Cloud services without the need for service account keys.

## Configuration

### SecretProviderClass Configuration

The external WIF is just available using pod-adc auth. To enable it add `wif.mode: external`  and `wif.audience: my-audience` in your SecretProviderClass configuration:

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: gcp-secrets
spec:
  provider: gcp
  parameters:
    auth: "pod-adc"
    # Required - The audience string for your WIF configuration
    wif.audience: my-audience
    # Required - Enables external WIF configuration
    wif.mode: external
    # Secret configuration as usual
    secrets: |
      - resourceName: "projects/PROJECT_ID/secrets/SECRET_NAME/versions/latest"
        path: "secret.txt"
```

### Required Parameters

- `wif.audience`: The audience string for your external WIF jwt auth. 
- `wif.mode`: The mode for the WIF auth, `external`


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
    wif.audience: "my-jwt-audience"
    secrets: |
      - resourceName: "projects/my-project/secrets/my-secret/versions/latest"
        path: "secret.txt"
```

### Step-by-Step Implementation Guide

More info at [workload-identity-federation-with-kubernetes](https://cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes#kubernetes)

Here's a complete walkthrough for implementing Generic WIF with a Helm chart:

1. **Prerequisites**:
   - GCP Secret Manager with secrets already created
   - Workload Identity Federation (WIF) configured in GCP
   - Kubernetes cluster with the Secret Store CSI Driver and GCP Provider installed

2. **Create a Workload Identity Pool and Provider in GCP (if not already done)**:
   ```bash
   # Create a Workload Identity Pool
   gcloud iam workload-identity-pools create "my-pool" \
     --project="my-project" \
     --location="global" \
     --display-name="My Kubernetes Pool"

   # Create a Workload Identity Provider
   gcloud iam workload-identity-pools providers create-oidc "my-k8s-cluster" \
     --project="my-project" \
     --location="global" \
     --workload-identity-pool="my-pool" \
     --attribute-mapping="google.subject=assertion.sub,\
        attribute.namespace=assertion['kubernetes.io']['namespace'],\
        attribute.service_account_name=assertion['kubernetes.io']['serviceaccount']['name'],\
        attribute.pod=assertion['kubernetes.io']['pod']['name']" \
     --issuer-uri="https://kubernetes.default.svc.cluster.local" \
     --allowed-audiences="my-jwt-audience" \
     --jwk-json-path=PATH_TO_JWKS_FILE
   ```

   **Note** To get JWKS_FILE do a request to k8s api, like `curl --request GET  https://<K8S_API>/openid/v1/jwks -H "Accept: application/jwk-set+json" -H "Authorization: Bearer MY_K8S_TOKEN"`

3. **Grant Access to the GCP Secret**:
   ```bash
   # Create or get a GCP service account
   gcloud iam service-accounts create "my-gcp-sa" \
     --project="my-project" \
     --display-name="My GCP Service Account"

   # Grant Secret Manager access to the service account
   gcloud secrets add-iam-policy-binding "my-secret" \
     --project="my-project" \
     --member="principalSet://iam.googleapis.com/PROJECT_NUMBER/subject/system:serviceaccount:default:my-app-sa" \
     --role="roles/secretmanager.secretAccessor"

   # Allow workload identity pool to impersonate the service account (optional)
   gcloud iam service-accounts add-iam-policy-binding "my-gcp-sa@my-project.iam.gserviceaccount.com" \
     --project="my-project" \
     --role="roles/iam.workloadIdentityUser" \
     --member="principalSet://iam.googleapis.com/PROJECT_NUMBER/subject/system:serviceaccount:default:my-app-sa"
   ```

4. **Create Kubernetes ServiceAccount and Annotations**:
   Either using your Helm chart or directly:
   ```yaml
   apiVersion: v1
   kind: ServiceAccount
   metadata:
     name: my-app-sa
     namespace: default
     annotations:
       # If using service account impersonation, add this annotation (optional)
       iam.gke.io/gcp-service-account: my-gcp-sa@my-project.iam.gserviceaccount.com
   ```
