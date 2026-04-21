/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package dra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	k8sv1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	// TODO: Replace with k8s.io types when KEP-5304 is implemented in kubernetes
	"kubevirt.io/kubevirt/pkg/dra/metadata"
)

// IsGPUDRA returns true if the GPU is a DRA GPU
func IsGPUDRA(gpu v1.GPU) bool {
	return gpu.DeviceName == "" && gpu.ClaimRequest != nil
}

// IsHostDeviceDRA returns true if the HostDevice is a DRA HostDevice
func IsHostDeviceDRA(hd v1.HostDevice) bool {
	return hd.DeviceName == "" && hd.ClaimRequest != nil
}

// DownwardAPIAttributes reads DRA device metadata files (*-metadata.json) that will be
// automatically provided by dra driver framework KEP-5304 is implemented and consumed by drivers.
// See: kubernetes/enhancements#5304
const (
	DefaultMetadataBasePath = "/var/run/kubernetes.io/dra-device-attributes"
)

// GetPCIAddressForClaim returns the PCI address for a device in the given claim and request.
// It lazily reads the KEP-5304 metadata file at lookup time.
func GetPCIAddressForClaim(basePath string, resourceClaims []k8sv1.PodResourceClaim, claimRefName, requestName string) (string, error) {
	device, err := resolveDevice(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return "", err
	}

	if attr, ok := device.Attributes[metadata.PCIBusIDAttribute]; ok {
		if attr.StringValue != nil && *attr.StringValue != "" {
			return *attr.StringValue, nil
		}
	}
	return "", fmt.Errorf("pciBusID not found for claim %q request %q", claimRefName, requestName)
}

// GetNUMANodeForClaim returns the NUMA node for a device in the given claim and request.
// This reads from KEP-5304 metadata, providing device NUMA topology for guest mapping.
func GetNUMANodeForClaim(basePath string, resourceClaims []k8sv1.PodResourceClaim, claimRefName, requestName string) (int64, error) {
	device, err := resolveDevice(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return -1, err
	}

	numaAttrName := resourcev1.QualifiedName("numaNode")
	if attr, ok := device.Attributes[numaAttrName]; ok {
		if attr.IntValue != nil {
			return *attr.IntValue, nil
		}
	}
	return -1, fmt.Errorf("numaNode not found for claim %q request %q", claimRefName, requestName)
}

// GetMDevUUIDForClaim returns the mdev UUID for a device in the given claim and request.
// It lazily reads the KEP-5304 metadata file at lookup time.
func GetMDevUUIDForClaim(basePath string, resourceClaims []k8sv1.PodResourceClaim, claimRefName, requestName string) (string, error) {
	device, err := resolveDevice(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return "", err
	}

	if attr, ok := device.Attributes[metadata.MDevUUIDAttribute]; ok {
		if attr.StringValue != nil && *attr.StringValue != "" {
			return *attr.StringValue, nil
		}
	}
	return "", fmt.Errorf("mdevUUID not found for claim %q request %q", claimRefName, requestName)
}

// resolveDevice finds and reads the metadata file for a specific claim ref and
// request, returning the single device from that request.
func resolveDevice(basePath string, resourceClaims []k8sv1.PodResourceClaim, claimRefName, requestName string) (*metadata.Device, error) {
	md, err := resolveClaimMetadata(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return nil, err
	}

	for _, req := range md.Requests {
		if req.Name == requestName {
			if len(req.Devices) == 0 {
				return nil, fmt.Errorf("request %q has no devices", requestName)
			}
			if len(req.Devices) > 1 {
				return nil, fmt.Errorf("request %q has %d devices but KubeVirt only supports exactly one device per request (count > 1 is not supported)", requestName, len(req.Devices))
			}
			return &req.Devices[0], nil
		}
	}
	return nil, fmt.Errorf("request %q not found in metadata for claim %q (available requests: %v)", requestName, md.Name, metadataRequestNames(md))
}

// resolveClaimMetadata reads the metadata file for a claim ref + request pair.
// For direct claims it constructs the exact path; for template claims it
// searches by PodClaimName.
// KEP-5304 container path: {base}/{resourceclaims|resourceclaimtemplates}/{claimName}/{requestName}/{driverName}-metadata.json
func resolveClaimMetadata(basePath string, resourceClaims []k8sv1.PodResourceClaim, claimRefName, requestName string) (*metadata.DeviceMetadata, error) {
	// KEP-5304 places metadata under resourceclaims/ for direct claims and
	// resourceclaimtemplates/ for template-generated claims. Try both paths.
	subdirs := []string{"resourceclaims", "resourceclaimtemplates"}
	for _, rc := range resourceClaims {
		if rc.Name != claimRefName {
			continue
		}
		if rc.ResourceClaimName != nil && *rc.ResourceClaimName != "" {
			for _, subdir := range subdirs {
				md, err := readMetadataFromDir(filepath.Join(basePath, subdir), *rc.ResourceClaimName, requestName)
				if err == nil {
					return md, nil
				}
			}
			return nil, fmt.Errorf("metadata not found for direct claim %q request %q", *rc.ResourceClaimName, requestName)
		}
		for _, subdir := range subdirs {
			md, err := findMetadataByPodClaimName(filepath.Join(basePath, subdir), rc.Name, requestName)
			if err == nil {
				return md, nil
			}
		}
		return nil, fmt.Errorf("metadata not found for template claim %q request %q", rc.Name, requestName)
	}
	return nil, fmt.Errorf("metadata not found for claim %q", claimRefName)
}

// readMetadataFromDir reads the metadata file at the exact path for a direct claim.
func readMetadataFromDir(basePath, claimName, requestName string) (*metadata.DeviceMetadata, error) {
	pattern := filepath.Join(basePath, claimName, requestName, "*-metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob for metadata file: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("metadata not found for claim %q request %q", claimName, requestName)
	}
	md, err := readMetadataFile(matches[0])
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for claim %q request %q: %w", claimName, requestName, err)
	}
	return md, nil
}

// findMetadataByPodClaimName searches for a template-generated claim's metadata
// file by matching PodClaimName, scoped to the given request name.
func findMetadataByPodClaimName(basePath, podClaimName, requestName string) (*metadata.DeviceMetadata, error) {
	pattern := filepath.Join(basePath, "*", requestName, "*-metadata.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob for metadata file: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no metadata file found for templateclaim %q request %q", podClaimName, requestName)
	}
	for _, file := range matches {
		md, err := readMetadataFile(file)
		if err != nil {
			log.Log.Reason(err).Warningf("Skipping metadata file %s", file)
			continue
		}
		if md.PodClaimName != nil && *md.PodClaimName == podClaimName {
			return md, nil
		}
	}
	return nil, fmt.Errorf("no metadata file found with matching with pod claim name %q request %q", podClaimName, requestName)
}

func metadataRequestNames(md *metadata.DeviceMetadata) []string {
	names := make([]string, 0, len(md.Requests))
	for _, req := range md.Requests {
		names = append(names, req.Name)
	}
	return names
}

func readMetadataFile(path string) (*metadata.DeviceMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading metadata file %q: %w", path, err)
	}
	var md metadata.DeviceMetadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("parsing metadata file %q: %w", path, err)
	}
	return &md, nil
}
