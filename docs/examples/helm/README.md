# Generic WIF Helm Chart Example

This is a complete example Helm chart demonstrating how to use Generic Workload Identity Federation (WIF) with the GCP Secret Store CSI Driver Provider.

## Prerequisites

Before using this chart, ensure you have:

1. A Kubernetes cluster with the Secret Store CSI Driver installed
2. The GCP Secret Store CSI Driver Provider installed
3. Workload Identity Federation configured in your GCP project
4. Secrets created in GCP Secret Manager

## Installing the Chart

1. Update the `values.yaml` file with your specific configuration:
   - Set your `projectId` to your GCP project
   - Configure your WIF `audience` string
   - Update the `secrets.items` list with your actual secrets
   - If using service account impersonation, uncomment and set the `serviceAccount.annotations` section

2. Install the chart:
   ```bash
   helm install my-wif-app ./generic-wif-example
   ```

3. To override values on the command line:
   ```bash
   helm install my-wif-app ./generic-wif-example \
     --set secrets.projectId=my-real-project \
     --set secrets.wif.audience="//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/my-pool/providers/my-provider"
   ```

## Configuration Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas | `1` |
| `serviceAccount.create` | Whether to create a ServiceAccount | `true` |
| `serviceAccount.name` | Name of the ServiceAccount | `generic-wif-app-sa` |
| `serviceAccount.annotations` | Annotations for the ServiceAccount | `{}` |
| `secrets.projectId` | GCP project ID | `my-gcp-project` |
| `secrets.wif.audience` | WIF audience string | `//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/my-pool/providers/my-provider` |
| `secrets.wif.tokenUrl` | Optional: Override the token URL | `nil` |
| `secrets.items` | List of secrets to mount | See `values.yaml` |

## Verifying Secret Access

To verify that your secrets are properly mounted:

```bash
# Get a shell to your pod
kubectl exec -it deploy/my-wif-app -- /bin/sh

# List the mounted secrets
ls -la /mnt/secrets-store/

# View a secret's contents
cat /mnt/secrets-store/app-config.json
```

## Debugging

If you encounter issues with the secret mounting:

1. Check the pod logs:
   ```bash
   kubectl logs deploy/my-wif-app
   ```

2. Check the GCP Secret Store CSI Driver Provider logs:
   ```bash
   kubectl logs -n kube-system -l app=csi-secrets-store-provider-gcp
   ```

3. Verify that the SecretProviderClass was created correctly:
   ```bash
   kubectl get secretproviderclasses
   kubectl describe secretproviderclass my-wif-app-gcp-secrets
   ```

4. See the [Generic WIF documentation](../../generic-wif.md) for more detailed troubleshooting steps. 