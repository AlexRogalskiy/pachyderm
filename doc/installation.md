# Local Installation
This guide will walk you through the recommended path to get Pachyderm running locally on OSX or Linux.

If you hit any errors not covered in this guide submit an issue on [GitHub](github.com/pachyderm/pachyderm) or email us at [support@pachyderm.io](mailto:support@pachyderm.io) and we can help you right away.  

## Prerequisites
- [Minikube](#minikube) (and VirtualBox)
- [Pachyderm Command Line Interface](#pachctl)

### Minikube

Kubernetes offers a fantastic guide to [install minikube](http://kubernetes.io/docs/getting-started-guides/minikube). Follow the Kubernetes installation guide to install Virtual Box, Minikibe, and Kubectl. Then come back here to install Pachyderm. 

### Pachctl

`pachctl` is a command-line utility used for interacting with a Pachyderm cluster.


```shell
# For OSX:
$ brew tap pachyderm/tap && brew install pachctl

# For Linux (64 bit):
$ curl -o /tmp/pachctl.deb -L https://pachyderm.io/pachctl.deb && dpkg -i /tmp/pachctl.deb
```

You can try running `pachctl version` to check that this worked correctly, but Pachyderm itself isn't deployed yet so you won't get a `pachd` version. 

```sh
$ pachctl version
COMPONENT           VERSION
pachctl             1.1.0
pachd               (version unknown) : error connecting to pachd server at address (0.0.0.0:30650): context deadline exceeded

please make sure pachd is up (`kubectl get all`) and portforwarding is enabled
```

### Deploy Pachyderm
Now that you have Minikube running, it's incredibly easy to deploy Pachyderm.

```sh
kubectl create -f https://pachyderm.io/manifest.json
```
This generates a Pachyderm manifest and deploys Pachyderm on Kubernetes. It may take a few minutes for the pachd nodes to be running because it's pulling containers from DockerHub. You can see the cluster status by using:

```sh
$ kubectl get all
NAME            DESIRED      CURRENT       AGE
etcd            1            1             6s
pachd           2            2             6s
rethink         1            1             6s
NAME            CLUSTER-IP   EXTERNAL-IP   PORT(S)                        AGE
etcd            10.0.0.45    <none>        2379/TCP,2380/TCP              6s
kubernetes      10.0.0.1     <none>        443/TCP                        6m
pachd           10.0.0.101   <nodes>       650/TCP                        6s
rethink         10.0.0.182   <nodes>       8080/TCP,28015/TCP,29015/TCP   6s
NAME            READY        STATUS        RESTARTS                       AGE
etcd-swoag      1/1          Running       0                              6s
pachd-7xyse     0/1          Running       0                              6s
pachd-gfdc6     0/1          Running       0                              6s
rethink-v5rsx   1/1          Running       0                              6s
```
Note: If you see a few restarts on the pachd nodes, that's ok. That simply means that Kubernetes tried to bring up those containers before Rethink was ready so it restarted them. 

### Port Forwarding

The last step is to set up port forwarding so commands you send can reach Pachyderm within the VM. We background this process since port forwarding blocks. 

```shell
#copy one of the pachd pod names. E.g. "pachd-7xyse" from above
kubectl port-forward <pachd_pod_name> 30650:650 &
```

Once port forwarding is complete, pachctl should automatically be connected. Try `pachctl version` to make sure everything is working. 

```shell
$ pachctl version
COMPONENT           VERSION
pachctl             1.1.0
pachd               1.1.0
```

We're good to go!
