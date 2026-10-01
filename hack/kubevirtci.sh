#!/bin/bash

set -ex

export KUBEVIRT_PROVIDER="${KUBEVIRT_PROVIDER:-k8s-1.36}"
export KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-main}
export KUBEVIRTCI_TAG=${KUBEVIRTCI_TAG:-2609300856-4bec1cc7}

# kubevirtci defaults to /run/podman/podman.sock (system socket); rootless podman
# uses a per-user socket instead. Detect it here so cluster-up finds the runtime.
if [[ -z "${KUBEVIRTCI_PODMAN_SOCKET:-}" ]]; then
  _user_socket="/run/user/$(id -u)/podman/podman.sock"
  if [[ -S "${_user_socket}" ]]; then
    export KUBEVIRTCI_PODMAN_SOCKET="${_user_socket}"
  fi
fi

_base_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
_cluster_up_dir="${_base_dir}/_cluster-up"
_kubectl="${_cluster_up_dir}/cluster-up/kubectl.sh"
_kubessh="${_cluster_up_dir}/cluster-up/ssh.sh"
_kubevirtcicli="${_cluster_up_dir}/cluster-up/cli.sh"
_action=$1
shift

function kubevirtci::fetch_kubevirtci() {
	if [[ ! -d ${_cluster_up_dir} ]]; then
    git clone --depth 1 --branch "${KUBEVIRTCI_TAG}" https://github.com/kubevirt/kubevirtci.git "${_cluster_up_dir}"
  fi
}

function kubevirtci::up() {
  make cluster-up -C "${_cluster_up_dir}"
  KUBECONFIG=$(kubevirtci::kubeconfig)
  export KUBECONFIG

  # Treat main as stable version
  if [ "$KUBEVIRT_VERSION" = "main" ]; then
    KUBEVIRT_VERSION=$(curl -L https://storage.googleapis.com/kubevirt-prow/devel/release/kubevirt/kubevirt/stable.txt)
  fi

  echo "installing kubevirt..."
  ${_kubectl} apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml"
  ${_kubectl} apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-cr.yaml"

  echo "waiting for kubevirt to become ready, this can take a few minutes..."
  ${_kubectl} -n kubevirt wait kv kubevirt --for condition=Available --timeout=15m
}

function kubevirtci::down() {
  make cluster-down -C "${_cluster_up_dir}"
}

function kubevirtci::kubeconfig() {
  "${_cluster_up_dir}/cluster-up/kubeconfig.sh"
}

function kubevirtci::registry() {
  port=$(${_kubevirtcicli} ports registry 2>/dev/null)
  echo "localhost:${port}"
}

kubevirtci::fetch_kubevirtci

case ${_action} in
  "up")
    kubevirtci::up
    ;;
  "down")
    kubevirtci::down
    ;;
  "kubeconfig")
    kubevirtci::kubeconfig
    ;;
  "registry")
    kubevirtci::registry
    ;;
  "ssh")
    ${_kubessh} "$@"
    ;;
  "kubectl")
    ${_kubectl} "$@"
    ;;
  *)
    echo "No command provided, known commands are 'up', 'down', 'kubeconfig', 'registry', 'kubectl', 'ssh'"
    exit 1
    ;;
esac
