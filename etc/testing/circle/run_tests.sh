#!/bin/bash

set -ex

# shellcheck disable=SC1090
source "$(dirname "$0")/env.sh"

VM_IP="$(minikube ip)"
export VM_IP

PACH_PORT="30650"
export PACH_PORT

ENTERPRISE_PORT="31650"
export ENTEPRRISE_PORT

TESTFLAGS="-v | stdbuf -i0 tee -a /tmp/results"
export TESTFLAGS

# make launch-kube connects with kubernetes, so it should just be available
minikube status
kubectl version

# any tests that build images will do it directly in minikube's docker registry
eval "$(minikube docker-env)"

echo "Running test suite based on BUCKET=$BUCKET"

function test_bucket {
    set +x
    package="${1}"
    target="${2}"
    bucket_num="${3}"
    num_buckets="${4}"
    if (( bucket_num == 0 )); then
        echo "Error: bucket_num should be > 0, but was 0" >/dev/stderr
        exit 1
    fi

    echo "Running bucket $bucket_num of $num_buckets"
    # shellcheck disable=SC2207
    tests=( $(go test -v  "${package}" -list ".*" | grep -v '^ok' | grep -v '^Benchmark') )
    # Add anchors for the regex so we don't run collateral tests
    tests=( "${tests[@]/#/^}" )
    tests=( "${tests[@]/%/\$\$}" )
    total_tests="${#tests[@]}"
    # Determine the offset and length of the sub-array of tests we want to run
    # The last bucket may have a few extra tests, to accommodate rounding
    # errors from bucketing:
    let "bucket_size=total_tests/num_buckets" \
        "start=bucket_size * (bucket_num-1)" \
        "bucket_size+=bucket_num < num_buckets ? 0 : total_tests%num_buckets"
    test_regex="$(IFS=\|; echo "${tests[*]:start:bucket_size}")"
    echo "Running ${bucket_size} tests of ${total_tests} total tests"
    make RUN="-run='${test_regex}'" "${target}"
    set -x
}

# Clean cached test results
go clean -testcache

case "${BUCKET}" in
 MISC)
    make lint
    make check-buckets
    make enterprise-code-checkin-test
    go install -v ./src/testing/match
    make test-cmds
    make test-proto-static
    make test-transaction
    make test-s3gateway-unit
    make test-worker
    # these tests require secure env vars to run, which aren't available
    # when the PR is coming from an outside contributor - so we just
    # disable them
    # make test-tls
    ;;
  INTERNAL)
    go install -v ./src/testing/match
    bash -ceo pipefail "go test -p 1 -count 1 ./src/internal/... ${TESTFLAGS}"
    ;;
  EXAMPLES)
    echo "Running the example test suite"
    ./etc/testing/examples.sh
    ;;
  PFS)
    make test-pfs-server
    ;;
  PPS?)
    make docker-build-kafka
    bucket_num="${BUCKET#PPS}"
    test_bucket "./src/server" test-pps "${bucket_num}" "${PPS_BUCKETS}"
    if [[ "${bucket_num}" -eq "${PPS_BUCKETS}" ]]; then
      export PACH_TEST_WITH_AUTH=1
      go test -v -count=1 ./src/server/pps/server -timeout 420s
    fi
    ;;
  AUTH)
    make test-identity
    make test-auth
    ;;
  ENTERPRISE)
    make test-license
    make test-enterprise
    # Launch a stand-alone enterprise server in a separate namespace
    make launch-enterprise
    echo "{\"pachd_address\": \"grpc://${VM_IP}:${ENTERPRISE_PORT}\", \"source\": 2}" | pachctl config set context "enterprise" --overwrite
    pachctl config set active-enterprise-context enterprise
    make test-enterprise-integration
    ;;
  *)
    echo "Unknown bucket"
    exit 1
    ;;
esac
