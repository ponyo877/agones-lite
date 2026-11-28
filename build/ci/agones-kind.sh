#!/usr/bin/env bash
# Makes a kind cluster that runs the Agones of third_party/agones, as its
# install.yaml puts it, for agones-diff.sh to compare scrim with. The one
# change is that the allocator answers without TLS: the comparison is of
# what it answers, and scrim's own TLS is tried elsewhere.
#
# Usage: build/ci/agones-kind.sh
set -euo pipefail

# A Kubernetes of the minor version Agones tests its release with
# (build/includes/kind.mk).
kind create cluster --name agones --image kindest/node:v1.35.5 --wait 5m
kubectl create namespace agones-system
kubectl apply --server-side -f third_party/agones/install/yaml/install.yaml
kubectl -n agones-system set env deployment/agones-allocator DISABLE_MTLS=true DISABLE_TLS=true
for deployment in agones-controller agones-extensions agones-allocator; do
	kubectl -n agones-system rollout status "deployment/$deployment" --timeout 5m
done
