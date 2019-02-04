#!/bin/bash

set -e

# Make sure cache dir exists and is writable
mkdir -p ~/.cache/go-build
sudo chown -R `whoami` ~/.cache/go-build

minikube delete || true  # In case we get a recycled machine
make launch-kube
sleep 5

# Wait until a connection with kubernetes has been established
echo "Waiting for connection to kubernetes..."
max_t=90
WHEEL="\|/-";
until {
  minikube status 2>&1 >/dev/null
  kubectl version 2>&1 >/dev/null
}; do
    if ((max_t-- <= 0)); then
        echo "Could not connect to minikube"
        echo "minikube status --alsologtostderr --loglevel=0 -v9:"
        echo "==================================================="
        minikube status --alsologtostderr --loglevel=0 -v9
        exit 1
    fi
    echo -en "\e[G$${WHEEL:0:1}";
    WHEEL="$${WHEEL:1}$${WHEEL:0:1}";
    sleep 1;
done
minikube status
kubectl version

echo "Running test suite based on BUCKET=$BUCKET"

PPS_SUITE=`echo $BUCKET | grep PPS > /dev/null; echo $?`

make install
make docker-build
for i in $(seq 3); do
    make clean-launch-dev || true # may be nothing to delete
    make launch-dev && break
    (( i < 3 )) # false if this is the last loop (causes exit)
    sleep 10
done

go install ./src/testing/match
sudo apt-get install jq

if [[ "$BUCKET" == "MISC" ]]; then
    if [[ "$TRAVIS_SECURE_ENV_VARS" == "true" ]]; then
        echo "Running the full misc test suite because secret env vars exist"

        make lint enterprise-code-checkin-test docker-build test-pfs-server \
            test-pfs-cmds test-deploy-cmds test-libs test-vault test-auth \
            test-enterprise test-worker test-admin
    else
        echo "Running the misc test suite with some tests disabled because secret env vars have not been set"

        # Do not run some tests when we don't have access to secret
        # credentials
        make lint enterprise-code-checkin-test docker-build test-pfs-server \
            test-pfs-cmds test-deploy-cmds test-libs test-admin
    fi
elif [[ "$BUCKET" == "EXAMPLES" ]]; then
    echo "Running the example test suite"

    # Some examples were designed for older versions of pachyderm and are not used here
    # TODO(ys): add run example when this is fixed: https://github.com/pachyderm/pachyderm/issues/3428

    pushd examples/opencv
        pachctl --no-port-forwarding create-repo images
        pachctl --no-port-forwarding create-pipeline -f edges.json
        pachctl --no-port-forwarding create-pipeline -f montage.json
        pachctl --no-port-forwarding put-file images master -i images.txt
        pachctl --no-port-forwarding put-file images master -i images2.txt

        # wait for everything to finish
        commit_id=`pachctl --no-port-forwarding list-commit images -n 1 --raw | jq .commit.id -r`
        pachctl --no-port-forwarding flush-job images/$commit_id

        # ensure the montage image was generated
        pachctl --no-port-forwarding inspect-file montage master montage.png
    popd


    pushd examples/shuffle
        pachctl --no-port-forwarding create-repo fruits
        pachctl --no-port-forwarding create-repo pricing
        pachctl --no-port-forwarding create-pipeline -f shuffle.json
        pachctl --no-port-forwarding put-file fruits master -f mango.jpeg
        pachctl --no-port-forwarding put-file fruits master -f apple.jpeg
        pachctl --no-port-forwarding put-file pricing master -f mango.json
        pachctl --no-port-forwarding put-file pricing master -f apple.json

        # wait for everything to finish
        commit_id=`pachctl --no-port-forwarding list-commit fruits -n 1 --raw | jq .commit.id -r`
        pachctl --no-port-forwarding flush-job fruits/$commit_id
        pachctl --no-port-forwarding flush-commit fruits/$commit_id

        # check downloaded and uploaded bytes
        downloaded_bytes=`pachctl --no-port-forwarding list-job -p shuffle --raw | jq '.stats.download_bytes | values'`
        if [ "$downloaded_bytes" != "" ]; then
            echo "Unexpected downloaded bytes in shuffle repo: $DOWNLOADED_BYTES"
            exit 1
        fi

        uploaded_bytes=`pachctl --no-port-forwarding list-job -p shuffle --raw | jq '.stats.upload_bytes | values'`
        if [ "$uploaded_bytes" != "" ]; then
            echo "Unexpected downloaded bytes in shuffle repo: $uploaded_bytes"
            exit 1
        fi

        # check that the files were made
        files=`pachctl --no-port-forwarding list-file shuffle master "*" --raw | jq '.file.path' -r`
        expected_files=`echo -e "/apple\n/apple/cost.json\n/apple/img.jpeg\n/mango\n/mango/cost.json\n/mango/img.jpeg"`
        if [ "$files" != "$expected_files" ]; then
            echo "Unexpected output files in shuffle repo: $files"
            exit 1
        fi
    popd

    pushd examples/word_count
        # note: we do not test reducing because it's slower
        pachctl --no-port-forwarding create-repo urls
        pachctl --no-port-forwarding put-file urls master -f Wikipedia
        pachctl --no-port-forwarding create-pipeline -f scraper.json
        pachctl --no-port-forwarding create-pipeline -f map.json

        # wait for everything to finish
        commit_id=`pachctl --no-port-forwarding list-commit urls -n 1 --raw | jq .commit.id -r`
        pachctl --no-port-forwarding flush-commit urls/$commit_id

        # just make sure the count for the word 'wikipedia' is a valid and
        # positive int, since the specific count may vary over time
        wikipedia_count=`pachctl --no-port-forwarding get-file map master wikipedia`
        if [ $wikipedia_count -le 0 ]; then
            echo "Unexpected count for the word 'wikipedia': $wikipedia_count"
            exit 1
        fi
    popd
