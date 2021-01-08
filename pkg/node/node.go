package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/enix/dothill-storage-controller/pkg/common"
	"github.com/kubernetes-csi/csi-lib-iscsi/iscsi"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// Driver is the implementation of csi.NodeServer
type Driver struct {
	semaphore   *semaphore.Weighted
	kubeletPath string
}

// NewDriver is a convenience function for creating a node driver
func NewDriver(kubeletPath string) *Driver {
	if klog.V(8) {
		iscsi.EnableDebugLogging(os.Stderr)
	}

	return &Driver{
		semaphore:   semaphore.NewWeighted(1),
		kubeletPath: kubeletPath,
	}
}

// NewServerInterceptors implements DriverImpl.NewServerInterceptors
func (driver *Driver) NewServerInterceptors(logRoutineServerInterceptor grpc.UnaryServerInterceptor) *[]grpc.UnaryServerInterceptor {
	serverInterceptors := []grpc.UnaryServerInterceptor{
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if info.FullMethod == "/csi.v1.Node/NodePublishVolume" {
				if !driver.semaphore.TryAcquire(1) {
					return nil, status.Error(codes.Aborted, "node busy: too many concurrent volume publication, try again later")
				}
				defer driver.semaphore.Release(1)
			}
			return handler(ctx, req)
		},
		logRoutineServerInterceptor,
	}

	return &serverInterceptors
}

// ShouldLogRoutine implements DriverImpl.ShouldLogRoutine
func (driver *Driver) ShouldLogRoutine(fullMethod string) bool {
	return fullMethod == "/csi.v1.Node/NodePublishVolume" || fullMethod == "/csi.v1.Node/NodeUnpublishVolume"
}

// NodeGetInfo returns info about the node
func (driver *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	initiatorName, err := readInitiatorName()
	if err != nil {
		return nil, err
	}

	return &csi.NodeGetInfoResponse{
		NodeId:            initiatorName,
		MaxVolumesPerNode: 255,
	}, nil
}

// NodeGetCapabilities returns the supported capabilities of the node server
func (driver *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	var csc []*csi.NodeServiceCapability
	cl := []csi.NodeServiceCapability_RPC_Type{
		// csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	}

	for _, cap := range cl {
		klog.V(4).Infof("enabled node service capability: %v", cap.String())
		csc = append(csc, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: csc}, nil
}

// NodePublishVolume mounts the volume mounted to the staging path to the target path
func (driver *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume with empty id")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume at an empty path")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume without capabilities")
	}

	klog.Infof("publishing volume %s", req.GetVolumeId())

	portals := strings.Split(req.GetVolumeContext()[common.PortalsConfigKey], ",")
	klog.Infof("ISCSI portals: %s", portals)

	lun, _ := strconv.ParseInt(req.GetPublishContext()["lun"], 10, 32)
	klog.Infof("LUN: %d", lun)

	klog.Info("initiating ISCSI connection...")
	targets := make([]iscsi.TargetInfo, 0)
	for _, portal := range portals {
		targets = append(targets, iscsi.TargetInfo{
			Iqn:    req.GetVolumeContext()[common.TargetIQNConfigKey],
			Portal: portal,
		})
	}
	connector := &iscsi.Connector{
		Targets:     targets,
		Lun:         int32(lun),
		DoDiscovery: true,
	}
	path, err := iscsi.Connect(connector)
	if err != nil {
		return nil, err
	}
	klog.Infof("attached device at %s", path)

	if len(connector.Devices) > 1 {
		klog.Info("device is using multipath")
	} else {
		klog.Info("device is NOT using multipath")
	}

	fsType := req.GetVolumeContext()[common.FsTypeConfigKey]
	err = ensureFsType(fsType, path)
	if err != nil {
		return nil, err
	}

	if err = checkFs(path); err != nil {
		return nil, fmt.Errorf("Filesystem seems to be corrupted: %v", err)
	}

	klog.Infof("mounting volume at %s", req.GetTargetPath())
	os.Mkdir(req.GetTargetPath(), 00755)
	out, err := exec.Command("mount", "-t", fsType, path, req.GetTargetPath()).CombinedOutput()
	if err != nil {
		return nil, errors.New(string(out))
	}

	iscsiInfoPath := fmt.Sprintf("%s/plugins/%s/iscsi-%s.json", driver.kubeletPath, common.PluginName, req.GetVolumeId())
	klog.Infof("saving ISCSI connection info in %s", iscsiInfoPath)
	err = iscsi.PersistConnector(connector, iscsiInfoPath)
	if err != nil {
		return nil, err
	}

	klog.Infof("succesfully mounted volume at %s", req.GetTargetPath())
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the volume from the target path
func (driver *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with empty id")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume at an empty path")
	}

	klog.Infof("unpublishing volume %s", req.GetVolumeId())

	_, err := os.Stat(req.GetTargetPath())
	if err == nil {
		klog.Infof("unmounting volume at %s", req.GetTargetPath())
		out, err := exec.Command("mountpoint", req.GetTargetPath()).CombinedOutput()
		if err == nil {
			out, err := exec.Command("umount", req.GetTargetPath()).CombinedOutput()
			if err != nil && !os.IsNotExist(err) {
				return nil, errors.New(string(out))
			}
		} else {
			klog.Warningf("assuming that volume is already unmounted: %s", out)
		}

		os.Remove(req.GetTargetPath())
	}

	iscsiInfoPath := fmt.Sprintf("%s/plugins/%s/iscsi-%s.json", driver.kubeletPath, common.PluginName, req.GetVolumeId())
	klog.Infof("loading ISCSI connection info from %s", iscsiInfoPath)
	connector, err := iscsi.GetConnectorFromFile(iscsiInfoPath)
	if err != nil {
		klog.Warning(errors.Wrap(err, "assuming that ISCSI connection is already closed"))
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	klog.Info("detaching ISCSI device")
	err = iscsi.DisconnectVolume(connector)
	if err != nil {
		return nil, err
	}

	klog.Infof("deleting ISCSI connection info file %s", iscsiInfoPath)
	os.Remove(iscsiInfoPath)

	klog.Info("successfully detached ISCSI device")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume finalizes volume expansion on the node
func (driver *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	fmt.Println("NodeExpandVolume call")
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume unimplemented yet")
}

// NodeGetVolumeStats return info about a given volume
// Will not be called as the plugin does not have the GET_VOLUME_STATS capability
func (driver *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is unimplemented and should not be called")
}

// NodeStageVolume mounts the volume to a staging path on the node. This is
// called by the CO before NodePublishVolume and is used to temporary mount the
// volume to a staging path. Once mounted, NodePublishVolume will make sure to
// mount it to the appropriate path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (driver *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume is unimplemented and should not be called")
}

