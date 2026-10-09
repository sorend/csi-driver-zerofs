package driver

import (
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sorend/csi-driver-zerofs/pkg/zerofs"
)

const (
	DriverName      = "zerofs.csi.sorend.github.com"
	DriverVersion   = "1.0.0"
	TopologyKeyNode = "topology.zerofs.csi.sorend.github.com/node"
)

type Driver struct {
	options *DriverOptions

	ids    *IdentityServer
	Cs     *ControllerServer
	ns     *NodeServer
	server *NonBlockingGRPCServer

	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
}

type DriverOptions struct {
	NodeID      string
	DriverName  string
	Endpoint    string
	Namespace   string
	Kubeconfig  string
	WorkDir     string
	ZerofsImage string
}

func NewDriver(options *DriverOptions) *Driver {
	if options.DriverName == "" {
		options.DriverName = DriverName
	}

	d := &Driver{
		options: options,
	}

	d.ids = NewIdentityServer(d)
	d.Cs = NewControllerServer(d)
	d.ns = NewNodeServer(d)

	return d
}

func (d *Driver) Run() error {
	scheme, addr := parseEndpoint(d.options.Endpoint)

	d.server = NewNonBlockingGRPCServer(scheme, addr)
	d.server.Start(d.ids, d.Cs, d.ns)
	d.server.Wait()

	return nil
}

// parseEndpoint splits a CSI endpoint into a net.Listen network and address.
// Both "unix:///csi/csi.sock" and "tcp://0.0.0.0:10000" forms are accepted;
// endpoints without a known scheme default to a unix socket.
func parseEndpoint(endpoint string) (string, string) {
	for _, scheme := range []string{"unix", "tcp"} {
		if strings.HasPrefix(endpoint, scheme+"://") {
			return scheme, strings.TrimPrefix(endpoint, scheme+"://")
		}
	}
	return "unix", endpoint
}

func (d *Driver) Stop() {
	if d.server != nil {
		d.server.Stop()
	}
}

func (d *Driver) GetManager() *zerofs.Manager {
	if d.Cs != nil {
		return d.Cs.manager
	}
	return nil
}
