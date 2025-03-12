# Generic WIF Kustomize Example

This is a complete example showing how to use Generic Workload Identity Federation (WIF) with the GCP Secret Store CSI Driver Provider using Kustomize.

## Structure

The example follows a standard Kustomize base/overlay pattern:

```
kustomize/
├── base/                 # Base configuration
│   ├── kustomization.yaml
│   ├── deployment.yaml
│   ├── serviceaccount.yaml
│   └── secretproviderclass.yaml
└── overlays/             # Environment-specific overrides
    ├── dev/              # Development environment
    │   ├── kustomization.yaml
    │   ├── secretproviderclass-patch.yaml
    │   └── serviceaccount-patch.yaml
    └── prod/             # Production environment
        ├── kustomization.yaml
        ├── deployment-patch.yaml
        ├── secretproviderclass-patch.yaml
        └── serviceaccount-patch.yaml
```

## Prerequisites

Before using these examples, ensure you have:

1. A Kubernetes cluster with the Secret Store CSI Driver installed
2. The GCP Secret Store CSI Driver Provider installed
3. Workload Identity Federation configured in your GCP project
4. Secrets created in GCP Secret Manager

## Usage

### 1. Customize the configurations:

Edit the overlay files to match your specific environments:

- Update the `wif.audience` in each `secretproviderclass-patch.yaml` file
- Update the `resourceName` values to point to your actual GCP secrets
- If using service account impersonation, update the ServiceAccount annotations

### 2. Deploy to development:

```bash
kubectl apply -k overlays/dev/
```

### 3. Deploy to production:

```bash
kubectl apply -k overlays/prod/
```

## Key Configuration Points

### SecretProviderClass

The SecretProviderClass in each environment specifies:

- The authentication method (`auth: "generic-wif"`)
- The WIF audience string specific to each environment
- The GCP secrets to mount in each environment

### ServiceAccount

The ServiceAccount patches configure:

- Environment-specific annotations for service account impersonation
- These annotations allow pods to impersonate different GCP service accounts in each environment

### Deployment

The base deployment configures:

- A volume that uses the Secret Store CSI Driver
- A volume mount that makes secrets available at `/mnt/secrets-store`

The production overlay adds:

- Increased replica count 
- Increased resource limits for production

## Verifying Secret Access

To verify that your secrets are properly mounted:

```bash
# For dev environment
kubectl exec -it deploy/dev-generic-wif-app -- /bin/sh

# For prod environment
kubectl exec -it deploy/prod-generic-wif-app -- /bin/sh

# List the mounted secrets
ls -la /mnt/secrets-store/

# View a secret's contents
cat /mnt/secrets-store/app-config.json
```

## Troubleshooting

If you encounter issues with the secret mounting:

1. Check the pod logs
2. Check the GCP Secret Store CSI Driver Provider logs
3. Verify that the SecretProviderClass was created correctly
4. See the [Generic WIF documentation](../../generic-wif.md) for more detailed troubleshooting steps 