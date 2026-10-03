# Preview lifetime

Set optional `spec.ttl` to a positive duration using whole hours, minutes and seconds in that order, for example `24h`, `90m`, `1h30m` or `30s`. Fractions, days, milliseconds, negative/zero durations and values beyond Go's duration range are rejected. Omitted TTL means no automatic expiry. Schema validation rejects invalid API requests; defensive runtime validation reports `Ready=False / InvalidTTL` for invalid objects encountered by the reconciler.

The deadline is `metadata.creationTimestamp + spec.ttl`, measured at second precision, and is reported as `status.expiresAt`. It includes provisioning and failure time. The controller schedules a queue wake-up at that deadline on every successful reconciliation, even for ownership conflicts or pending Applications. An earlier terminating-resource retry is preserved. API errors use controller-runtime backoff. Spec changes and manager restart recalculate the deadline; TTL edits do not reset the creation time. Removing TTL clears expiresAt and prevents expiry. An already-queued wake-up after extending/removing TTL is harmless because it rereads current spec.

At expiry the controller records `Ready=False / TTLExpired` and requests deletion of the PreviewEnvironment with UID/resourceVersion preconditions. Concurrent changes invalidate that stale decision and retry. It does not provision new resources once expired. Normal deletion events drive the existing cleanup finalizer: Application and its Argo cascading cleanup first, then namespace disappearance, then release of the controller's finalizer. Other finalizers remain intact. Cleanup Conditions replace TTLExpired as deletion progresses. A failed delete sets `Ready=False / TTLDeletionFailed` with the API error and returns an error for retry; expiry is a deletion request deadline, not a guarantee that every resource is gone at that instant. Argo outages or foreign-resource conflicts can keep cleanup blocked. Once deletionTimestamp is set, a TTL edit cannot restore the preview.

## Manual smoke test

These TTL checks have not been run in a real cluster. Deploy the new image/CRD/RBAC together using the baseline or Argo overlay described in the existing guides; the controller now requires delete permission on PreviewEnvironments. For a baseline preview:

```sh
kubectl -n preview-environment-controller-system apply -f - <<'YAML'
apiVersion: platform.ellery.dev/v1alpha1
kind: PreviewEnvironment
metadata:
  name: ttl-smoke
spec:
  namespace: preview-ttl-smoke
  ttl: 2m
YAML
kubectl -n preview-environment-controller-system get previewenvironment ttl-smoke -o yaml
kubectl -n preview-environment-controller-system wait --for=delete previewenvironment/ttl-smoke --timeout=5m
kubectl get namespace preview-ttl-smoke
```

Expect expiresAt and eventual deletion of both CR and namespace without manually clearing finalizers. On separate previews, extend TTL and verify the original deadline passes without deletion; remove TTL and verify expiresAt disappears; shorten TTL below the preview's age and verify cleanup starts. Add TTL to the Application sample to verify Argo Application/workload deletion precedes namespace deletion. Record image tag, timestamp and observed cleanup results in the learning journal. Avoid reusing the same namespace while a previous preview is deleting.

Unit tests cover duration parsing, missing timestamps, invalid runtime configuration, scheduling, deadline edits/removal, zero-write idempotency, conflict expiry, earlier retries, delete failure and retained finalizers. Envtest checks CRD validation and automatic queue-driven expiration after a TTL edit using the real manager/API server; no manual Reconcile call drives expiry. It deliberately leaves namespace deletion waiting because envtest has no namespace controller. Existing Argo tests cover the shared Application-before-namespace deletion path. This does not demonstrate real-cluster cleanup latency or scale.
