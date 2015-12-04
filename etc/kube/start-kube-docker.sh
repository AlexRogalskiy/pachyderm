#!/bin/sh

set -Ee

docker run --net=host -d gcr.io/google_containers/etcd:2.0.12 /usr/local/bin/etcd --addr=127.0.0.1:4001 --bind-addr=0.0.0.0:4001 --data-dir=/var/etcd/data
kubelet=$(docker create \
    --volume=/:/rootfs:ro \
    --volume=/sys:/sys:ro \
    --volume=/dev:/dev \
    --volume=/var/lib/docker/:/var/lib/docker:rw \
    --volume=/var/lib/kubelet/:/var/lib/kubelet:rw \
    --volume=/var/run:/var/run:rw \
    --net=host \
    --pid=host \
    --privileged=true \
    gcr.io/google_containers/hyperkube:v1.1.2 \
    /hyperkube kubelet \
        --containerized \
        --hostname-override="127.0.0.1" \
        --address="0.0.0.0" \
        --api-servers=http://localhost:8080 \
        --config=/etc/kubernetes/manifests \
        --allow-privileged=true)
docker cp etc/kube/master.json $kubelet:/etc/kubernetes/manifests/master.json
docker start $kubelet
docker run -d --net=host --privileged gcr.io/google_containers/hyperkube:v1.1.2 /hyperkube proxy --master=http://127.0.0.1:8080 --v=2
until kubectl version 2>/dev/null >/dev/null; do sleep 5; done
