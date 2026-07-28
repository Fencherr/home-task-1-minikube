#!/usr/bin/env bash
set -e

echo "=== 1. Starting Minikube with Calico CNI ==="
minikube start --nodes 2 --cni calico --driver=docker
minikube addons enable ingress
minikube addons enable metrics-server

echo "=== 2. Building & Loading API Image ==="
docker build -t qoves-api:1.0.0 .
minikube image load qoves-api:1.0.0

echo "=== 3. Adding Helm Repositories & Installing Controllers ==="
helm repo add bitnami https://charts.bitnami.com/bitnami
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo add argo https://argoproj.github.io/argo-helm
helm repo update

helm install sealed-secrets bitnami/sealed-secrets --namespace kube-system 2>/dev/null || true
helm install prometheus prometheus-community/kube-prometheus-stack -n monitoring --create-namespace --set grafana.enabled=false 2>/dev/null || true
helm install argocd argo/argo-cd -n argocd --create-namespace 2>/dev/null || true

echo "=== 4. Applying SealedSecrets & Bootstrapping ArgoCD ==="
kubectl apply -f k8s/secrets/sealed-db-credentials.yaml
kubectl apply -f argocd/root-app.yaml

echo "=== Complete! Checking Applications ==="
kubectl get applications -n argocd