// NodeUnstageVolume unstages the volume from the staging path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (driver *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume is unimplemented and should not be called")
}

func checkFs(path string) error {
	klog.Infof("Checking filesystem at %s", path)
	if out, err := exec.Command("e2fsck", "-n", path).CombinedOutput(); err != nil {
		return errors.New(string(out))
	}
	return nil
}

// see https://github.com/kubernetes-csi/driver-registrar/blob/795af1899f3c94dd0c6dda2a25ed301123479bb9/vendor/k8s.io/kubernetes/pkg/util/mount/mount_linux.go#L543
func getDiskFormat(disk string) (string, error) {
	args := []string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", disk}
	klog.V(2).Infof("Attempting to determine if disk %q is formatted using blkid with args: (%v)", disk, args)
	output, err := exec.Command("blkid", args...).CombinedOutput()
	klog.V(2).Infof("Output: %q, err: %v", output, err)

	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if exit.ExitCode() == 2 {
				klog.V(2).Infof("Disk device is unformatted (%v)", err)
				// Disk device is unformatted.
				// For `blkid`, if the specified token (TYPE/PTTYPE, etc) was
				// not found, or no (specified) devices could be identified, an
				// exit code of 2 is returned.
				return "", nil
			}
		}
		return "", fmt.Errorf("Could not determine if disk %q is formatted (%v)", disk, err)
	}

	var fsType, ptType string

	re := regexp.MustCompile(`([A-Z]+)="?([^"\n]+)"?`) // Handles alpine and debian outputs
	matches := re.FindAllSubmatch(output, -1)
	for _, match := range matches {
		if len(match) != 3 {
			return "", fmt.Errorf("blkid returns invalid output: %s", output)
		}
		// TYPE is filesystem type, and PTTYPE is partition table type, according
		// to https://www.kernel.org/pub/linux/utils/util-linux/v2.21/libblkid-docs/.
		if string(match[1]) == "TYPE" {
			fsType = string(match[2])
		} else if string(match[1]) == "PTTYPE" {
			ptType = string(match[2])
		}
	}

	if len(ptType) > 0 {
		klog.V(2).Infof("Disk %s detected partition table type: %s", ptType)
		// Returns a special non-empty string as filesystem type, then kubelet
		// will not format it.
		return "unknown data, probably partitions", nil
	}

	return fsType, nil
}

func ensureFsType(fsType string, disk string) error {
	currentFsType, err := getDiskFormat(disk)

	if err != nil {
		return err
	}

	klog.V(1).Infof("Detected filesystem: %q", currentFsType)
	if currentFsType != fsType {
		if currentFsType != "" {
			return fmt.Errorf("Could not create %s filesystem on device %s since it already has one (%s)", fsType, disk, currentFsType)
		}

		klog.Infof("Creating %s filesystem on device %s", fsType, disk)
		out, err := exec.Command(fmt.Sprintf("mkfs.%s", fsType), disk).CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
	}

	return nil
}

func readInitiatorName() (string, error) {
	initiatorNameFilePath := "/etc/iscsi/initiatorname.iscsi"
	file, err := os.Open(initiatorNameFilePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if equal := strings.Index(line, "="); equal >= 0 {
			if strings.TrimSpace(line[:equal]) == "InitiatorName" {
				return strings.TrimSpace(line[equal+1:]), nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("InitiatorName key is missing from %s", initiatorNameFilePath)
}
