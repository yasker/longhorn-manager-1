package csi

import (
	"context"
	"fmt"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	putil "sigs.k8s.io/sig-storage-lib-external-provisioner/util"

	longhornclient "github.com/longhorn/longhorn-manager/client"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"
)

const (
	timeoutAttachDetach = 120 * time.Second
	tickAttachDetach    = 2 * time.Second
)

type ControllerServer struct {
	*csicommon.DefaultControllerServer
	apiClient *longhornclient.RancherClient
}

func NewControllerServer(d *csicommon.CSIDriver, apiClient *longhornclient.RancherClient) *ControllerServer {
	return &ControllerServer{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d),
		apiClient:               apiClient,
	}
}

func (cs *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	logrus.Infof("ControllerServer create volume req: %v", req)
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		logrus.Errorf("CreateVolume: invalid create volume req: %v", req)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Check sanity of request Name, Volume Capabilities
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume Name cannot be empty")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities cannot be empty")
	}

	// check for already existing volume name
	// ID and name are same in longhorn API
	existVol, err := cs.apiClient.Volume.ById(req.GetName())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existVol != nil && existVol.Name == req.GetName() {
		logrus.Debugf("CreateVolume: got an exist volume: %s", existVol.Name)
		exVolSize, err := util.ConvertSize(existVol.Size)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				Id:            existVol.Id,
				CapacityBytes: exVolSize,
				Attributes:    req.GetParameters(),
			},
		}, nil
	}

	vol, err := getVolumeOptions(req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	vol.Name = req.Name

	volSizeBytes := int64(putil.GiB)
	if req.GetCapacityRange() != nil {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	volSizeGiB := putil.RoundUpToGiB(volSizeBytes)
	vol.Size = fmt.Sprintf("%dGi", volSizeGiB)

	logrus.Infof("CreateVolume: creating a volume by API client, name: %s, size: %s", vol.Name, vol.Size)
	resVol, err := cs.apiClient.Volume.Create(vol)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if !cs.waitForVolumeState(resVol.Id, types.VolumeStateDetached, true, false) {
		return nil, status.Error(codes.Internal, "cannot wait for volume creation to complete")
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            resVol.Id,
			CapacityBytes: int64(volSizeGiB * putil.GiB),
			Attributes:    req.GetParameters(),
		},
	}, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	logrus.Infof("ControllerServer delete volume req: %v", req)
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		logrus.Errorf("DeleteVolume: invalid delete volume req: %v", req)
		return nil, status.Error(codes.Internal, err.Error())
	}

	existVol, err := cs.apiClient.Volume.ById(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if existVol == nil {
		logrus.Warnf("DeleteVolume: volume %s not exists", req.GetVolumeId())
		return &csi.DeleteVolumeResponse{}, nil
	}

	logrus.Debugf("DeleteVolume: volume %s exists", req.GetVolumeId())
	if err = cs.apiClient.Volume.Delete(existVol); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	logrus.Infof("ControllerServer ValidateVolumeCapabilities req: %v", req)
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{Supported: false, Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

// ControllerPublishVolume will attach the volume to the specified node
func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	logrus.Infof("ControllerServer ControllerPublishVolume req: %v", req)
	existVol, err := cs.apiClient.Volume.ById(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existVol == nil {
		msg := fmt.Sprintf("ControllerPublishVolume: the volume %s not exists", req.GetVolumeId())
		logrus.Warn(msg)
		return nil, status.Error(codes.NotFound, msg)
	}

	if existVol.InitialRestorationRequired {
		return nil, status.Errorf(codes.Aborted, "The volume %s is restoring backup", req.GetVolumeId())
	}

	if existVol.State == string(types.VolumeStateAttaching) || existVol.State == string(types.VolumeStateDetaching) {
		return nil, status.Errorf(codes.Aborted, "The volume %s is %s", req.GetVolumeId(), existVol.State)
	}

	needToAttach := true
	if existVol.State == string(types.VolumeStateAttached) {
		needToAttach = false
	}

	logrus.Debugf("ControllerPublishVolume: current nodeID %s", req.GetNodeId())
	if needToAttach {
		// attach longhorn volume with frontend enabled
		input := &longhornclient.AttachInput{
			HostId:          req.GetNodeId(),
			DisableFrontend: false,
		}
		existVol, err = cs.apiClient.Volume.ActionAttach(existVol, input)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		logrus.Infof("ControllerPublishVolume: no need to attach volume %s", req.GetVolumeId())
	}

	if !cs.waitForVolumeState(req.GetVolumeId(), types.VolumeStateAttached, false, false) {
		return nil, status.Errorf(codes.Aborted, "Attaching volume %s failed", req.GetVolumeId())
	}
	logrus.Debugf("Volume %s attached on %s", req.GetVolumeId(), req.GetNodeId())

	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume will detach the volume
func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	logrus.Infof("ControllerServer ControllerUnpublishVolume req: %v", req)

	existVol, err := cs.apiClient.Volume.ById(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existVol == nil {
		msg := fmt.Sprintf("ControllerPublishVolume: the volume %s not exists", req.GetVolumeId())
		logrus.Warn(msg)
		return nil, status.Error(codes.NotFound, msg)
	}
	if existVol.State == string(types.VolumeStateDetaching) {
		return nil, status.Errorf(codes.Aborted, "The volume %s is detaching", req.GetVolumeId())
	}

	needToDetach := false
	if existVol.State == string(types.VolumeStateAttached) || existVol.State == string(types.VolumeStateAttaching) {
		needToDetach = true
	}

	if needToDetach {
		// detach longhorn volume
		_, err = cs.apiClient.Volume.ActionDetach(existVol)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	if !cs.waitForVolumeState(req.GetVolumeId(), types.VolumeStateDetached, false, true) {
		return nil, status.Errorf(codes.Aborted, "Detaching volume %s failed", req.GetVolumeId())
	}
	logrus.Debugf("Volume %s detached on %s", req.GetVolumeId(), req.GetNodeId())

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *ControllerServer) waitForVolumeState(volumeID string, state types.VolumeState, notFoundRetry, notFoundReturn bool) bool {
	timeout := time.After(timeoutAttachDetach)
	tick := time.Tick(tickAttachDetach)
	for {
		select {
		case <-timeout:
			logrus.Warnf("waitForVolumeState: timeout to wait for volume %s become %s", volumeID, state)
			return false
		case <-tick:
			logrus.Debugf("Polling %s state for %s at %s", volumeID, state, time.Now().String())
			existVol, err := cs.apiClient.Volume.ById(volumeID)
			if err != nil {
				logrus.Warnf("waitForVolumeState: wait for %s state %s: %s", volumeID, state, err)
				continue
			}
			if existVol == nil {
				logrus.Warnf("waitForVolumeState: volume %s not exist", volumeID)
				if notFoundRetry {
					continue
				}
				return notFoundReturn
			}
			if existVol.State == string(state) {
				return true
			}
		}
	}
}
