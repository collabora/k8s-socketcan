# SocketCAN Kubernetes device plugin

<div align="center">
  <video src="https://user-images.githubusercontent.com/107984/157506173-5d2788e1-71ca-49ae-afc3-b7f7afbab798.mp4"></video>
</div>

*We asked an [AI](https://colab.research.google.com/github/zippy731/disco-diffusion-turbo/blob/main/Disco_Diffusion_v5_Turbo_%5Bw_3D_animation%5D.ipynb) what it thinks about our SocketCAN Kubernetes plugin, this was it's answer...  
What we are still trying to figure out is: what's with all the lighthouses??*

This plugins enables you to use hardware-backed and virtual [SocketCAN](https://en.wikipedia.org/wiki/SocketCAN) interfaces inside your Kubernetes Pods.
`vcan` allows processes inside the pod to communicate with each other using the full Linux SocketCAN API. If you have
a real CAN adapter in you embeded system you can use this plugin to use it inside your Kubernetes deployment.

## Usage example

Assuming you have a [microk8s](https://microk8s.io) Kubernetes cluster you can install the SocketCAN plugin:

    microk8s kubectl apply -f https://raw.githubusercontent.com/Collabora/k8s-socketcan/main/k8s-socketcan-daemonset.yaml
    microk8s kubectl wait --for=condition=ready pod -l name=k8s-socketcan

NOTE: Using it with other k8s providers should require only an adjustment to the `init` container script to add a new
search path for the `containerd.sock` control socket and/or install the `vcan` kernel module.

Next, you can create a simple Pod that has two `vcan` interfaces enabled:

    microk8s kubectl apply -f https://raw.githubusercontent.com/Collabora/k8s-socketcan/main/k8s-socketcan-client-example.yaml
    microk8s kubectl wait --for=condition=ready pod k8s-socketcan-client-example

Afterwards you can run these two commands in two separate terminals to verify it's working correctly:

    microk8s kubectl exec -it k8s-socketcan-client-example -- candump vcan0
    microk8s kubectl exec -it k8s-socketcan-client-example -- cansend vcan0 5A1#11.2233.44556677.88

If everything goes according to plan you should see this in the two terminals:

[![video of the SocketCAN demo](setup.svg)](https://asciinema.org/a/469930)

Adding SocketCAN support to an existing Pod is as easy as adding a resource limit in the container spec:

```yaml
resources:
  limits:
    k8s.collabora.com/vcan: 1
```

## Hardware CAN interfaces

The SocketCAN device plugin also supports hardware CAN interfaces which is useful if you want to use (for example)
[K3s](https://k3s.io) to manage your embedded system software. It allows you to improve security by moving a SocketCAN network
interface into a single container and fully isolating it from any other applications on the system. It's a perfect
solution if you have a daemon that arbitrates all access to the CAN bus and you wish to containerize it.

To move a hardware CAN interface into a Pod you have to modify the DaemonSet to specify the names of the interfaces
you wish to make available. The names should be passed as a space separated list in the `SOCKETCAN_DEVICES` environment
variable:

```yaml
containers:
- name: k8s-socketcan
  image: ghcr.io/collabora/k8s-socketcan:latest
  env:
  - name: SOCKETCAN_DEVICES
    value: "can1 can2"
```

Afterwards, in the client container definition, instead of `k8s.collabora.com/vcan` you can specify the name of
the interface you wish to use (adding the `socketcan-` prefix to make sure it's unambiguous):

```yaml
resources:
  limits:
    k8s.collabora.com/socketcan-can1: 1
```

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

The plugin requires kernel support for SocketCAN (compiled in or as a module) on the cluster Nodes.
This is a package installation away on Ubuntu (so microk8s and Azure AKS work great) but unfortunatelly does not seem
possible at all on Alpine (so for example Rancher Desktop <= v1.1.1 does not work).

Currently each Pod get it's own isolated virtual SocketCAN network. There is no support for bridging
this to other Pods on the same node or to other nodes in the cluster. [Adding local bridging would be possible with
the `vxcan` functionality in the kernel and the `cangw` tool.](https://www.lagerdata.com/articles/forwarding-can-bus-traffic-to-a-docker-container-using-vxcan-on-raspberry-pi) Transparent bridging to other cluster nodes over
the network should be possible manually with [cannelloni](https://github.com/mguentner/cannelloni). Pull requests to either of these cases automatically
are more then welcome.

Currently, the plugin only work with clusters based on containerd, which includes most production clusters but
not Docker Desktop (we recommend using [microk8s](https://microk8s.io) instead). Pull requests to support `dockerd` are of course welcome.

## Other solutions

This project was inspired by the [k8s-device-plugin-socketcan](https://github.com/mpreu/k8s-device-plugin-socketcan) project by [Matthias Preu](https://www.matthiaspreu.com) but it was written
from scrach and has some significant improvements:

- it has a valid Open Source license (MIT)
- it supports `containerd` (which is used by default in most k8s clusters, like AKS, these days) instead of the `dockerd`
- it is capable of handling multiple Pods starting at the same time, which avoids head-of-the-line blocking issues when you have Pods that take a long time to start
- it supports exclusive use of real CAN interfaces

Neither project currently supports sharing a single SocketCAN interface among multiple Pods.
