# Shared protocol

Shared Go types and contracts used by the Controller and Agent. Build the product from the repository root using the [packaging guide](../docs/deployment-and-rollback.md); deployment starts at the [main README](../README.md).

Runtime paths are registered on the target node. Remote operations reference a trusted installation and typed operation; they do not grant arbitrary shell or filesystem access. Public HTTP contracts are documented in [OpenAPI](../docs/openapi-v2.yaml).
