/*
Copyright 2016 The Kubernetes Authors.

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

package cinder

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

type cinderDiskAttacher struct {
	host           volume.VolumeHost
	cinderProvider CinderProvider
}

var _ volume.Attacher = &cinderDiskAttacher{}

var _ volume.AttachableVolumePlugin = &cinderPlugin{}

const (
	checkSleepDuration = time.Second
)

func (plugin *cinderPlugin) NewAttacher() (volume.Attacher, error) {
	cinder, err := getCloudProvider(plugin.host.GetCloudProvider())
	if err != nil {
		return nil, err
	}
	return &cinderDiskAttacher{
		host:           plugin.host,
		cinderProvider: cinder,
	}, nil
}

func (plugin *cinderPlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	mounter := plugin.host.GetMounter()
	return mount.GetMountRefs(mounter, deviceMountPath)
}

func (attacher *cinderDiskAttacher) Attach(spec *volume.Spec, nodeName types.NodeName) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	volumeID := volumeSource.VolumeID

	instanceID, err := attacher.nodeInstanceID(nodeName)
	if err != nil {
		return "", err
	}

	pending, volumeStatus, err := attacher.cinderProvider.OperationPending(volumeID)
	if err != nil {
		return "", err
	}
	if pending {
		glog.Infof("status of volume %q is %s, wait it to be finished", volumeID, volumeStatus)
		return "", nil
	}

	attached, err := attacher.cinderProvider.DiskIsAttached(instanceID, volumeID)
	if err != nil {
		// Log error and continue with attach
		glog.Warningf(
			"Error checking if volume (%q) is already attached to current instance (%q). Will continue and try attach anyway. err=%v",
			volumeID, instanceID, err)
	}

	if err == nil && attached {
		// Volume is already attached to instance.
		glog.Infof("Attach operation is successful. volume %q is already attached to instance %q.", volumeID, instanceID)
	} else {
		_, err = attacher.cinderProvider.AttachDisk(instanceID, volumeID)
		if err == nil {
			glog.Infof("Attach operation successful: volume %q attached to instance %q.", volumeID, instanceID)
		} else {
			glog.Infof("Attach volume %q to instance %q failed with: %v", volumeID, instanceID, err)
			return "", err
		}
	}

	// When volume's status is 'attaching', DiskIsAttached() return <false, nil>, and can't get device path.
	// We should get device path next time and do not return err.
	devicePath := ""
	pending, _, err = attacher.cinderProvider.OperationPending(volumeID)
	if err != nil {
		return "", err
	}
	if !pending {
		devicePath, err = attacher.cinderProvider.GetAttachmentDiskPath(instanceID, volumeID)
		if err != nil {
			glog.Infof("Can not get device path of volume %q which be attached to instance %q, failed with: %v", volumeID, instanceID, err)
			return "", err
		}
	}

	return devicePath, nil
}

func (attacher *cinderDiskAttacher) VolumesAreAttached(specs []*volume.Spec, nodeName types.NodeName) (map[*volume.Spec]bool, error) {
	volumesAttachedCheck := make(map[*volume.Spec]bool)
	volumeSpecMap := make(map[string]*volume.Spec)
	volumeIDList := []string{}
	for _, spec := range specs {
		volumeSource, _, err := getVolumeSource(spec)
		if err != nil {
			glog.Errorf("Error getting volume (%q) source : %v", spec.Name(), err)
			continue
		}

		volumeIDList = append(volumeIDList, volumeSource.VolumeID)
		volumesAttachedCheck[spec] = true
		volumeSpecMap[volumeSource.VolumeID] = spec
	}

	instanceID, err := attacher.nodeInstanceID(nodeName)
	if err != nil {
		return volumesAttachedCheck, err
	}

	attachedResult, err := attacher.cinderProvider.DisksAreAttached(instanceID, volumeIDList)
	if err != nil {
		// Log error and continue with attach
		glog.Errorf(
			"Error checking if Volumes (%v) are already attached to current node (%q). Will continue and try attach anyway. err=%v",
			volumeIDList, nodeName, err)
		return volumesAttachedCheck, err
	}

	for volumeID, attached := range attachedResult {
		if !attached {
			spec := volumeSpecMap[volumeID]
			volumesAttachedCheck[spec] = false
			glog.V(2).Infof("VolumesAreAttached: check volume %q (specName: %q) is no longer attached", volumeID, spec.Name())
		}
	}
	return volumesAttachedCheck, nil
}

func (attacher *cinderDiskAttacher) WaitForAttach(spec *volume.Spec, devicePath string, timeout time.Duration) (string, error) {
	// NOTE: devicePath is is path as reported by Cinder, which may be incorrect and should not be used. See Issue #33128
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	volumeID := volumeSource.VolumeID

	if devicePath == "" {
		return "", fmt.Errorf("WaitForAttach failed for Cinder disk %q: devicePath is empty.", volumeID)
	}

	ticker := time.NewTicker(checkSleepDuration)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		probeAttachedVolume()
		select {
		case <-ticker.C:
			glog.V(5).Infof("Checking Cinder disk %q is attached.", volumeID)
			probeAttachedVolume()
			if !attacher.cinderProvider.ShouldTrustDevicePath() {
				// Using the Cinder volume ID, find the real device path (See Issue #33128)
				devicePath = attacher.cinderProvider.GetDevicePath(volumeID)
			}
			exists, err := volumeutil.PathExists(devicePath)
			if exists && err == nil {
				glog.Infof("Successfully found attached Cinder disk %q at %v.", volumeID, devicePath)
				return devicePath, nil
			} else {
				// Log an error, and continue checking periodically
				glog.Errorf("Error: could not find attached Cinder disk %q (path: %q): %v", volumeID, devicePath, err)
			}
		case <-timer.C:
			return "", fmt.Errorf("Could not find attached Cinder disk %q. Timeout waiting for mount paths to be created.", volumeID)
		}
	}
}

func (attacher *cinderDiskAttacher) GetDeviceMountPath(
	spec *volume.Spec) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	return makeGlobalPDName(attacher.host, volumeSource.VolumeID), nil
}

// FIXME: this method can be further pruned.
func (attacher *cinderDiskAttacher) MountDevice(spec *volume.Spec, devicePath string, deviceMountPath string) error {
	mounter := attacher.host.GetMounter()
	notMnt, err := mounter.IsLikelyNotMountPoint(deviceMountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(deviceMountPath, 0750); err != nil {
				return err
			}
			notMnt = true
		} else {
			return err
		}
	}

	volumeSource, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return err
	}

	options := []string{}
	if readOnly {
		options = append(options, "ro")
	}
	if notMnt {
		diskMounter := &mount.SafeFormatAndMount{Interface: mounter, Runner: exec.New()}
		mountOptions := volume.MountOptionFromSpec(spec, options...)
		err = diskMounter.FormatAndMount(devicePath, deviceMountPath, volumeSource.FSType, mountOptions)
		if err != nil {
			os.Remove(deviceMountPath)
			return err
		}
	}
	return nil
}

type cinderDiskDetacher struct {
	mounter        mount.Interface
	cinderProvider CinderProvider
}

var _ volume.Detacher = &cinderDiskDetacher{}

func (plugin *cinderPlugin) NewDetacher() (volume.Detacher, error) {
	cinder, err := getCloudProvider(plugin.host.GetCloudProvider())
	if err != nil {
		return nil, err
	}
	return &cinderDiskDetacher{
		mounter:        plugin.host.GetMounter(),
		cinderProvider: cinder,
	}, nil
}

func (detacher *cinderDiskDetacher) Detach(deviceMountPath string, nodeName types.NodeName) error {
	volumeID := path.Base(deviceMountPath)
	instances, res := detacher.cinderProvider.Instances()
	if !res {
		return fmt.Errorf("failed to list openstack instances")
	}
	instanceID, err := instances.InstanceID(nodeName)
	if ind := strings.LastIndex(instanceID, "/"); ind >= 0 {
		instanceID = instanceID[(ind + 1):]
	}

	attached, err := detacher.cinderProvider.DiskIsAttached(instanceID, volumeID)
	if err != nil {
		// Log error and continue with detach
		glog.Errorf(
			"Error checking if volume (%q) is already attached to current node (%q). Will continue and try detach anyway. err=%v",
			volumeID, nodeName, err)
	}

	if err == nil && !attached {
		// Volume is already detached from node.
		glog.Infof("detach operation was successful. volume %q is already detached from node %q.", volumeID, nodeName)
		return nil
	}

	if err = detacher.cinderProvider.DetachDisk(instanceID, volumeID); err != nil {
		glog.Errorf("Error detaching volume %q: %v", volumeID, err)
		return err
	}
	glog.Infof("detached volume %q from instance %q", volumeID, instanceID)
	return nil
}

func (detacher *cinderDiskDetacher) UnmountDevice(deviceMountPath string) error {
	return volumeutil.UnmountPath(deviceMountPath, detacher.mounter)
}

func (attacher *cinderDiskAttacher) nodeInstanceID(nodeName types.NodeName) (string, error) {
	instances, res := attacher.cinderProvider.Instances()
	if !res {
		return "", fmt.Errorf("failed to list openstack instances")
	}
	instanceID, err := instances.InstanceID(nodeName)
	if err != nil {
		return "", err
	}
	if ind := strings.LastIndex(instanceID, "/"); ind >= 0 {
		instanceID = instanceID[(ind + 1):]
	}
	return instanceID, nil
}
