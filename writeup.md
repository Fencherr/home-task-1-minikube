# QOVES API Platform - Writeup

## Run It

### Prerequisites
- minikube v1.34+
- kubectl
- helm v3+
- Docker

### Stand up from scratch

  ash
# 1. Start cluster with Calico
minikube start --nodes 2 --cni calico --driver=docker
minikube addons enable ingress
minikube addons enable metrics-server

# 2. Build and load API image
docker build -t qoves-api:1.0.1 .
minikube image load qoves-api:1.0.1

# 3. Install infrastructure
helm install sealed-secrets bitnami/sealed-secrets --namespace kube-system
helm install prometheus prometheus-community/kube-prometheus-stack -n monitoring --create-namespace --set grafana.enabled=false
helm install argocd argo/argo-cd -n argocd --create-namespace

# 4. Apply manifests
kubectl apply -f k8s/namespaces/
kubectl apply -f k8s/secrets/
kubectl apply -f k8s/postgres/
kubectl apply -f k8s/network-policies/
kubectl apply -f k8s/api/
kubectl apply -f k8s/ingress/
kubectl apply -f k8s/hpa/
kubectl apply -f k8s/monitoring/

# 5. GitOps: apply root app
kubectl apply -f argocd/root-app.yaml
  

### Repo layout
`
qoves-api-gitops/
  app/
  Dockerfile
  k8s/
    namespaces/
    secrets/
    postgres/
    api/
    network-policies/
    ingress/
    hpa/
    monitoring/
  argocd/
    root-app.yaml
    apps/
  writeup.md
`

### Making a change (GitOps flow)
1. Edit the desired manifest in k8s/
2. Commit and push to git
3. ArgoCD detects drift and reconciles
4. The cluster converges to the desired state

## Decisions

### CNI: Calico
Decision: Calico (installed via minikube start --cni calico)
Alternatives: Cilium, Weave, Flannel (no policy enforcement)
Why: Calico is the most mature CNI with NetworkPolicy enforcement. minikube's default CNI does not enforce policies. Cilium would also work with eBPF advantages, but Calico is simpler to debug for this scope. On real metal, Calico's BGP-based networking also integrates naturally with existing network infrastructure.

### Secrets: Sealed Secrets
Decision: Bitnami Sealed Secrets
Alternatives: SOPS + ArgoCD plugin, External Secrets Operator
Why: Sealed Secrets is the simplest fully-local option - no external dependencies, no sidecar containers, no plugin configuration. The controller runs in-cluster and decrypts on apply. SOPS would require configuring an ArgoCD plugin adding complexity. ESO requires a running secret store which is overkill for minikube. In production, ESO + Vault would be preferred for key rotation and audit trails.

### Postgres: Raw StatefulSet vs Operator
Decision: Raw StatefulSet + PVC
Alternatives: CloudNativePG operator, Zalando Postgres Operator
Why: The raw StatefulSet is the minimal correct choice - it gives persistent storage, stable network identities, and ordered pod management. A DB operator would add automated backups, cloning, and failover (production requirements) but unnecessary complexity here. In production, CloudNativePG would be the right call.

### Scaling signal: CPU-based HPA
Decision: CPU utilization at 70% target
Alternatives: Memory, custom metrics (request rate), KEDA
Why: CPU-based HPA is the simplest starting point. However, for this API (which primarily waits on database queries), CPU is not the ideal signal - the API spends most time in network I/O. A better signal would be request latency or request rate via Prometheus custom metrics. CPU is acceptable for a v1; I would migrate to request-based (KEDA) or latency-based scaling in production.

## What minikube did for me

Minikube handles several layers that would need manual setup on bare metal:
- Control-plane bootstrap: kubeadm init, certificate generation, etcd cluster setup, API server configuration
- CNI install: Calico deployment and configuration via --cni calico flag
- Ingress load-balancing: nginx-ingress-controller deployed by the addon; on bare metal you would need MetalLB
- Storage provisioner: Dynamic hostPath-based PV provisioning without configuration
- etcd: Single-node etcd bootstrapped automatically with TLS certs; production needs 3-5 node cluster
- Node management: Adding worker nodes handles join tokens and cert approval automatically

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

1. Verify the failure: kubectl get pods -n default-dev -l app=postgres
2. Check logs: kubectl logs postgres-0 -n default-dev --tail=50
3. If CrashLoopBackOff: Check PVC is bound, then delete the pod (StatefulSet recreates it)
4. If PVC/data lost: Restore from backup, or delete PVC and pod to reinitialize
5. If node is down: Force delete pod and PVC, StatefulSet recreates on healthy node
6. After recovery: Verify DB connectivity and API health
7. If using GitOps, commit manifest changes and ArgoCD syncs automatically
