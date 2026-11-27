package driver

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"
)

var _ = ginkgo.Describe("NodeServer", func() {
	var (
		ns  *NodeServer
		drv *Driver
	)

	ginkgo.BeforeEach(func() {
		drv = NewDriver(&DriverOptions{
			DriverName: "zerofs.csi.sorend.github.com",
			NodeID:     "test-node",
		})
		ns = NewNodeServer(drv)
	})

	ginkgo.Context("NodeGetCapabilities", func() {
		ginkgo.It("should return capabilities", func() {
			resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.Capabilities).NotTo(gomega.BeEmpty())

			capTypes := make(map[csi.NodeServiceCapability_RPC_Type]bool)
			for _, cap := range resp.Capabilities {
				if rpc := cap.GetRpc(); rpc != nil {
					capTypes[rpc.GetType()] = true
				}
			}
			gomega.Expect(capTypes[csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME]).To(gomega.BeTrue())
			gomega.Expect(capTypes[csi.NodeServiceCapability_RPC_EXPAND_VOLUME]).To(gomega.BeTrue())
			gomega.Expect(capTypes[csi.NodeServiceCapability_RPC_GET_VOLUME_STATS]).To(gomega.BeTrue())
		})
	})

	ginkgo.Context("NodeGetInfo", func() {
		ginkgo.It("should return node info with configured node ID", func() {
			resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.NodeId).To(gomega.Equal("test-node"))
			gomega.Expect(resp.MaxVolumesPerNode).To(gomega.Equal(int64(1000)))
		})

		ginkgo.It("should return hostname as node ID when not configured", func() {
			ns.driver.options.NodeID = ""
			resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			hostname, _ := os.Hostname()
			gomega.Expect(resp.NodeId).To(gomega.Equal(hostname))
		})
	})

	ginkgo.Context("NodeStageVolume", func() {
		ginkgo.DescribeTable("should use a stable 9P mount source",
			func(addresses map[string]string, expected, lookupHost string) {
				fakeMounter := mount.NewFakeMounter(nil)
				ns.mounter.Interface = fakeMounter
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lookups := 0
				ns.lookupIP = func(lookupCtx context.Context, network, host string) ([]net.IP, error) {
					lookups++
					gomega.Expect(lookupHost).NotTo(gomega.BeEmpty(), "numeric mount sources should not require DNS")
					gomega.Expect(lookupCtx).To(gomega.Equal(ctx))
					gomega.Expect(network).To(gomega.Equal("ip4"))
					gomega.Expect(host).To(gomega.Equal(lookupHost))
					return []net.IP{net.ParseIP("10.43.0.3")}, nil
				}
				stagingPath, err := os.MkdirTemp("", "zerofs-source-")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer os.RemoveAll(stagingPath)
				volumeContext := mergeMapCopy(addresses)
				volumeContext["protocol"] = "ninep"
				volumeContext["port"] = "5564"
				_, err = ns.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
					VolumeId:          "test-volume",
					StagingTargetPath: stagingPath,
					VolumeContext:     volumeContext,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
						AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
					},
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mounts, err := fakeMounter.List()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(mounts).To(gomega.HaveLen(1))
				gomega.Expect(mounts[0].Device).To(gomega.Equal(expected))
				if lookupHost == "" {
					gomega.Expect(lookups).To(gomega.BeZero())
				} else {
					gomega.Expect(lookups).To(gomega.Equal(1))
				}
			},
			ginkgo.Entry("Service IP wins over a stale Pod IP and hostname",
				map[string]string{"serverIP": "10.43.0.1", "podIP": "10.42.0.99", "server": "zerofs.example.invalid"}, "10.43.0.1", ""),
			ginkgo.Entry("Service IP works without a Pod IP",
				map[string]string{"serverIP": "10.43.0.1"}, "10.43.0.1", ""),
			ginkgo.Entry("server address wins over a stale Pod IP when Service IP is absent",
				map[string]string{"server": "10.43.0.2", "podIP": "10.42.0.99"}, "10.43.0.2", ""),
			ginkgo.Entry("Service DNS is resolved before mounting even with a stale Pod IP",
				map[string]string{"server": "zerofs.example.invalid", "podIP": "10.42.0.99"}, "10.43.0.3", "zerofs.example.invalid"),
			ginkgo.Entry("hostname-only volumes resolve without a Pod IP",
				map[string]string{"server": "zerofs.example.invalid"}, "10.43.0.3", "zerofs.example.invalid"),
			ginkgo.Entry("legacy Pod-IP-only volumes still mount",
				map[string]string{"podIP": "10.42.0.1"}, "10.42.0.1", ""),
		)

		ginkgo.DescribeTable("should not fall back to a stale Pod IP when Service DNS fails",
			func(lookupErr error, expectedCode codes.Code) {
				fakeMounter := mount.NewFakeMounter(nil)
				ns.mounter.Interface = fakeMounter
				ns.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
					return nil, lookupErr
				}
				_, err := ns.stageNinepVolume(context.Background(), "test-volume", "zerofs.example.invalid", "5564", "/", "/stage",
					map[string]string{"podIP": "10.42.0.99"}, nil)
				gomega.Expect(status.Code(err)).To(gomega.Equal(expectedCode))
				gomega.Expect(fakeMounter.GetLog()).To(gomega.BeEmpty())
			},
			ginkgo.Entry("lookup failure", errors.New("DNS unavailable"), codes.Internal),
			ginkgo.Entry("empty answer", nil, codes.Internal),
			ginkgo.Entry("cancellation", context.Canceled, codes.Canceled),
			ginkgo.Entry("deadline", context.DeadlineExceeded, codes.DeadlineExceeded),
		)

		ginkgo.It("should reject a 9P volume without any server address", func() {
			fakeMounter := mount.NewFakeMounter(nil)
			ns.mounter.Interface = fakeMounter
			_, err := ns.stageNinepVolume(context.Background(), "test-volume", "", "5564", "/", "/stage", nil, nil)
			gomega.Expect(status.Code(err)).To(gomega.Equal(codes.InvalidArgument))
			gomega.Expect(fakeMounter.GetLog()).To(gomega.BeEmpty())
		})

		ginkgo.DescribeTable("should pass effective mount options to the mounter",
			func(protocol string, volumeOptions string, flags, expected []string) {
				fakeMounter := mount.NewFakeMounter(nil)
				ns.mounter.Interface = fakeMounter
				stagingPath, err := os.MkdirTemp("", "zerofs-stage-")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				defer os.RemoveAll(stagingPath)

				port := "2049"
				if protocol == "ninep" {
					port = "5564"
				}
				_, err = ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
					VolumeId:          "test-volume",
					StagingTargetPath: stagingPath,
					VolumeContext: map[string]string{
						"protocol":     protocol,
						"serverIP":     "10.0.0.1",
						"port":         port,
						"mountOptions": volumeOptions,
					},
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
					},
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				mounts, err := fakeMounter.List()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(mounts).To(gomega.HaveLen(1))
				gomega.Expect(mounts[0].Opts).To(gomega.ConsistOf(expected))
			},
			ginkgo.Entry("9P defaults support shared writable mmap", "ninep", "", nil,
				[]string{"trans=tcp", "port=5564", "version=9p2000.L", "msize=65536", "cache=mmap"}),
			ginkgo.Entry("StorageClass flags override 9P defaults without dropping the port", "ninep", "",
				[]string{"msize=262144", "cache=none", "ro"},
				[]string{"trans=tcp", "port=5564", "version=9p2000.L", "msize=262144", "cache=none", "ro"}),
			ginkgo.Entry("legacy volume options retain required 9P defaults", "ninep", "msize=131072", nil,
				[]string{"trans=tcp", "port=5564", "version=9p2000.L", "msize=131072", "cache=mmap"}),
			ginkgo.Entry("CSI flags take precedence over legacy options", "ninep", "cache=loose,msize=131072",
				[]string{"cache=mmap", "msize=65536", "trans=tcp"},
				[]string{"trans=tcp", "port=5564", "version=9p2000.L", "msize=65536", "cache=mmap"}),
			ginkgo.Entry("NFS defaults are unchanged", "nfs", "", nil,
				[]string{"nolock", "vers=3", "tcp", "port=2049", "mountport=2049", "soft", "timeo=30", "retrans=2"}),
			ginkgo.Entry("StorageClass flags are honored for NFS too", "nfs", "timeo=60", []string{"timeo=90", "ro"},
				[]string{"nolock", "vers=3", "tcp", "port=2049", "mountport=2049", "soft", "timeo=90", "retrans=2", "ro"}),
		)

		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when staging target path is empty", func() {
			_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
				VolumeId: "test-volume",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})
	})

	ginkgo.Context("NodeUnstageVolume", func() {
		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when staging target path is empty", func() {
			_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
				VolumeId: "test-volume",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})
	})

	ginkgo.Context("NodePublishVolume", func() {
		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when staging target path is empty", func() {
			_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: "test-volume",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when target path is empty", func() {
			_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId:          "test-volume",
				StagingTargetPath: "/tmp/staging",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})
	})

	ginkgo.Context("NodeUnpublishVolume", func() {
		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when target path is empty", func() {
			_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
				VolumeId: "test-volume",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})
	})

	ginkgo.Context("NodeGetVolumeStats", func() {
		ginkgo.It("should return error when volume path is empty", func() {
			_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
				VolumePath: "/tmp",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when volume path does not exist", func() {
			_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
				VolumeId:   "test-volume",
				VolumePath: "/nonexistent/path",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.NotFound))
		})

		ginkgo.It("should return stats for existing directory", func() {
			tmpDir := filepath.Join(os.TempDir(), "zerofs-test-stats")
			os.MkdirAll(tmpDir, 0755)
			defer os.RemoveAll(tmpDir)

			resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
				VolumeId:   "test-volume",
				VolumePath: tmpDir,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.Usage).NotTo(gomega.BeEmpty())
		})
	})

	ginkgo.Context("NodeExpandVolume", func() {
		ginkgo.It("should return error when volume ID is empty", func() {
			_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when volume path is empty", func() {
			_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
				VolumeId: "test-volume",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})

		ginkgo.It("should return error when capacity range is empty", func() {
			_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
				VolumeId:   "test-volume",
				VolumePath: "/tmp",
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			st, ok := status.FromError(err)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(st.Code()).To(gomega.Equal(codes.InvalidArgument))
		})
	})
})
