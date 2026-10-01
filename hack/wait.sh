#!/bin/bash
set -e

kubectl -n kubevirt wait deployment deadline-eviction-controller \
  --for condition=Available --timeout=5m
