# Manual cluster smoke test

Run against a disposable development cluster with a NetworkPolicy-capable CNI. These commands mutate the selected cluster. The seven checks were reported successful on edlab on 2026-10-03 with image `790e0e6`; this saved procedure is not an automated test or a scale test.

## 1. Publish and deploy

```sh
kubectl config current-context
PREVIEW_HASH=$(git rev-parse --short HEAD)
PREVIEW_IMAGE=container-registry.lab.edlab.dev/edlab/preview-environment-controller/manager:$PREVIEW_HASH
podman build --platform linux/amd64 --build-arg TARGETARCH=amd64 \
  --build-arg VERSION=0.1.0 --build-arg GIT_HASH="$PREVIEW_HASH" -t "$PREVIEW_IMAGE" .
podman push "$PREVIEW_IMAGE"
make deploy IMG="$PREVIEW_IMAGE"
kubectl -n preview-environment-controller-system rollout status \
  deployment/preview-environment-controller-controller-manager --timeout=120s
```

The platform targets the **cluster nodes**, not the macOS workstation. Use `linux/arm64` / `TARGETARCH=arm64` for an arm64 cluster. `make deploy` updates `config/manager/kustomization.yaml`; inspect that image-pin change before committing.

## 2. Create a preview

```sh
kubectl apply -f - <<'YAML'
apiVersion: platform.ellery.dev/v1alpha1
kind: PreviewEnvironment
metadata:
  name: smoke
  namespace: preview-environment-controller-system
spec:
  namespace: preview-smoke
YAML
kubectl -n preview-environment-controller-system wait \
  --for=condition=Ready previewenvironment/smoke --timeout=120s
kubectl -n preview-smoke get resourcequota,limitrange,networkpolicy,serviceaccount
kubectl -n preview-environment-controller-system get previewenvironment smoke -o yaml
```

Expect all four baseline resources and Ready=True / BaselineProvisioned with matching observedGeneration. This baseline-only preview does not test application health.

## 3. Verify networking

```sh
kubectl -n preview-smoke run server --image=nginx:alpine \
  --overrides='{"spec":{"serviceAccountName":"preview-workload"}}'
kubectl -n preview-smoke run client --image=busybox:1.37 \
  --overrides='{"spec":{"serviceAccountName":"preview-workload"}}' --command -- sleep 3600
kubectl create namespace smoke-outsider
kubectl -n smoke-outsider run server --image=nginx:alpine
kubectl -n smoke-outsider run client --image=busybox:1.37 --command -- sleep 3600
kubectl -n preview-smoke wait --for=condition=Ready pod/server pod/client --timeout=120s
kubectl -n smoke-outsider wait --for=condition=Ready pod/server pod/client --timeout=120s
PREVIEW_SERVER_IP=$(kubectl -n preview-smoke get pod server -o jsonpath='{.status.podIP}')
OUTSIDE_SERVER_IP=$(kubectl -n smoke-outsider get pod server -o jsonpath='{.status.podIP}')
kubectl -n preview-smoke exec client -- wget -T 5 -qO- "http://$PREVIEW_SERVER_IP"
kubectl -n preview-smoke exec client -- nslookup kubernetes.default.svc.cluster.local
kubectl -n smoke-outsider exec client -- wget -T 5 -qO- "http://$PREVIEW_SERVER_IP"
kubectl -n preview-smoke exec client -- wget -T 5 -qO- "http://$OUTSIDE_SERVER_IP"
```

The first HTTP request and DNS lookup must succeed. Both cross-namespace HTTP requests must time out and exit nonzero. Successful cross-namespace access fails the test. Check CNI enforcement and other additive NetworkPolicies. NodeLocal DNS requires a different DNS egress rule; the current policy expects kube-system DNS pods labeled `k8s-app=kube-dns`.

## 4. Verify defaults and workload identity

```sh
kubectl -n preview-smoke get pod client -o jsonpath='{.spec.containers[0].resources}{"\n"}'
kubectl -n preview-smoke exec client -- sh -c \
  'test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token'
kubectl auth can-i list secrets -n preview-smoke \
  --as=system:serviceaccount:preview-smoke:preview-workload
```

Expect requests 100m CPU / 128Mi, limits 500m / 256Mi, an absent token, and `no` for Secret access. The impersonation check requires your kubectl identity to have impersonation permission; an impersonation-forbidden error does not prove the workload permissions.

## 5. Verify quota admission

```sh
kubectl -n preview-smoke apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: quota-check
spec:
  replicas: 11
  selector:
    matchLabels:
      app: quota-check
  template:
    metadata:
      labels:
        app: quota-check
    spec:
      serviceAccountName: preview-workload
      containers:
      - name: sleeper
        image: busybox:1.37
        command: [sleep, "3600"]
        resources:
          requests: {cpu: 100m, memory: 32Mi}
          limits: {cpu: 100m, memory: 32Mi}
YAML
kubectl -n preview-smoke get resourcequota preview-baseline -o yaml
kubectl -n preview-smoke get pods
kubectl -n preview-smoke describe replicaset -l app=quota-check
kubectl -n preview-smoke delete deployment quota-check
```

Wait for quota accounting and ReplicaSet retries. The ReplicaSet must report `FailedCreate` with `exceeded quota`; fewer than 11 quota-check pods may be admitted. Pod scheduling or image errors alone do not prove quota enforcement.

## 6. Verify drift repair and recreation

```sh
kubectl -n preview-smoke patch resourcequota preview-baseline \
  --type=merge -p '{"spec":{"hard":{"pods":"100"}}}'
kubectl -n preview-smoke get resourcequota preview-baseline -w
# Once spec.hard.pods is 10, stop the watch with Ctrl-C.
kubectl -n preview-smoke delete networkpolicy preview-baseline
kubectl -n preview-smoke get networkpolicy preview-baseline -w
# Once it reappears, stop the watch with Ctrl-C.
kubectl -n preview-environment-controller-system logs \
  deployment/preview-environment-controller-controller-manager --since=5m
```

Expect the pod quota to return to 10 and the policy to be recreated. Transient optimistic-lock conflicts can occur while quota accounting or another reconcile writes. Verify eventual desired state and Ready rather than treating one conflict log as permanent failure. If repair stalls, inspect Conditions and repeated errors.

## 7. Verify cleanup

```sh
kubectl -n preview-environment-controller-system delete previewenvironment smoke --wait=false
kubectl wait --for=delete namespace/preview-smoke --timeout=180s
kubectl -n preview-environment-controller-system wait \
  --for=delete previewenvironment/smoke --timeout=180s
kubectl delete namespace smoke-outsider --wait=true --timeout=180s
```

All preview children must disappear through real namespace cleanup. Do not manually remove finalizers to pass the test. If deletion stalls, inspect preview and namespace Conditions/finalizers. Record results, image tag, cluster version and unexpected behavior in `docs/learning.md`.
