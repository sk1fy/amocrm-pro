# Architecture decision records

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](0001-postgresql-without-redis.md) | Accepted | PostgreSQL without Redis |
| [0002](0002-two-go-binaries-one-module.md) | Accepted | Two runtime binaries in one Go module |
| [0003](0003-docker-only-runtime-and-tooling.md) | Accepted | Docker-only runtime and tooling |
| [0004](0004-widget-browser-and-cleanup-contract.md) | Accepted, implementation gap | Direct widget browser access and bounded cleanup |
| [0005](0005-webhook-workflow-effect-correlation.md) | Accepted | Webhook workflow effect correlation |
| [0006](0006-webhook-payload-retention-and-metrics.md) | Accepted | Raw webhook payload retention and metrics |
| [0007](0007-async-rule-management-principal-and-cas.md) | Accepted | Async rule management principal and CAS |
| [0008](0008-multi-widget-capability-boundary.md) | Accepted | Multi-widget provisioning and capability boundary |
| [0009](0009-service-modules-and-fair-admission.md) | Accepted | Service modules, widget limits and fair queue admission |

ADR records a decision and its consequences. Runtime implementation gaps belong
in [`../project-memory/BUGS.md`](../project-memory/BUGS.md); remaining product
scope belongs in GitHub Issues.
