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
variables, and pod checksum that rolls pods when configuration changes. This PoC
branch always enables installer-role setup and discovery. There is no per-environment
toggle; every deployment must supply the installer-role configuration.

The rosa-seaplane deployment configuration must supply these values from its
environment identity and Terraform outputs:

```yaml
global:
  aws_region: us-east-1 # API/discovery region; independent of the table region
platformApi:
  installerRole:
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

The binary requires
`INSTALLER_ROLE_CONFIG_FILE=/etc/platform-api/installer-role/config.yaml` or
`--installer-role-config-file`. An explicit flag overrides the environment value.
Startup fails before opening database connections or listeners if configuration is
missing or invalid. The dedicated DynamoDB client uses workload credentials and the
table region; STS and customer EC2 clients retain the API region.

The four PoC routes are available to gateway-authenticated AWS callers without
prior account enrollment, Cedar permissions, Red Hat login, or organization mapping:

| Method | Route | Behavior |
| --- | --- | --- |
| POST | `/api/v0/aws-account-access` | Initialize the owning account's assignment with `prefix` and `path`; 201 created, 200 unchanged, 409 conflicting selection. |
| GET | `/api/v0/installer-role-config` | Read approved metadata, trust, shared tags, and concurrency tokens; allocates nothing. |
| PUT | `/api/v0/aws-account-access` | Supply `assignmentId`, `resourceVersion`, and `configurationRevision` from GET; validate AWS trust/access and mark Ready. |
| GET | `/api/v0/cloud_discovery/vpcs` | List all customer VPCs in the API region through a Ready assignment and both role hops. |

Requests cannot override the customer account, environment, role ARN, ExternalId,
or region, and no query parameters are accepted. ExternalId is always the verified
owning AWS account ID. Existing resource authorization remains in place. These
handlers rely on the private backend's gateway-only ingress and the gateway's
SigV4 verification and identity-header replacement; headers alone do not authenticate
an otherwise public backend.

The shared DynamoDB table stores assignments under
`pk=ENV#<environment>#ACCOUNT#<account>`, `sk=INSTALLER`. Initialization and state
updates are conditional, and reads are strongly consistent. Repeating setup keeps
the assignment ID; every state update replaces its opaque `resourceVersion` UUID.
The wire field is `resourceVersion`, corresponding to the plan's assignment revision.
Assignments contain no credentials or placeholder organization ownership.

Completion freshly assumes the jump and installer roles, checks IAM role identity,
shared `red-hat-managed=true` and `seaplane-access=true` tags, managed policy attachment,
and explicit ExternalId-protected trust. Compatible explicit role principals for
other environments are allowed. Wildcard/account-wide principals, ambiguous JSON,
unsupported trust shapes, and ExternalId bypasses are rejected. Negative assumption
checks require an actual AWS authorization denial; throttling/timeouts are
inconclusive. Failed revalidation conditionally returns a previously Ready record
to Pending without overwriting newer results. Discovery requires validation against
the current configuration. Ready denotes AWS validation, not verified Red Hat
organization ownership.

Run the unit, fake AWS, HTTP workflow, and chart tests from `seaplane-api` with
`GOWORK=off go test -race ./...`. DynamoDB Local persistence/concurrency tests run
with `GOWORK=off go test -race -tags integration ./pkg/accountaccess`. They use
`ACCOUNT_ACCESS_DYNAMODB_ENDPOINT` if set, otherwise start a disposable local Podman
container. No real AWS credentials are used by these tests.

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
