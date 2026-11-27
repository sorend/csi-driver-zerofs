package driver

import (
	"context"
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
