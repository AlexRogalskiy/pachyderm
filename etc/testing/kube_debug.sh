#!/bin/bash

set -e

echo "=== TEST FAILED OR TIMED OUT, DUMPING DEBUG INFO ==="

# TODO: Extend this to show kubectl describe output for failed pods, this will
# probably show why things are hanging.

cmds=(
  'kubectl version'
  'kubectl get all --all-namespaces'
  'kubectl describe pod -l suite=pachyderm,app=pachd'
  'kubectl describe pod -l suite=pachyderm,app=etcd'
  # Set --tail b/c by default 'kubectl logs' only outputs 10 lines if -l is set
  'kubectl logs --tail=500 -l suite=pachyderm,app=pachd'
  'kubectl logs --tail=1000 -l suite=pachyderm,app=pachd --previous || true # if pachd restarted'
  'sudo dmesg | tail -n 40'
  '{ minikube logs | tail -n 100; } || true'
  'top -b -n 1 | head -n 40'
  'df -h'
)
for c in "${cmds[@]}"; do
  echo "======================================================================"
  echo "${c}"
  echo "----------------------------------------------------------------------"
  eval "${c}"
done
echo "======================================================================"
