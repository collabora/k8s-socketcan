package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceNamespace         = "k8s.collabora.com"
	containerWaitDelaySeconds = 5
	vcanNameTemplate          = "vcan%d"
)

// Enumeration class
type SocketCANLister struct {
	real_devices []string
}

func (scl SocketCANLister) GetResourceNamespace() string {
	return resourceNamespace
}

func (scl SocketCANLister) Discover(pluginListCh chan dpm.PluginNameList) {
	all_devices := append([]string{"vcan"}, scl.real_devices...)
	var plugins = dpm.PluginNameList(all_devices)

	pluginListCh <- plugins
}

func (scl SocketCANLister) NewPlugin(kind string) dpm.PluginInterface {
	glog.V(3).Infof("Creating device plugin %s", kind)

	if kind == "vcan" {
		return &VCANDevicePlugin{
			assignmentCh: make(chan *Assignment),
		}
	} else {
		return &SocketCANDevicePlugin{
			assignmentCh: make(chan *Assignment),
			device_name:  strings.TrimPrefix(kind, "socketcan-"),
		}
	}
}

// Device plugin class
const (
	fakeDevicePath = "/root/var/run/k8s-socketcan/fakedev"
)

type VCANDevicePlugin struct {
	assignmentCh chan *Assignment
	device_paths map[string]*Assignment
	client       *containerd.Client
	ctx          context.Context
}

type Assignment struct {
	ContainerPath string
	Name          string
}

// PluginInterfaceStart is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after plugin instantiation and before its registartion to kubelet.
// This method could be used to prepare resources before they are offered to Kubernetes.
func (p *VCANDevicePlugin) Start() error {
	go p.interfaceCreator()
	return nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list.
func (scdp *VCANDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	devices := make([]*pluginapi.Device, 100)

	for i := range devices {
		devices[i] = &pluginapi.Device{
			ID:     fmt.Sprintf("vcan-%d", i),
			Health: pluginapi.Healthy,
		}
	}
	s.Send(&pluginapi.ListAndWatchResponse{Devices: devices})

	for {
		time.Sleep(10 * time.Second)
	}
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (scdp *VCANDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse

	for _, req := range r.ContainerRequests {
		var devices []*pluginapi.DeviceSpec
		for i, devid := range req.DevicesIDs {
			dev := new(pluginapi.DeviceSpec)
			containerPath := fmt.Sprintf("/tmp/k8s-socketcan/%s", devid)
			dev.HostPath = fakeDevicePath
			dev.ContainerPath = containerPath
			dev.Permissions = "r"
			devices = append(devices, dev)

			scdp.assignmentCh <- &Assignment{
				containerPath,
				fmt.Sprintf(vcanNameTemplate, i),
			}
		}

		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: devices,
		})

	}

	return &response, nil
}

