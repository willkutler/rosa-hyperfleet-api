# platform-api Helm Chart

Helm chart for the ROSA Seaplane API.

## Overview

This chart deploys:

- Platform API application with API Gateway IAM authentication, Cedar authorization, and rate limiting
- Service exposing ports 8000 (API), 8081 (health), and 9090 (metrics)
- TargetGroupBinding for AWS Application Load Balancer integration

## Prerequisites

- Kubernetes cluster (EKS recommended)
- AWS Load Balancer Controller installed
- Target Group ARN for the Application Load Balancer

## Configuration

See [values.yaml](values.yaml) for all configuration options. Key settings:

```yaml
platformApi:
  namespace: platform-api

  app:
    name: platform-api
    image:
      repository: quay.io/cdoan0/rosa-seaplane-api
      tag: nodb

  targetGroup:
    arn: "PLACEHOLDER" # AWS Target Group ARN
    targetType: ip
```

## Installation

### Installer-role PoC configuration

The API chart owns the installer-role ConfigMap, read-only mount, environment
variables, and pod checksum that rolls pods when configuration changes. The feature
is disabled by default and should be enabled only in the isolated PoC environment.

The rosa-seaplane deployment configuration must supply these values from its
environment identity and Terraform outputs:

```yaml
global:
  aws_region: us-east-1 # API/discovery region; independent of the table region
platformApi:
  installerRole:
    enabled: true
    environmentId: ephemeral-example-123
    jumpRoleArn: arn:aws:iam::123456789012:role/ephemeral-example-123-jump
    accountAccess:
      tableArn: arn:aws:dynamodb:us-west-2:123456789012:table/example-account-access
      tableRegion: us-west-2
```

All regional APIs in one environment must share the environment ID and table
location. Separate ephemeral deployments need distinct environment IDs. The table
may reside in another account; workload IAM and any table resource policy must
permit access to its ARN. The chart does not create the table, workload identity,
or jump role.

The chart defaults `formatVersion` to `1`, `trustTemplateVersion` to `"1"`, and
`installerManagedPolicyArn` to
`arn:aws:iam::aws:policy/service-role/ROSAInstallerPolicy`. These are the only
supported versions and policy for this PoC; jump-role and table ARNs must use the
commercial `aws` partition. Environment IDs use 1–128 letters, digits, underscores,
or hyphens and start with a letter or digit.

The binary accepts `INSTALLER_ROLE_ENABLED=true` and
`INSTALLER_ROLE_CONFIG_FILE=/etc/platform-api/installer-role/config.yaml`, or
`--installer-role-enabled` and `--installer-role-config-file`. Explicit flags
override environment values. Enabling requires a valid file; startup fails before
opening database connections or listeners if configuration is missing or invalid.
Disabled startup ignores the file. The dedicated DynamoDB client uses workload
credentials and the table region; the API/discovery AWS configuration is preserved.

This step supplies configuration and client construction. Account-association
storage and installer-role HTTP routes are implemented in later plan steps.

After these changes are committed and the API image is built, rosa-seaplane must
pin the chart revision and API image containing them.

### Basic Installation

```bash
helm install platform-api ./platform-api/charts
```

### Production Installation with Custom Values

```bash
helm install platform-api ./platform-api/charts \
  --set platformApi.targetGroup.arn="arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"
```

### Using a Custom Values File

Create a `custom-values.yaml`:

```yaml
platformApi:
  app:
    image:
      tag: "v1.2.3"
    args:
      logLevel: debug

  targetGroup:
    arn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"

  deployment:
    replicas: 3

  app:
    resources:
      requests:
        cpu: 200m
        memory: 256Mi
      limits:
        cpu: 1000m
        memory: 1Gi
```

Install with custom values:

```bash
helm install platform-api ./platform-api/charts \
  -f custom-values.yaml
```

## Upgrading

```bash
helm upgrade platform-api ./platform-api/charts \
  --set platformApi.targetGroup.arn="arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"
```

## Uninstallation

```bash
helm uninstall platform-api
```

To also delete the namespace:

```bash
kubectl delete namespace platform-api
```

## Parameters

### Application Configuration

| Parameter                          | Description                          | Default                            |
| ---------------------------------- | ------------------------------------ | ---------------------------------- |
| `platformApi.namespace`            | Namespace to deploy into             | `platform-api`                     |
| `platformApi.app.name`             | Application name                     | `platform-api`                     |
| `platformApi.app.image.repository` | Container image repository           | `quay.io/cdoan0/rosa-seaplane-api` |
| `platformApi.app.image.tag`        | Container image tag                  | `nodb`                             |
| `platformApi.app.args.logLevel`    | Log level (debug, info, warn, error) | `info`                             |
| `platformApi.deployment.replicas`  | Number of replicas                   | `1`                                |

### Target Group Configuration

| Parameter                            | Description                  | Default         |
| ------------------------------------ | ---------------------------- | --------------- |
| `platformApi.targetGroup.arn`        | AWS Target Group ARN         | `"PLACEHOLDER"` |
| `platformApi.targetGroup.targetType` | Target type (ip or instance) | `ip`            |

## Architecture

```
┌─────────────────────────────────────────┐
│   Application Load Balancer (ALB)      │
└────────────────┬────────────────────────┘
                 │ :8000
                 │
┌────────────────▼────────────────────────┐
│           Platform API :8000            │
└────────────────┬────────────────────────┘
                 │
     ┌───────────┼───────────┐
     │           │           │
     ▼           ▼           ▼
   :8000       :8081       :9090
    API       Health      Metrics
```

## Health Checks

The application exposes health endpoints on port 8081:

- `/healthz` - Liveness probe
- `/readyz` - Readiness probe

Kubernetes probes check these endpoints directly.

## API Endpoints

All API endpoints require the `X-Amz-Account-Id` header with an allowed AWS account ID:

```bash
curl -s http://localhost:8000/api/v0/management_clusters \
  -H "X-Amz-Account-Id: 123456789012"
```

## Troubleshooting

### Check pod status

```bash
kubectl get pods -n platform-api
kubectl describe pod -n platform-api <pod-name>
```

### View logs

```bash
# Application logs
kubectl logs -n platform-api <pod-name> -c platform-api

```

### Check TargetGroupBinding

```bash
kubectl get targetgroupbinding -n platform-api
kubectl describe targetgroupbinding -n platform-api platform-api
```

### Test health endpoints

```bash
# Port-forward to test locally
kubectl port-forward -n platform-api svc/platform-api 8000:8000

curl http://localhost:8000/api/v0/live
```
