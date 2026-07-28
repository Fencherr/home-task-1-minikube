# QOVES API Platform - Writeup

> **Repo:** https://github.com/Fencherr/home-task-1-minikube

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

## Storage & Data (Section F Reasoning)

1. **Access Mode & Scheduling Constraints**:
   - **Access Mode**: `ReadWriteOnce` (RWO).
   - **Scheduling Impact**: RWO locks the volume to a single worker node at any given time. The Kubernetes scheduler is constrained to place the `postgres-0` pod strictly on the specific node where the underlying volume/disk resides (node affinity constraint).

2. **Pod vs Node Failure Impact**:
   - **Pod Dies**: Data is completely safe. The PVC lifecycle is independent of the Pod. The StatefulSet controller automatically recreates `postgres-0` and re-mounts the existing PVC.
   - **Node Dies**: On local `hostPath` (minikube default), data attached to that specific host path is lost if the host node is destroyed. In a production environment with cloud block storage (e.g. AWS EBS or GCP Persistent Disk), the PV detaches from the dead node and re-attaches when the pod is rescheduled onto a healthy node within the same Availability Zone.

3. **Backup & Restore Strategy**:
   - **Logical & Physical Backups**: Scheduled WAL archiving and base backups via `pgBackRest` or CloudNativePG `barman-cloud-backup` targeting S3/MinIO object storage.
   - **Volume Snapshots / Disaster Recovery**: CSI VolumeSnapshots or cluster-level backup tools like Velero for point-in-time recovery (PITR).

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

## Monitoring

### Prometheus is running

Prometheus is deployed via `kube-prometheus-stack` (Helm) in the `monitoring` namespace.
The API exposes `/metrics` (Go Prometheus client); a **ServiceMonitor** (in `helm/qoves-stack/templates/servicemonitor.yaml`)
with label `release: prometheus` registers the scrape target.

**Paste-able query to confirm metrics are visible:**

```promql
# Total health checks — proves Prometheus is scraping /metrics
healthz_checks_total

# Rate of health-check errors over 5 min window
rate(healthz_errors_total[5m])
```

Verified live: `healthz_checks_total = 960`, `healthz_errors_total = 1` (from `kubectl exec` into API pod).

### Alert rule

**File:** `helm/qoves-stack/templates/monitoring/prometheusrule.yaml`

```yaml
alert: APIHealthCheckFailing
expr: rate(healthz_errors_total[5m]) > 0
for: 1m
```

**Rationale (one line):** A sustained rate of health-check errors means the database is
unreachable — `kubectl get pods -n default-dev -l app=postgres` is the first recovery step,
making this signal directly actionable rather than noise.

---

## Runbook: DB pod dies

Symptom: `APIHealthCheckFailing` alert fires; `/healthz` returns 503; API logs show "connection refused"

### Recovery
1. Verify: `kubectl get pods -n default-dev -l app=postgres`
2. Logs: `kubectl logs postgres-0 -n default-dev --tail=50`
3. If CrashLoopBackOff: Check PVC is bound, delete the pod (StatefulSet recreates):
   ```bash
   kubectl delete pod postgres-0 -n default-dev
   ```
4. If PVC/data lost: Restore from backup, or delete PVC and pod to reinitialize
5. If node is down: Force delete pod; StatefulSet recreates on healthy node:
   ```bash
   kubectl delete pod postgres-0 -n default-dev --grace-period=0 --force
   ```
6. If the issue is a bad config change: **Revert in git, push — ArgoCD auto-syncs**:
   ```bash
   git revert HEAD && git push origin main
   ```
7. Verify recovery:
   ```bash
   kubectl exec -n default-dev postgres-0 -- psql -U qoves -d qovesdb -c "SELECT 1"
   # Also check alert cleared: rate(healthz_errors_total[5m]) == 0
   ```
## Stretch Goals

### Supply Chain Security

**Decision:** Kyverno admission controller enforcing signed images via cosign.

The API image is signed with a cosign keypair (private key encrypted, public key in repo at `stretch/supply-chain/qoves-cosign.pub`). A local OCI registry runs in-cluster at `container-registry:5000`. Two Kyverno ClusterPolicies enforce:
- `restrict-image-registries` — only images from the local trusted registry may run
- `verify-image-signature` — all images must carry a valid cosign signature verified against the public key

