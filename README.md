# Virtual SocketCAN Kubernetes device plugin

This plugins enables you to create virtual [SocketCAN](https://en.wikipedia.org/wiki/SocketCAN) interfaces inside your Kubernetes Pods.
`vcan` allows processes inside the pod to communicate with each other using the full Linux SocketCAN API.

## Usage example

Assuming you have a microk8s Kubernetes cluster with `kubectl` configured properly you can install the SocketCAN plugin:

    kubectl apply -f https://raw.githubusercontent.com/jpc/k8s-socketcan/main/k8s-socketcan-daemonset-microk8s.yaml

There is also a YAML file for Azure AKS. Using it with other k8s providers may required an adjustment of the
`run-containerd` hostPath volume to point to the containerd control socket.

Next, you can create a simple Pod that has two vcan interfaces enabled:

    kubectl apply -f https://raw.githubusercontent.com/jpc/k8s-socketcan/main/k8s-socketcan-client-example.yaml

Afterwards you can run these two commands in two separate terminals to verify it's working correctly:

    kubectl exec -it k8s-socketcan-client-example -- candump vcan0
    kubectl exec -it k8s-socketcan-client-example -- cansend vcan0 5A1#11.2233.44556677.88

## Learn more

SocketCAN official documentation is a little bit scattered around the Internet but we found these two presentations
by Oliver Hartkopp from Microchip to be invaluable to understand the motivation behind and the architecture of the
SocketCAN subsystem:

- [The CAN Subsystem of the Linux Kernel](https://wiki.automotivelinux.org/_media/agl-distro/agl2017-socketcan-print.pdf) (includes discussion of the kernel interfaces, C code examples, usage of the "firewall" filters on CAN frames)
- [Design & separation of CAN applications](https://wiki.automotivelinux.org/_media/agl-distro/agl2018-socketcan.pdf) (discusses the `vxcan` interface pairs and SocketCAN usage inside of namespaces/containers)

Other resources:

- [SocketCAN - The official CAN API of the Linux kernel by Marc Kleine-Budde from Pengutronix](https://www.can-cia.org/fileadmin/resources/documents/proceedings/2012_kleine-budde.pdf)
- [python-can library](https://python-can.readthedocs.io/en/master/index.html)

## Limitations

Currently each Pod get it's own isolated virtual SocketCAN network. There is no support for bridging
this to other Pods on the same node or to other nodes in the cluster.

[Adding local bridging would be possible with the `vxcan` functionality in the kernel and the `cangw` tool.](https://www.lagerdata.com/articles/forwarding-can-bus-traffic-to-a-docker-container-using-vxcan-on-raspberry-pi)
Transparent bridging to other cluster nodes over the network should be possible manually with [cannelloni](https://github.com/mguentner/cannelloni).
Pull requests to integrate both of these automatically are more then welcome.

## Other solutions

This project was inspired by the [k8s-device-plugin-socketcan](https://github.com/mpreu/k8s-device-plugin-socketcan) project by [Matthias Preu](https://www.matthiaspreu.com) but it was written
from scrach and has some significant improvements:

- it has a valid Open Source license (MIT)
- it supports `containerd` (which is used by default in most k8s clusters, like AKS, these days) instead of the Docker daemon
- it is capable of handling multiple Pods starting at the same time, which avoids head-of-the-line blocking issues when you have Pods that take a long time to start

Both projects currently support only separate per-Pod SocketCAN networks.
