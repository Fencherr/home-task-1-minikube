# QOVES API Platform - Writeup

## Run It

### Prerequisites
- minikube v1.34+
- kubectl
- helm v3+
- Docker

### Stand up from scratch

```bash
# 1. Start cluster with Calico CNI (enforces NetworkPolicy)
minikube start --nodes 2 --cni calico --driver=docker
minikube addons enable ingress
minikube addons enable metrics-server

# 2. Build and load API image
docker build -t qoves-api:1.0.1 .
minikube image load qoves-api:1.0.1

# 3. Install platform infra
helm install sealed-secrets bitnami/sealed-secrets --namespace kube-system
helm install prometheus prometheus-community/kube-prometheus-stack \
  -n monitoring --create-namespace --set grafana.enabled=false
helm install argocd argo/argo-cd -n argocd --create-namespace

# 4. Bootstrap SealedSecret (db-credentials)
kubectl create secret generic db-credentials -n default-dev \
  --dry-run=client \
  --from-literal=DATABASE_URL="postgres://qoves:<PASSWORD>@postgres.default-dev.svc.cluster.local:5432/qovesdb?sslmode=disable" \
  -o json | kubeseal --format yaml > k8s/secrets/sealed-db-credentials.yaml
kubectl apply -f k8s/secrets/sealed-db-credentials.yaml

# 5. GitOps: apply root Application (manages everything)
kubectl apply -f argocd/root-app.yaml
```

### Repo layout

```
repo/
  app/                          # Go API source + go.mod + go.sum
  Dockerfile                    # Multi-stage build (Go -> alpine)
  helm/qoves-stack/             # Helm chart for the full stack
    Chart.yaml
    values.yaml
    templates/
      default-dev.yaml          # Namespace
      sealed-db-credentials.yaml # SealedSecret
      networkpolicies/          # 6 NetworkPolicies (default-deny, DNS, ingress, cross-ns)
      api/deployment.yaml
      api/service.yaml
      postgres/pvc.yaml
      postgres/service.yaml
      postgres/statefulset.yaml
      ingress/ingress.yaml
      hpa/hpa.yaml
      monitoring/servicemonitor.yaml
      monitoring/prometheusrule.yaml
  argocd/
    root-app.yaml               # App-of-apps root -> argocd/apps/
    apps/
      qoves-stack.yaml          # Child: Helm chart
      secrets.yaml              # Child: SealedSecret (Replace=true)
  k8s/secrets/                  # SealedSecret source of truth
  writeup.md
```

### Making a change (GitOps flow)

1. Edit the desired manifest in `helm/qoves-stack/templates/`
2. Commit and push to git
3. ArgoCD detects drift and reconciles the cluster
4. Verify via `kubectl get applications -n argocd`

## Decisions

### CNI: Calico
Decision: Calico via `minikube start --cni calico`
Alternatives: Cilium, Weave, Flannel (no policy enforcement)
Why: Calico is the most mature CNI with NetworkPolicy enforcement. minikube's default
CNI does not enforce policies - they silently succeed without blocking anything.
Cilium would also work with eBPF advantages, but Calico is simpler to debug.
On bare metal, Calico's BGP mode integrates naturally with existing network infrastructure.

### Secrets: Sealed Secrets
Decision: Bitnami Sealed Secrets with a separate ArgoCD child app using Replace=true
Alternatives: SOPS + ArgoCD plugin, External Secrets Operator
Why: Sealed Secrets is the simplest fully-local option - no external dependencies,
no sidecar containers, no plugin configuration. The controller runs in-cluster and
decrypts on apply. SOPS requires an ArgoCD plugin (complexity). ESO needs a running
secret store (overkill for minikube). The SealedSecret ciphertext is safely committed
to git. A separate child app with Replace=true avoids resource version conflicts with
the SealedSecrets controller. In production, ESO + Vault would be preferred for key
rotation and audit trails.

### Postgres: Raw StatefulSet vs Operator
Decision: Raw StatefulSet + PVC
Alternatives: CloudNativePG operator, Zalando Postgres Operator
Why: The raw StatefulSet is the minimal correct choice - persistent storage, stable
network identities, ordered pod management. An operator adds automated backups,
cloning, and failover (production requirements) but unnecessary complexity here.
The tradeoff is manual operational burden. In production, CloudNativePG would be
the right call for automated backup scheduling and point-in-time recovery.

### Scaling signal: CPU-based HPA
Decision: CPU utilization at 70% target
Alternatives: Memory, custom metrics (request rate), KEDA
Why: CPU-based HPA is the simplest starting point and catches traffic-driven CPU load.
However, for this API (which waits on database queries), CPU is not the ideal signal:
the API spends most time in network I/O. A better signal would be request latency or
request rate via Prometheus custom metrics. CPU is acceptable for v1; I would migrate
to request-based (KEDA) or latency-based scaling in production using the
`http_requests_total` metric exposed by the API.

## What minikube did for me

Minikube handles layers that need manual setup on bare metal:
- Control-plane bootstrap: kubeadm init, cert generation, etcd cluster, API server config
- CNI install: Calico deployed via `--cni calico` flag
- Ingress load-balancing: nginx-ingress-controller via addon; on bare metal, MetalLB
- Storage provisioner: Dynamic hostPath-based PV provisioning without config
- etcd: Single-node etcd bootstrapped with TLS; production needs 3-5 nodes
- Node management: Worker join tokens and cert approval handled automatically

## Production gaps

### High Availability
- Single-node etcd and single API server - control plane outage takes down the cluster
- Postgres: Single replica StatefulSet with no automated failover
- Ingress controller: Single replica, no pod anti-affinity

### Backups
- No automated database backup schedule
- No PVC snapshot or backup strategy
- etcd backups would need Velero

### Real secret backend
- Sealed Secrets lacks key rotation, audit logging, cloud KMS integration
- Production needs ESO + Vault/AWS Secrets Manager

### Upgrades
- No upgrade strategy for the cluster (minikube delete + start destroys data)
- No rollback strategy beyond git revert + ArgoCD sync

### Multi-cluster
- Single cluster, no DR region
- Would need Cluster API and cross-cluster service mesh

## Runbook: DB pod dies

Symptom: /healthz returns 503, API logs show connection refused

### Recovery
1. Verify: `kubectl get pods -n default-dev -l app=postgres`
2. Logs: `kubectl logs postgres-0 -n default-dev --tail=50`
3. If CrashLoopBackOff: Check PVC is bound, delete the pod (StatefulSet recreates)
4. If PVC/data lost: Restore from backup, or delete PVC and pod to reinitialize
5. If node is down: Force delete pod and PVC, StatefulSet recreates on healthy node
6. If the issue is a bad config change: Revert in git, push, ArgoCD auto-syncs
7. Verify: `kubectl exec -n default-dev postgres-0 -- psql -U qoves -d qovesdb -c "SELECT 1"`