elif [[ $PPS_SUITE -eq 0 ]]; then
    PART=`echo $BUCKET | grep -Po '\d+'`
    NUM_BUCKETS=`cat etc/build/PPS_BUILD_BUCKET_COUNT`
    echo "Running pps test suite, part $PART of $NUM_BUCKETS"
    LIST=`go test -v  ./src/server/ -list ".*" | grep -v ok | grep -v Benchmark`
    COUNT=`echo $LIST | tr " " "\n" | wc -l`
    BUCKET_SIZE=$(( $COUNT / $NUM_BUCKETS ))
    MIN=$(( $BUCKET_SIZE * $(( $PART - 1 )) ))
    #The last bucket may have a few extra tests, to accommodate rounding errors from bucketing:
    MAX=$COUNT
    if [[ $PART -ne $NUM_BUCKETS ]]; then
        MAX=$(( $MIN + $BUCKET_SIZE ))
    fi

    RUN=""
    INDEX=0

    for test in $LIST; do
        if [[ $INDEX -ge $MIN ]] && [[ $INDEX -lt $MAX ]] ; then
            if [[ "$RUN" == "" ]]; then
                RUN=$test
            else
                RUN="$RUN|$test"
            fi
        fi
        INDEX=$(( $INDEX + 1 ))
    done
    echo "Running $( echo $RUN | tr '|' '\n' | wc -l ) tests of $COUNT total tests"
    make RUN=-run=\"$RUN\" test-pps-helper
else
    echo "Unknown bucket"
    exit 1
fi

# Disable aws CI for now, see:
# https://github.com/pachyderm/pachyderm/issues/2109
exit 0

echo "Running local tests"
make local-test

echo "Running aws tests"

sudo pip install awscli
sudo apt-get install realpath uuid
wget --quiet https://github.com/kubernetes/kops/releases/download/1.7.10/kops-linux-amd64
chmod +x kops-linux-amd64
sudo mv kops-linux-amd64 /usr/local/bin/kops

# Use the secrets in the travis environment to setup the aws creds for the aws command:
echo -e "${AWS_ACCESS_KEY_ID}\n${AWS_SECRET_ACCESS_KEY}\n\n\n" \
    | aws configure

make install
echo "pachctl is installed here:"
which pachctl

# Travis doesn't come w an ssh key
# kops needs one in place (because it enables ssh access to nodes w it)
# for now we'll just generate one on the fly
# travis supports adding a persistent one if we pay: https://docs.travis-ci.com/user/private-dependencies/#Generating-a-new-key
if [[ ! -e ${HOME}/.ssh/id_rsa ]]; then
    ssh-keygen -t rsa -b 4096 -C "buildbot@pachyderm.io" -f ${HOME}/.ssh/id_rsa -N ''
    echo "generated ssh keys:"
    ls ~/.ssh
    cat ~/.ssh/id_rsa.pub
fi

# Need to login so that travis can push the bench image
docker login -u pachydermbuildbot -p ${DOCKER_PWD}

# Run tests in the cloud
make aws-test
