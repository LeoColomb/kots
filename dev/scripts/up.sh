#!/bin/bash

set -e

. dev/scripts/common.sh

component=$1

# Check if a component name was provided
if [ -z "$component" ]; then
	echo "Error: No component name provided."
	exit 1
fi

# Check if already up
if [ -f "dev/patches/$component-down.yaml.tmp" ]; then
  up $component
  exit 0
fi

# Save current state
kubectl get deployment $(deployment $component) -oyaml > dev/patches/$component-down.yaml.tmp

# Prepare and apply the patch
render dev/patches/$component-up.yaml | kubectl patch deployment $(deployment $component) --patch-file=/dev/stdin

# Wait for rollout to complete
kubectl rollout status deployment/$(deployment $component)

# Up into the updated deployment
up $component