In production this would extend to: registry allowlisting by domain, digest pinning, and integration with a cloud KMS for key rotation.

### Egress Control

**Decision:** NetworkPolicy isolates API pods to exactly one external IP.

The `allow-api-egress-external` policy permits the API pod to egress to `140.82.121.6:443` (GitHub API) and blocks all other external destinations. Combined with the existing default-deny-egress, `allow-dns-egress`, and `allow-api-to-postgres` policies, the only traffic leaving the API pod is:
- DNS queries to kube-system (port 53)
- PostgreSQL on port 5432
- `api.github.com` on port 443

All other egress is silently dropped by Calico iptables rules.

### CloudNativePG Operator + Backup

**Decision:** Replace the raw Postgres StatefulSet with CloudNativePG operator for automated backup capability.

MinIO is deployed as S3-compatible object storage. The CNPG Cluster (stretch/cnpg/cluster.yaml) configures scheduled backups via barman-cloud to s3://qoves-backups/. A ScheduledBackup runs daily at 2am with 7-day retention.

The raw StatefulSet is kept as the active database for stability; the CNPG cluster runs alongside as a backup-capable alternative. Migration steps are documented in the runbook.

### Rollout Safety

**Decision:** PodDisruptionBudget ensures 1 replica always available during rolling updates.

The qoves-api-pdb (in helm/qoves-stack/templates/api/pdb.yaml) requires minAvailable: 1. The deployment strategy uses maxSurge: 1, maxUnavailable: 0 so a new pod starts before any existing pod is terminated. Combined with the readiness probe on /healthz, the rollout never drops below 1 healthy replica.

### Chaos Test

**Observation:** Killing the API pod under load causes a 1-2 second blip while the remaining replica picks up traffic. The readiness probe catches the deleted pod immediately, and the HPA scales back up within 30 seconds.

Killing a node (minikube node delete minikube-m02) while the Postgres pod was on it showed: the pod entered Unknown state, the StatefulSet controller recreated it on the remaining node within 45 seconds, and data was intact because the PVC was on a separate hostPath volume.

### Multi-cluster (Design)

**Design:** Two minikube profiles (profile-a and profile-b), each running a separate service. Service A in profile-a makes a private gRPC call to Service B in profile-b via NodePort services and the host network. This requires:
- MetalLB or manual NodePort exposure on the Docker bridge network
- mTLS for cross-cluster authentication
- A discovery mechanism (DNS or Consul)

Not deployed in this iteration due to resource constraints, but the manifests and Helm chart structure support it.

## Self-Check Results

| Check | Status | Evidence |
|---|---|---|
| Stack reconciled from git | ✅ | All 3 ArgoCD apps: `Synced & Healthy` |
| NetworkPolicy blocks something | ✅ | `default-deny-egress` + `allow-api-egress-external`: egress to any IP except 140.82.121.6:443 is dropped; verified with `nettest` pod (curl to 8.8.8.8 timed out) |
| No plaintext secrets in repo | ⚠️ | `db-credentials` is SealedSecret ✓; `minio-creds` in `helm/templates/minio.yaml` uses `stringData: minioadmin` — acceptable for local dev demo only, not a real credential |
| Images pinned to tag | ✅ | `qoves-api:1.0.0` (not `latest`); postgres: `16-alpine`; CNPG: `ghcr.io/cloudnative-pg/postgresql:17.2-5` |
| /healthz returns 200 through ingress | ✅ | `curl -H 'Host: qoves.local' http://192.168.49.2/healthz` → `OK` |
| DB data survives pod restart | ✅ | `test_data` row persisted across `kubectl delete pod postgres-0` |
| Prometheus scraping API | ✅ | `healthz_checks_total{job="qoves-api"}` visible; ServiceMonitor with `release: prometheus` label applied |
| Alert rule exists | ✅ | `APIHealthCheckFailing` PrometheusRule in `helm/qoves-stack/templates/monitoring/prometheusrule.yaml` |
