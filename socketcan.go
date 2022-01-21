package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
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
	containerWaitRetrySeconds = 5
	vcanNameTemplate          = "vcan%d"
)

// Enumeration class
type SocketCANLister struct{}

func (scl SocketCANLister) GetResourceNamespace() string {
	return resourceNamespace
}

func (scl SocketCANLister) Discover(pluginListCh chan dpm.PluginNameList) {
	var plugins = dpm.PluginNameList{"socketcan"}

	pluginListCh <- plugins
}

func (scl SocketCANLister) NewPlugin(socketcan string) dpm.PluginInterface {
	glog.V(3).Infof("Creating device plugin %s", socketcan)
	return &SocketCANDevicePlugin{
		assignmentCh: make(chan *Assignment),
	}
}

const (
	fakeDevicePath = "/var/run/device-plugin-socketcan-fakedev"
)

// Device plugin class
type SocketCANDevicePlugin struct {
	assignmentCh chan *Assignment
	device_paths map[string]*Assignment
	client       *containerd.Client
	ctx          context.Context
}

// PluginInterfaceStart is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after plugin instantiation and before its registartion to kubelet.
// This method could be used to prepare resources before they are offered to Kubernetes.
func (p *SocketCANDevicePlugin) Start() error {
	createFakeDevice()
	go p.interfaceCreator()
	return nil
}

func createFakeDevice() {
	_, err := os.Stat(fakeDevicePath)
	if err == nil {
		// already exists
	} else if os.IsNotExist(err) {
		if err := syscall.Mknod(fakeDevicePath, uint32(0777)|syscall.S_IFBLK, 0x0101); err != nil {
			panic(err)
		}
	} else {
		panic(err)
	}
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (scdp *SocketCANDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	devices := make([]*pluginapi.Device, 100)

	for i := range devices {
		devices[i] = &pluginapi.Device{
			ID:     fmt.Sprintf("socketcan-%d", i),
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
// of the steps to make the Device available in the container
func (scdp *SocketCANDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse

	for _, req := range r.ContainerRequests {
		var devices []*pluginapi.DeviceSpec
		for i, devid := range req.DevicesIDs {
			glog.V(3).Infof("Allocate: %s %s", devid, req)
			dev := new(pluginapi.DeviceSpec)
			assignmentPath := fmt.Sprintf("/tmp/k8s-socketcan/%s", devid)
			dev.HostPath = fakeDevicePath
			dev.ContainerPath = assignmentPath
			dev.Permissions = "r"
			devices = append(devices, dev)

			scdp.assignmentCh <- &Assignment{
				devid,
				assignmentPath,
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
// Manager
func (SocketCANDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return nil, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as reseting the device before making devices available to the container
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
	client, err := containerd.New("/var/run/containerd/containerd.sock")
	if err != nil {
		glog.V(3).Info("Failed to connect to containerd")
		panic(err)
	}
	p.client = client

	context := context.Background()
	p.ctx = namespaces.WithNamespace(context, "k8s.io")

	// we'll keep a list of pending allocations and keep checking for new containers every
	// containerWaitRetrySeconds
	p.device_paths = make(map[string]*Assignment)

	go func() {
		var retry *time.Timer = time.NewTimer(0)
		<-retry.C
		for {

			select {
			case alloc := <-p.assignmentCh:
				glog.V(3).Infof("New allocation request: %v", alloc)
				p.device_paths[alloc.ContainerPath] = alloc
			case <-retry.C:
			}

			glog.V(3).Infof("Trying to allocate: %v", p.device_paths)
			p.tryAllocatingDevices()

			if len(p.device_paths) > 0 {
				retry = time.NewTimer(containerWaitRetrySeconds * time.Second)
			}
		}
	}()
}

func findContainerWithDevice(ctx context.Context, containers []containerd.Container, device_paths map[string]*Assignment) (containerd.Container, string, error) {
	var last_err error
	for _, container := range containers {
		spec, err := container.Spec(ctx)
		if err != nil {
			last_err = err
		}
		for _, device := range spec.Linux.Devices {
			if _, ok := device_paths[device.Path]; ok {
				return container, device.Path, nil
			}
		}
	}
	return nil, "", last_err
}

func (p *SocketCANDevicePlugin) tryAllocatingDevices() {
	containers, err := p.client.Containers(p.ctx, "")
	if err != nil {
		glog.V(3).Infof("Failed to get container list: %v", err)
		return
	}

	for {
		container, path, err := findContainerWithDevice(p.ctx, containers, p.device_paths)
		if err != nil {
			glog.V(3).Infof("Failed to find container: %v", err)
			return
		}
		if container == nil {
			return
		}

		task, err := container.Task(p.ctx, nil)
		if err != nil {
			glog.V(3).Infof("Failed to get the task: %v", err)
			return
		}

		pids, err := task.Pids(p.ctx)
		if err != nil {
			glog.V(3).Infof("Failed to get task Pids: %v", err)
			return
		}

		err = p.createSocketcanInPod(p.device_paths[path].Name, int(pids[0].Pid))
		if err != nil {
			glog.V(3).Infof("Pod attachment failed with: %v", err)
			return
		}

		glog.V(3).Infof("Successfully created the vcan interface: %v", path)
		delete(p.device_paths, path)
	}
}

// creates the named vcan interface inside the pod namespace
func (nbdp *SocketCANDevicePlugin) createSocketcanInPod(ifname string, containerPid int) error {
	la := netlink.NewLinkAttrs()
	la.Name = ifname
	la.Flags = net.FlagUp
	la.Namespace = netlink.NsPid(containerPid)
	link := &netlink.GenericLink{
		LinkAttrs: la,
		LinkType:  "vcan",
	}

	err := netlink.LinkAdd(link)
	if err != nil {
		glog.V(3).Infof("LinkAdd failed with: %v", err)
		return err
	}

	return nil
}

type Assignment struct {
	DeviceID      string
	ContainerPath string
	Name          string
}

func main() {
	flag.Parse()
	manager := dpm.NewManager(SocketCANLister{})
	manager.Run()
}
