# Argo CD test schema

`application-crd.yaml` is copied unchanged from [Argo CD v3.3.0](https://github.com/argoproj/argo-cd/blob/v3.3.0/manifests/crds/application-crd.yaml), licensed under Apache-2.0. It is an envtest fixture, not part of controller installation. Use the Argo CD CRDs supplied with the cluster installation. This version pins schema validation; it does not assert that edlab runs v3.3.0.

Envtest runs no Argo CD controller. Tests explicitly simulate status updates and finalizer completion; Git access, rendering, sync, health assessment and cascading workload deletion require the manual cluster exercise.