// GetDevicePluginOptions returns options to be communicated with Device
// Manager.
func (VCANDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return nil, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as reseting the device before making devices available to the container.
func (VCANDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return nil, nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate
// from a list of available ones. The resulting preferred allocation is not
// guaranteed to be the allocation ultimately performed by the
// devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (VCANDevicePlugin) GetPreferredAllocation(ctx context.Context, in *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return nil, nil
}

// This is an internal method which injects the socketcan virtual network interfaces.
// K8s only support setting up mounts and `/dev` devices, so we create a fake device node
// and keep checking all containers to look for this sentinel device. After we find one, we
// inject the network interface into it's namespace.
func (p *VCANDevicePlugin) interfaceCreator() {
	client, err := containerd.New("/root/var/run/k8s-socketcan/containerd.sock")
	if err != nil {
		glog.V(3).Info("Failed to connect to containerd")
		panic(err)
	}
	p.client = client

	context := context.Background()
	p.ctx = namespaces.WithNamespace(context, "k8s.io")

	// we'll keep a list of pending allocations and keep checking for new containers every
	// containerWaitDelaySeconds
	p.device_paths = make(map[string]*Assignment)

	go func() {
		var retry *time.Timer = time.NewTimer(0)
		var waiting = false
		<-retry.C
		for {
			select {
			case alloc := <-p.assignmentCh:
				glog.V(3).Infof("New allocation request: %v", alloc)
				p.device_paths[alloc.ContainerPath] = alloc
			case <-retry.C:
				waiting = false
				glog.V(3).Infof("Trying to allocate: %v", p.device_paths)
				p.tryAllocatingDevices()
			}

			if !waiting && len(p.device_paths) > 0 {
				retry = time.NewTimer(containerWaitDelaySeconds * time.Second)
				waiting = true
			}
		}
	}()
}

// Searches through all containers for matching fake devices and creates the network interfaces.
func (p *VCANDevicePlugin) tryAllocatingDevices() {
	containers, err := p.client.Containers(p.ctx, "")
	if err != nil {
		glog.V(3).Infof("Failed to get container list: %v", err)
		return
	}

	for _, container := range containers {
		spec, err := container.Spec(p.ctx)
		if err != nil {
			glog.V(3).Infof("Failed to get fetch container spec: %v", err)
			return
		}
		for _, device := range spec.Linux.Devices {
			if assignment, ok := p.device_paths[device.Path]; ok {
				// we found a container we are looking for
				task, err := container.Task(p.ctx, nil)
				if err != nil {
					glog.Warningf("Failed to get the task: %v", err)
					return
				}

				pids, err := task.Pids(p.ctx)
				if err != nil {
					glog.Warningf("Failed to get task Pids: %v", err)
					return
				}

				err = p.createSocketcanInPod(assignment.Name, int(pids[0].Pid))
				if err != nil {
					glog.Warningf("Failed to create interface: %v: %v", assignment.Name, err)
					return
				}

				glog.V(3).Infof("Successfully created the vcan interface: %v", assignment)
				delete(p.device_paths, device.Path)
			}
		}
	}
}

// Creates the named vcan interface inside the pod namespace.
func (nbdp *VCANDevicePlugin) createSocketcanInPod(ifname string, containerPid int) error {
	la := netlink.NewLinkAttrs()
	la.Name = ifname
	la.Flags = net.FlagUp
	la.Namespace = netlink.NsPid(containerPid)

	return netlink.LinkAdd(&netlink.GenericLink{
		LinkAttrs: la,
		LinkType:  "vcan",
	})
}

type SocketCANDevicePlugin struct {
	assignmentCh chan *Assignment
	device_name  string
	device_paths map[string]*Assignment
	client       *containerd.Client
	ctx          context.Context
}

// PluginInterfaceStart is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after plugin instantiation and before its registartion to kubelet.
// This method could be used to prepare resources before they are offered to Kubernetes.
func (p *SocketCANDevicePlugin) Start() error {
	go p.interfaceCreator()
	return nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list.
func (scdp *SocketCANDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	devices := make([]*pluginapi.Device, 1)

	for i := range devices {
		devices[i] = &pluginapi.Device{
			ID:     scdp.device_name,
			Health: pluginapi.Healthy,
		}
	}
	s.Send(&pluginapi.ListAndWatchResponse{Devices: devices})

	for {
		time.Sleep(10 * time.Second)
	}
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
func (scdp *SocketCANDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse

	for _, req := range r.ContainerRequests {
		var devices []*pluginapi.DeviceSpec
		for _, devid := range req.DevicesIDs {
			dev := new(pluginapi.DeviceSpec)
			containerPath := fmt.Sprintf("/tmp/k8s-socketcan/socketcan-%s", devid)
			dev.HostPath = fakeDevicePath
			dev.ContainerPath = containerPath
			dev.Permissions = "r"
			devices = append(devices, dev)

			scdp.assignmentCh <- &Assignment{
				containerPath,
				scdp.device_name,
			}
		}

		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: devices,
		})

	}

	return &response, nil
}

// GetDevicePluginOptions returns options to be communicated with Device
// Manager.
func (SocketCANDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return nil, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as reseting the device before making devices available to the container.
func (SocketCANDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return nil, nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate
// from a list of available ones. The resulting preferred allocation is not
// guaranteed to be the allocation ultimately performed by the
// devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (SocketCANDevicePlugin) GetPreferredAllocation(ctx context.Context, in *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return nil, nil
}

// This is an internal method which injects the socketcan virtual network interfaces.
// K8s only support setting up mounts and `/dev` devices, so we create a fake device node
// and keep checking all containers to look for this sentinel device. After we find one, we
// inject the network interface into it's namespace.
func (p *SocketCANDevicePlugin) interfaceCreator() {
	client, err := containerd.New("/var/run/k8s-socketcan/containerd.sock")
	if err != nil {
		glog.V(3).Info("Failed to connect to containerd")
		panic(err)
	}
	p.client = client

	context := context.Background()
	p.ctx = namespaces.WithNamespace(context, "k8s.io")

	// we'll keep a list of pending allocations and keep checking for new containers every
	// containerWaitDelaySeconds
	p.device_paths = make(map[string]*Assignment)

	go func() {
		var retry *time.Timer = time.NewTimer(0)
		var waiting = false
		<-retry.C
		for {
			select {
			case alloc := <-p.assignmentCh:
				glog.V(3).Infof("New allocation request: %v", alloc)
				p.device_paths[alloc.ContainerPath] = alloc
			case <-retry.C:
				waiting = false
				glog.V(3).Infof("Trying to allocate: %v", p.device_paths)
				p.tryAllocatingDevices()
			}

			if !waiting && len(p.device_paths) > 0 {
				retry = time.NewTimer(containerWaitDelaySeconds * time.Second)
				waiting = true
			}
		}
	}()
}

// Searches through all containers for matching fake devices and creates the network interfaces.
func (p *SocketCANDevicePlugin) tryAllocatingDevices() {
	containers, err := p.client.Containers(p.ctx, "")
	if err != nil {
		glog.V(3).Infof("Failed to get container list: %v", err)
		return
	}

	for _, container := range containers {
		spec, err := container.Spec(p.ctx)
		if err != nil {
			glog.V(3).Infof("Failed to get fetch container spec: %v", err)
			return
		}
		for _, device := range spec.Linux.Devices {
			if assignment, ok := p.device_paths[device.Path]; ok {
				// we found a container we are looking for
				task, err := container.Task(p.ctx, nil)
				if err != nil {
					glog.Warningf("Failed to get the task: %v", err)
					return
				}

				pids, err := task.Pids(p.ctx)
				if err != nil {
					glog.Warningf("Failed to get task Pids: %v", err)
					return
				}

				err = p.moveSocketcanIntoPod(assignment.Name, int(pids[0].Pid))
				if err != nil {
					glog.Warningf("Failed to create interface: %v: %v", assignment.Name, err)
					return
				}

				glog.V(3).Infof("Successfully created the vcan interface: %v", assignment)
				delete(p.device_paths, device.Path)
			}
		}
	}
}

// Creates the named vcan interface inside the pod namespace.
func (nbdp *SocketCANDevicePlugin) moveSocketcanIntoPod(ifname string, containerPid int) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return err
	}
	return netlink.LinkSetNsPid(link, containerPid)
}

func main() {
	flag.Parse()

	// Kubernetes plugin uses the kubernetes library, which uses glog, which logs to the filesystem by default,
	// while we need all logs to go to stderr
	// See also: https://github.com/coredns/coredns/pull/1598
	flag.Set("logtostderr", "true")

	hw_devices := []string{}

	device_list := os.Getenv("SOCKETCAN_DEVICES")
	if device_list != "" {
		hw_devices = strings.Split(device_list, " ")
	}

	manager := dpm.NewManager(SocketCANLister{hw_devices})
	manager.Run()
}
