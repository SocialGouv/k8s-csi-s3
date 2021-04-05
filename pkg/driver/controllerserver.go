/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/ctrox/csi-s3/pkg/mounter"
	"github.com/ctrox/csi-s3/pkg/s3"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

const (
	defaultFsPrefix = "csi-fs"
)

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	params := req.GetParameters()

	volumeID := sanitizeVolumeID(req.GetName())
	if bucketName, bucketExists := params[mounter.BucketKey]; bucketExists {
		volumeID = sanitizeVolumeID(bucketName)
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	capacityBytes := int64(req.GetCapacityRange().GetRequiredBytes())

	mounter := params[mounter.TypeKey]

	glog.V(4).Infof("Got a request to create volume %s", volumeID)
	client, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := client.BucketExists(volumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to check if bucket %s exists: %v", volumeID, err)
	}
	var b *s3.Bucket
	if exists {
		b, err = client.GetBucket(volumeID)

		if err != nil {
			glog.Warningf("Bucket %s exists, but failed to get its metadata: %v", volumeID, err)
			b = &s3.Bucket{
				Name:          volumeID,
				Mounter:       mounter,
				CapacityBytes: capacityBytes,
				FSPath:        "",
				CreatedByCsi:  false,
			}
		} else {
			// Check if volume capacity requested is bigger than the already existing capacity
			if capacityBytes > b.CapacityBytes {
				return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with smaller size already exist", volumeID))
			}
			b.Mounter = mounter
		}
	} else {
		if err = client.CreateBucket(volumeID); err != nil {
			return nil, fmt.Errorf("failed to create volume %s: %v", volumeID, err)
		}
		if err = client.CreatePrefix(volumeID, defaultFsPrefix); err != nil {
			return nil, fmt.Errorf("failed to create prefix %s: %v", defaultFsPrefix, err)
		}
		b = &s3.Bucket{
			Name:          volumeID,
			Mounter:       mounter,
			CapacityBytes: capacityBytes,
			FSPath:        defaultFsPrefix,
			CreatedByCsi:  !exists,
		}
	}
	if err := client.SetBucket(b); err != nil {
		return nil, fmt.Errorf("Error setting bucket metadata: %v", err)
	}

	glog.V(4).Infof("create volume %s", volumeID)
	s3Vol := s3Volume{}
	s3Vol.VolName = volumeID
	s3Vol.VolID = volumeID
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: capacityBytes,
			VolumeContext: req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("Invalid delete volume req: %v", req)
		return nil, err
	}
	glog.V(4).Infof("Deleting volume %s", volumeID)

	client, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := client.BucketExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		b, err := client.GetBucket(volumeID)
		if err != nil {
			return nil, fmt.Errorf("Failed to get metadata of buckect %s", volumeID)
		}
		if b.CreatedByCsi {
			if err := client.RemoveBucket(volumeID); err != nil {
				glog.V(3).Infof("Failed to remove volume %s: %v", volumeID, err)
				return nil, err
			}
			glog.V(4).Infof("Bucket %s removed", volumeID)
		} else {
			glog.V(4).Infof("Bucket %s is not created by csi-s3, will not be deleted by csi-s3 automatically.", volumeID)
		}
	} else {
		glog.V(5).Infof("Bucket %s does not exist, ignoring request", volumeID)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	s3, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := s3.BucketExists(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if !exists {
		// return an error if the volume requested does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %s does not exist", req.GetVolumeId()))
	}

	// We currently only support RWO
	supportedAccessMode := &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "Only single node writer is supported"}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessMode: supportedAccessMode,
				},
			},
		},
	}, nil
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return &csi.ControllerExpandVolumeResponse{}, status.Error(codes.Unimplemented, "ControllerExpandVolume is not implemented")
}

func sanitizeVolumeID(volumeID string) string {
	volumeID = strings.ToLower(volumeID)
	if len(volumeID) > 63 {
		h := sha1.New()
		io.WriteString(h, volumeID)
		volumeID = hex.EncodeToString(h.Sum(nil))
	}
	return volumeID
}