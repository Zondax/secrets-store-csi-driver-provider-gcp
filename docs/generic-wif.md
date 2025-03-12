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

## Deployment Examples

This section provides practical examples for deploying applications that use Generic WIF for accessing GCP Secret Manager secrets using common Kubernetes deployment tools.

### Helm Chart Example

Below is an example of how to structure a Helm chart that uses the Generic WIF authentication method to mount GCP secrets.

#### Directory Structure

```
my-app/
├── Chart.yaml
├── templates/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── serviceaccount.yaml
│   └── secretproviderclass.yaml
└── values.yaml
```

#### secretproviderclass.yaml

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: {{ include "my-app.fullname" . }}-gcp-secrets
spec:
  provider: gcp
  parameters:
    auth: "generic-wif"
    wif.audience: {{ .Values.secrets.wif.audience | quote }}
    {{- if .Values.secrets.wif.tokenUrl }}
    wif.token_url: {{ .Values.secrets.wif.tokenUrl | quote }}
    {{- end }}
    {{- if .Values.secrets.wif.credentialSource }}
    wif.credential_source: {{ .Values.secrets.wif.credentialSource | quote }}
    {{- end }}
    secrets: |
      {{- range .Values.secrets.items }}
      - resourceName: "projects/{{ $.Values.secrets.projectId }}/secrets/{{ .name }}/versions/{{ .version | default "latest" }}"
        path: "{{ .path }}"
        {{- if .mode }}
        mode: {{ .mode }}
        {{- end }}
      {{- end }}
```

#### serviceaccount.yaml

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "my-app.serviceAccountName" . }}
  {{- if .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml .Values.serviceAccount.annotations | nindent 4 }}
  {{- end }}
```

#### deployment.yaml (relevant parts)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "my-app.fullname" . }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.replicaCount }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "my-app.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "my-app.selectorLabels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ include "my-app.serviceAccountName" . }}
      volumes:
        - name: secrets-store-inline
          csi:
            driver: secrets-store.csi.k8s.io
            readOnly: true
            volumeAttributes:
              secretProviderClass: {{ include "my-app.fullname" . }}-gcp-secrets
      containers:
        - name: {{ .Chart.Name }}
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          volumeMounts:
            - name: secrets-store-inline
              mountPath: "/mnt/secrets-store"
              readOnly: true
          env:
            - name: SECRET_PATH
              value: "/mnt/secrets-store/{{ index .Values.secrets.items 0 "path" }}"
```

#### values.yaml

```yaml
replicaCount: 1

image:
  repository: nginx
  pullPolicy: IfNotPresent
  tag: ""

serviceAccount:
  create: true
  annotations: {}
  name: ""

secrets:
  projectId: "my-gcp-project"
  wif:
    audience: "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/my-pool/providers/my-provider"
    # Optional values
    # tokenUrl: "https://sts.googleapis.com/v1/token"
    # credentialSource: "file:/var/run/secrets/tokens/gcp-ksa/token"
  items:
    - name: "app-secret"
      version: "latest"
      path: "app-secret.txt"
    - name: "db-password"
      version: "latest"
      path: "db-password.txt"
      mode: 0400
```

### Kustomize Example

For Kustomize, you can structure your configuration as follows:

#### Directory Structure

```
my-app/
├── base/
│   ├── kustomization.yaml
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── serviceaccount.yaml
│   └── secretproviderclass.yaml
└── overlays/
    ├── dev/
    │   ├── kustomization.yaml
    │   └── secrets-patch.yaml
    └── prod/
        ├── kustomization.yaml
        └── secrets-patch.yaml
```

#### base/secretproviderclass.yaml

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: my-app-gcp-secrets
spec:
  provider: gcp
  parameters:
    auth: "generic-wif"
    # These will be patched in the overlay
    wif.audience: "TO_BE_REPLACED"
    secrets: |
      - resourceName: "projects/PROJECT_ID/secrets/SECRET_NAME/versions/latest"
        path: "secret.txt"
```

#### base/deployment.yaml (relevant parts)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      serviceAccountName: my-app-sa
      volumes:
        - name: secrets-store-inline
          csi:
            driver: secrets-store.csi.k8s.io
            readOnly: true
            volumeAttributes:
              secretProviderClass: my-app-gcp-secrets
      containers:
        - name: my-app
          image: my-app:latest
          volumeMounts:
            - name: secrets-store-inline
              mountPath: "/mnt/secrets-store"
              readOnly: true
```

#### base/serviceaccount.yaml

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-app-sa
```

#### base/kustomization.yaml

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- deployment.yaml
- service.yaml
- serviceaccount.yaml
- secretproviderclass.yaml
```

#### overlays/dev/secrets-patch.yaml

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: my-app-gcp-secrets
spec:
  parameters:
    wif.audience: "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/dev-pool/providers/my-provider"
    secrets: |
      - resourceName: "projects/my-dev-project/secrets/app-secret/versions/latest"
        path: "app-secret.txt"
      - resourceName: "projects/my-dev-project/secrets/db-password/versions/latest"
        path: "db-password.txt"
```

#### overlays/dev/kustomization.yaml

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../../base

patchesStrategicMerge:
- secrets-patch.yaml

namePrefix: dev-
```

### Step-by-Step Implementation Guide

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
   gcloud iam workload-identity-pools providers create-oidc "my-provider" \
     --project="my-project" \
     --location="global" \
     --workload-identity-pool="my-pool" \
     --attribute-mapping="google.subject=assertion.sub,attribute.namespace=assertion.namespace" \
     --issuer-uri="https://kubernetes.default.svc.cluster.local"
   ```

3. **Grant Access to the GCP Secret**:
   ```bash
   # Create or get a GCP service account
   gcloud iam service-accounts create "my-gcp-sa" \
     --project="my-project" \
     --display-name="My GCP Service Account"

   # Grant Secret Manager access to the service account
   gcloud secrets add-iam-policy-binding "my-secret" \
     --project="my-project" \
     --member="serviceAccount:my-gcp-sa@my-project.iam.gserviceaccount.com" \
     --role="roles/secretmanager.secretAccessor"

   # Allow workload identity pool to impersonate the service account
   gcloud iam service-accounts add-iam-policy-binding "my-gcp-sa@my-project.iam.gserviceaccount.com" \
     --project="my-project" \
     --role="roles/iam.workloadIdentityUser" \
     --member="principalSet://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/my-pool/attribute.namespace/default"
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
       # If using service account impersonation, add this annotation
       iam.gke.io/gcp-service-account: my-gcp-sa@my-project.iam.gserviceaccount.com
   ```

5. **Deploy the Helm Chart**:
   ```bash
   # Update values.yaml with your specific settings
   helm install my-app ./my-app \
     --set secrets.projectId=my-project \
     --set secrets.wif.audience="//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/my-pool/providers/my-provider"
   ```

6. **Verify Secret Mounting**:
   ```bash
   # Get a shell into your pod
   kubectl exec -it deploy/my-app -- /bin/sh
   
   # Check if the secrets are properly mounted
   ls -la /mnt/secrets-store/
   cat /mnt/secrets-store/app-secret.txt
   ```

This comprehensive guide should help you implement Generic WIF authentication with Helm or Kustomize for your applications that need to access GCP Secret Manager. 