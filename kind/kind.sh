#!/bin/bash

# Exit on any error
set -e

CLUSTER_NAME="step-job-kind-cluster"

# Check if kind is installed
if ! command -v kind &> /dev/null; then
    echo "Kind is not installed. Please install it first."
    exit 1
fi

# Check if kubectl is installed
if ! command -v kubectl &> /dev/null; then
    echo "kubectl is not installed. Please install it first."
    exit 1
fi

# Create a Kind cluster
echo "Creating Kind cluster: $CLUSTER_NAME..."
kind create cluster --name "$CLUSTER_NAME"

# Set the context to the new cluster
echo "Setting kubectl context to the new cluster..."
kubectl config use-context kind-$CLUSTER_NAME

# Install Calico for networking
kubectl apply -f https://docs.projectcalico.org/manifests/calico.yaml

# Display cluster info
echo "Cluster created. Displaying cluster info..."
kubectl cluster-info

