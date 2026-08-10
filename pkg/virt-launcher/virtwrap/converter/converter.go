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

package converter

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

/*
 ATTENTION: Rerun code generators when interface signatures are modified.
*/

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	k8sv1 "k8s.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"
	"kubevirt.io/client-go/precond"

	drautil "kubevirt.io/kubevirt/pkg/dra"
	cmdv1 "kubevirt.io/kubevirt/pkg/handler-launcher-com/cmd/v1"
	"kubevirt.io/kubevirt/pkg/os/disk"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/compute"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/iothreads"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/kvm"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/metadata"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/mshv"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/network"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/storage"
	convertertypes "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/types"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/vcpu"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/virtio"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/disksource"
)

type DirectIOChecker interface {
	CheckBlockDevice(path string) (bool, error)
	CheckFile(path string) (bool, error)
}

type directIOChecker struct{}

func NewDirectIOChecker() DirectIOChecker {
	return &directIOChecker{}
}

func (c *directIOChecker) CheckBlockDevice(path string) (bool, error) {
	return c.check(path, syscall.O_RDONLY)
}

func (c *directIOChecker) CheckFile(path string) (bool, error) {
	flags := syscall.O_RDONLY
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// try to create the file and perform the check
		flags = flags | syscall.O_CREAT
		defer os.Remove(path)
	}
	return c.check(path, flags)
}

// based on https://gitlab.com/qemu-project/qemu/-/blob/master/util/osdep.c#L344
func (c *directIOChecker) check(path string, flags int) (bool, error) {
	// #nosec No risk for path injection as we only open the file, not read from it. The function leaks only whether the directory to `path` exists.
	f, err := os.OpenFile(path, flags|syscall.O_DIRECT, 0600)
	if err != nil {
		// EINVAL is returned if the filesystem does not support the O_DIRECT flag
		if err, ok := err.(*os.PathError); ok && err.Err == syscall.EINVAL {
			// #nosec No risk for path injection as we only open the file, not read from it. The function leaks only whether the directory to `path` exists.
			f, err := os.OpenFile(path, flags & ^syscall.O_DIRECT, 0600)
			if err == nil {
				defer util.CloseIOAndCheckErr(f, nil)
				return false, nil
			}
		}
		return false, err
	}
	defer util.CloseIOAndCheckErr(f, nil)
	return true, nil
}

func SetDriverCacheMode(disk *api.Disk, directIOChecker DirectIOChecker) error {
	if disk == nil {
		return fmt.Errorf("unable to set a driver cache mode, disk is nil")
	}

	t := disksource.Resolve(*disk)

	if t.BackendPath() == "" {
		if disk.Device == "cdrom" {
			return nil
		}
		return fmt.Errorf("unable to set a driver cache mode, disk has no backend path")
	}

	var err error
	supportDirectIO := true
	mode := v1.DriverCache(disk.Driver.Cache)

	if mode == "" || mode == v1.CacheNone {
		if t.BackendIsBlock() {
			supportDirectIO, err = directIOChecker.CheckBlockDevice(t.BackendPath())
		} else {
			supportDirectIO, err = directIOChecker.CheckFile(t.BackendPath())
		}
		if err != nil {
			log.Log.Reason(err).Errorf("Direct IO check failed for %s", t.BackendPath())
		} else if !supportDirectIO {
			log.Log.Infof("%s file system does not support direct I/O", t.BackendPath())
		}
		// when the disk is backed-up by another file, we need to also check if that
		// file sits on a file system that supports direct I/O
		if backingFile := disk.BackingStore; backingFile != nil {
			backingFilePath := backingFile.Source.File
			backFileDirectIOSupport, err := directIOChecker.CheckFile(backingFilePath)
			if err != nil {
				log.Log.Reason(err).Errorf("Direct IO check failed for %s", backingFilePath)
			} else if !backFileDirectIOSupport {
				log.Log.Infof("%s backing file system does not support direct I/O", backingFilePath)
			}
			supportDirectIO = supportDirectIO && backFileDirectIOSupport
		}
	}

	// if user set a cache mode = 'none' and fs does not support direct I/O then return an error
	if mode == v1.CacheNone && !supportDirectIO {
		return fmt.Errorf("Unable to use '%s' cache mode, file system where %s is stored does not support direct I/O", mode, t.BackendPath())
	}

	// if user did not set a cache mode and fs supports direct I/O then set cache = 'none'
	// else set cache = 'writethrough
	if mode == "" && supportDirectIO {
		mode = v1.CacheNone
	} else if mode == "" && !supportDirectIO {
		mode = v1.CacheWriteThrough
	}

	disk.Driver.Cache = string(mode)
	log.Log.Infof("Driver cache mode for %s set to %s", t.BackendPath(), mode)

	return nil
}

func IsPreAllocated(path string) bool {
	diskInf, err := disk.GetDiskInfo(path)
	if err != nil {
		return false
	}
	// ActualSize can be a little larger then VirtualSize for qcow2
	return diskInf.VirtualSize <= diskInf.ActualSize
}

// Set optimal io mode automatically
func SetOptimalIOMode(disk *api.Disk, isPreAllocated func(path string) bool) {
	if disk == nil {
		return
	}

	ds := disksource.Resolve(*disk)

	// If the user explicitly set the io mode do nothing
	if disk.Driver.IO != "" {
		return
	}

	if ds.BackendPath() == "" {
		return
	}

	// O_DIRECT is needed for io="native"
	if v1.DriverCache(disk.Driver.Cache) == v1.CacheNone {
		// set native for block device or pre-allocateed image file
		if ds.BackendIsBlock() || isPreAllocated(ds.BackendPath()) {
			disk.Driver.IO = v1.IONative
		}
	}
	// For now we don't explicitly set io=threads even for sparse files as it's
	// not clear it's better for all use-cases
	if disk.Driver.IO != "" {
		log.Log.Infof("Driver IO mode for %s set to %s", ds.BackendPath(), disk.Driver.IO)
	}
}

func Convert_v1_VirtualMachineInstance_To_api_Domain(vmi *v1.VirtualMachineInstance, domain *api.Domain, c *convertertypes.ConverterContext) (err error) {

	precond.MustNotBeNil(vmi)
	precond.MustNotBeNil(domain)
	precond.MustNotBeNil(c)

	hasIOThreads := iothreads.HasIOThreads(vmi)
	var ioThreadCount, autoThreads int
	if hasIOThreads {
		ioThreadCount, autoThreads = iothreads.GetIOThreadsCountType(vmi)
	}

	architecture := c.Architecture.GetArchitecture()
	virtioModel := virtio.InterpretTransitionalModelType(
		vmi.Spec.Domain.Devices.UseVirtioTransitional,
		architecture,
	)
	scsiControllerModel := c.Architecture.SCSIControllerModel(virtioModel)

	configurators := []convertertypes.Configurator{
		metadata.DomainConfigurator{},
		network.NewDomainConfigurator(
			network.WithDomainAttachmentByInterfaceName(c.DomainAttachmentByInterfaceName),
			network.WithUseLaunchSecuritySEV(c.UseLaunchSecuritySEV),
			network.WithUseLaunchSecurityPV(c.UseLaunchSecurityPV),
			network.WithROMTuningSupport(c.Architecture.IsROMTuningSupported()),
			network.WithVirtioModel(virtioModel),
		),
		compute.TPMDomainConfigurator{},
		compute.VSOCKDomainConfigurator{ProcPath: c.VSOCKProcPath},
		compute.NewLaunchSecurityDomainConfigurator(architecture),
		compute.ChannelsDomainConfigurator{},
		compute.ClockDomainConfigurator{},
		compute.NewRNGDomainConfigurator(
			compute.RNGWithUseLaunchSecuritySEV(c.UseLaunchSecuritySEV),
			compute.RNGWithUseLaunchSecurityPV(c.UseLaunchSecurityPV),
			compute.RNGWithVirtioModel(virtioModel),
		),
		compute.NewInputDeviceDomainConfigurator(architecture),
		compute.NewBalloonDomainConfigurator(
			compute.BalloonWithUseLaunchSecuritySEV(c.UseLaunchSecuritySEV),
			compute.BalloonWithUseLaunchSecurityPV(c.UseLaunchSecurityPV),
			compute.BalloonWithFreePageReporting(c.FreePageReporting),
			compute.BalloonWithMemBalloonStatsPeriod(c.MemBalloonStatsPeriod),
			compute.BalloonWithVirtioModel(virtioModel),
		),
		compute.NewGraphicsDomainConfigurator(architecture, c.BochsForEFIGuests, c.AllowCrossArchEmulation),
		compute.SoundDomainConfigurator{},
		compute.NewHostDeviceDomainConfigurator(
			c.GenericHostDevices,
			c.GPUHostDevices,
			c.SRIOVDevices,
		),
		compute.NewWatchdogDomainConfigurator(architecture),
		compute.NewConsoleDomainConfigurator(c.SerialConsoleLog),
		compute.PanicDevicesDomainConfigurator{},
		compute.NewHypervisorFeaturesDomainConfigurator(c.Architecture.HasVMPort(), c.UseLaunchSecurityTDX),
		compute.NewSysInfoDomainConfigurator(convertCmdv1SMBIOSToComputeSMBIOS(c.SMBios)),
		compute.NewOSDomainConfigurator(c.Architecture.IsSMBiosNeeded(), convertEFIConfiguration(c.EFIConfiguration)),
		storage.NewVirtiofsConfigurator(),
		storage.NewDiskConfigurator(c),
		compute.UsbRedirectDeviceDomainConfigurator{},
		compute.NewControllersDomainConfigurator(
			compute.ControllersWithUSBNeeded(c.Architecture.IsUSBNeeded(vmi)),
			compute.ControllersWithSCSIModel(scsiControllerModel),
			compute.ControllersWithSCSIIOThreads(uint(autoThreads)),
			compute.ControllersWithUseLaunchSecuritySEV(c.UseLaunchSecuritySEV),
			compute.ControllersWithUseLaunchSecurityPV(c.UseLaunchSecurityPV),
			compute.ControllersWithSupportPCIHole64Disabling(c.Architecture.SupportPCIHole64Disabling()),
			compute.ControllersWithVirtioSerialModel(virtioModel),
		),
		compute.NewQemuCmdDomainConfigurator(c.Architecture.ShouldVerboseLogsBeEnabled()),
		compute.NewCPUDomainConfigurator(
			compute.CPUWithHotplugSupported(c.Architecture.SupportCPUHotplug()),
			compute.CPUWithMPXCPUValidation(c.Architecture.RequiresMPXCPUValidation()),
			compute.CPUWithCrossArchEmulation(c.AllowCrossArchEmulation),
			compute.CPUWithMemfdSupported(c.Architecture.IsMemfdSupported()),
		),
		compute.NewIOThreadsDomainConfigurator(uint(ioThreadCount)),
		compute.MemoryConfigurator{},
		compute.NewMemoryBackingConfigurator(c.Architecture.IsMemfdSupported()),
		compute.RebootPolicyDomainConfigurator{},
		compute.NewIOMMUFDConfigurator(c.IOMMUFDEnabled),
	}

	switch c.HypervisorName {
	case v1.HyperVDirectHypervisorName:
		configurators = append(configurators, mshv.NewMshvDomainConfigurator(c.AllowEmulation, c.HypervisorDeviceAvailable))
	default:
		if c.AllowCrossArchEmulation {
			configurators = append(configurators, kvm.NewKvmDomainConfiguratorWithCrossArch(c.AllowEmulation, c.HypervisorDeviceAvailable, c.AllowCrossArchEmulation, c.HostArchitecture))
		} else {
			configurators = append(configurators, kvm.NewKvmDomainConfigurator(c.AllowEmulation, c.HypervisorDeviceAvailable))
		}
	}

	builder := convertertypes.NewDomainBuilder(configurators...)
	if err := builder.Build(vmi, domain); err != nil {
		return err
	}

	if vmi.Spec.Domain.CPU != nil {
		hasDRAGuestMapping := vmi.Spec.Domain.CPU.NUMA != nil &&
			vmi.Spec.Domain.CPU.NUMA.GuestMappingPassthrough != nil &&
			len(vmi.Spec.ResourceClaims) > 0

		if vmi.IsCPUDedicated() && hasDRAGuestMapping {
			if graceIOVirtualizationRequested(c) {
				return configureGraceIOVirtualization(&domain.Spec, c.GraceHostDeviceAliases, c.IOMMUFDEnabled)
			}

			if err := buildDRANUMACells(domain, vmi, c, nil); err != nil {
				log.Log.Reason(err).Warningf("Failed to build DRA NUMA cells, falling back to cpuset-based NUMA")
				if err := vcpu.AdjustDomainForTopologyAndCPUSet(domain, vmi, c.Topology, c.CPUSet); err != nil {
					return err
				}
			} else {
				applyDRANUMATopology(domain, vmi)
			}
		} else if vmi.IsCPUDedicated() {
			if err := vcpu.AdjustDomainForTopologyAndCPUSet(domain, vmi, c.Topology, c.CPUSet); err != nil {
				return err
			}

			if graceIOVirtualizationRequested(c) {
				return configureGraceIOVirtualization(&domain.Spec, c.GraceHostDeviceAliases, c.IOMMUFDEnabled)
			}

			if c.PCINUMAAwareTopologyEnabled {
				if c.Architecture.SupportPCIePlacement() {
					strictPCIPlacement := vmi.Spec.Domain.CPU.NUMA != nil &&
						vmi.Spec.Domain.CPU.NUMA.GuestMappingPassthrough != nil
					var opts []PCIPlacementOption
					if strictPCIPlacement {
						opts = append(opts, WithStrictPCINUMAPlacement())
					}
					if err := PlacePCIDevicesWithNUMAAlignment(&domain.Spec, opts...); err != nil {
						if strictPCIPlacement {
							return fmt.Errorf("failed to process strict PCIe NUMA-aware topology: %w", err)
						}
						log.Log.Reason(err).Warningf("Failed to process PCIe NUMA-aware topology, falling back to default placement")
					}
				} else {
					log.Log.Infof("Skipping PCIe NUMA alignment: architecture %s does not support PCIe placement", c.Architecture.GetArchitecture())
				}
			}
		} else if hasDRAGuestMapping {
			if err := buildDRANUMACells(domain, vmi, c, nil); err != nil {
				log.Log.Reason(err).Warningf("Failed to build DRA NUMA cells")
			} else {
				applyDRANUMATopology(domain, vmi)
			}
		}
	}

	if val := vmi.Annotations[v1.PlacePCIDevicesOnRootComplex]; val == "true" {
		if c.Architecture.SupportPCIePlacement() {
			if err := PlacePCIDevicesOnRootComplex(&domain.Spec); err != nil {
				return err
			}
		} else {
			log.Log.Infof("Skipping PCIe root complex placement: architecture %s does not support PCIe placement", c.Architecture.GetArchitecture())
		}
	}

	return nil
}

// buildDRANUMAOverrides extracts NUMA node information from DRA device
// metadata for all DRA-backed host devices and GPUs, keyed by PCI address.
func buildDRANUMAOverrides(vmi *v1.VirtualMachineInstance) map[string]uint32 {
	overrides := make(map[string]uint32)

	type draRef struct {
		claimName   string
		requestName string
	}

	var refs []draRef
	for _, hd := range vmi.Spec.Domain.Devices.HostDevices {
		if drautil.IsHostDeviceDRA(hd) {
			refs = append(refs, draRef{hd.ClaimRequest.ClaimName, hd.ClaimRequest.RequestName})
		}
	}
	for _, gpu := range vmi.Spec.Domain.Devices.GPUs {
		if drautil.IsGPUDRA(gpu) {
			refs = append(refs, draRef{gpu.ClaimRequest.ClaimName, gpu.ClaimRequest.RequestName})
		}
	}

	for _, ref := range refs {
		pciAddr, err := drautil.GetPCIAddressForClaim(
			drautil.DefaultMetadataBasePath, vmi.Spec.ResourceClaims, ref.claimName, ref.requestName)
		if err != nil {
			continue
		}

		numaNode, err := drautil.GetNUMANodeForClaim(
			drautil.DefaultMetadataBasePath, vmi.Spec.ResourceClaims, ref.claimName, ref.requestName)
		if err != nil {
			if sysfsNUMA, sysErr := hardware.GetDeviceNumaNode(pciAddr); sysErr == nil && sysfsNUMA != nil {
				numaNode = int64(*sysfsNUMA)
				log.Log.V(2).Infof("DRA NUMA fallback to sysfs for %s: NUMA %d", pciAddr, numaNode)
			} else {
				continue
			}
		}

		if numaNode >= 0 {
			overrides[pciAddr] = uint32(numaNode) //nolint:gosec // G115: NUMA node IDs are small positive integers
			log.Log.Infof("DRA NUMA override: device %s (claim=%s request=%s) → NUMA %d",
				pciAddr, ref.claimName, ref.requestName, numaNode)
		}
	}

	return overrides
}

// numaNodesFromOverrides extracts the unique set of NUMA node IDs from
// per-device NUMA overrides.
func numaNodesFromOverrides(overrides map[string]uint32) map[uint32]bool {
	if len(overrides) == 0 {
		return nil
	}
	nodes := make(map[uint32]bool)
	for _, n := range overrides {
		nodes[n] = true
	}
	return nodes
}

func applyDRANUMATopology(domain *api.Domain, vmi *v1.VirtualMachineInstance) {
	numaOvr := buildDRANUMAOverrides(vmi)
	draDeviceNUMANodes := numaNodesFromOverrides(numaOvr)
	hostToGuest := buildHostToGuestNUMAMapping(&domain.Spec, draDeviceNUMANodes)
	injectGuestSLITDistances(&domain.Spec, hostToGuest)

	if len(numaOvr) > 0 {
		transformDRAOverridesToGuestCells(numaOvr, hostToGuest)
		if err := PlacePCIDevicesWithNUMAAlignment(&domain.Spec, WithNUMAOverrides(numaOvr)); err != nil {
			log.Log.Reason(err).Warningf("Failed to process PCIe NUMA-aware topology with DRA overrides, falling back to default placement")
		}
	}
}

// buildDRANUMACells constructs guest NUMA cells from DRA device metadata,
// distributing vCPUs and memory evenly across the discovered NUMA nodes.
// It scans all KEP-5304 metadata files mounted into the pod to discover
// NUMA nodes from every allocated device (CPUs, GPUs, NICs, etc.).
func buildDRANUMACells(
	domain *api.Domain,
	vmi *v1.VirtualMachineInstance,
	c *convertertypes.ConverterContext,
	deviceNUMANodes map[uint32]bool,
) error {
	allDevices, err := drautil.DiscoverNUMANodesFromAllMetadata(drautil.DefaultMetadataBasePath)
	if err != nil {
		log.Log.Reason(err).Warning("Failed to discover NUMA nodes from DRA metadata")
	}

	numaNodes := make(map[int64]bool)
	for _, dev := range allDevices {
		if dev.NUMANode >= 0 {
			numaNodes[dev.NUMANode] = true
		}
	}
	for n := range deviceNUMANodes {
		numaNodes[int64(n)] = true
	}

	if len(numaNodes) == 0 {
		return fmt.Errorf("no NUMA nodes found in DRA metadata")
	}

	vcpus := hardware.GetNumberOfVCPUs(vmi.Spec.Domain.CPU)
	if vcpus == 0 {
		vcpus = 1
	}

	sortedNUMAs := make([]int64, 0, len(numaNodes))
	for n := range numaNodes {
		sortedNUMAs = append(sortedNUMAs, n)
	}
	sort.Slice(sortedNUMAs, func(i, j int) bool { return sortedNUMAs[i] < sortedNUMAs[j] })

	numCells := int64(len(sortedNUMAs))
	cpuPerCell := vcpus / numCells
	extraCPUs := vcpus % numCells

	var guestMemory int64
	if vmi.Spec.Domain.Memory != nil && vmi.Spec.Domain.Memory.Guest != nil {
		guestMemory = vmi.Spec.Domain.Memory.Guest.Value() / 1024 // KiB
	} else if req, ok := vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceMemory]; ok {
		guestMemory = req.Value() / 1024
	}
	if guestMemory <= 0 {
		return fmt.Errorf("cannot build DRA NUMA cells: no guest memory configured")
	}
	memPerCell := guestMemory / numCells

	domain.Spec.CPU.NUMA = &api.NUMA{}
	var cpuID int64
	for i, numaID := range sortedNUMAs {
		cpus := cpuPerCell
		if int64(i) < extraCPUs {
			cpus++
		}

		var cpuList []string
		for j := int64(0); j < cpus; j++ {
			cpuList = append(cpuList, fmt.Sprintf("%d", cpuID))
			cpuID++
		}

		cellMem := memPerCell
		if i == len(sortedNUMAs)-1 {
			// Give any remainder to the last cell
			cellMem = guestMemory - memPerCell*int64(len(sortedNUMAs)-1)
		}

		cellMemU := uint64(cellMem) //nolint:gosec // G115: memory is always positive (guarded above)
		cell := api.NUMACell{
			ID:     fmt.Sprintf("%d", i),
			CPUs:   strings.Join(cpuList, ","),
			Memory: &cellMemU,
			Unit:   "KiB",
		}
		domain.Spec.CPU.NUMA.Cells = append(domain.Spec.CPU.NUMA.Cells, cell)

		log.Log.Infof("DRA NUMA cell: guest=%d host=%d cpus=%s memory=%d KiB",
			i, numaID, strings.Join(cpuList, ","), cellMem)
	}

	return nil
}

// buildHostToGuestNUMAMapping creates a mapping from host NUMA node IDs to
// guest NUMA cell IDs based on sorted order correspondence.
func buildHostToGuestNUMAMapping(spec *api.DomainSpec, deviceNUMANodes map[uint32]bool) map[uint32]uint32 {
	hostToGuest := make(map[uint32]uint32)
	if spec.CPU.NUMA == nil {
		return hostToGuest
	}

	sortedHostNUMAs := make([]uint32, 0, len(deviceNUMANodes))
	for n := range deviceNUMANodes {
		sortedHostNUMAs = append(sortedHostNUMAs, n)
	}
	sort.Slice(sortedHostNUMAs, func(i, j int) bool { return sortedHostNUMAs[i] < sortedHostNUMAs[j] })

	for i, hostNUMA := range sortedHostNUMAs {
		if i < len(spec.CPU.NUMA.Cells) {
			guestID, err := strconv.ParseUint(spec.CPU.NUMA.Cells[i].ID, 10, 32)
			if err == nil {
				hostToGuest[hostNUMA] = uint32(guestID)
			}
		}
	}

	return hostToGuest
}

// transformDRAOverridesToGuestCells remaps device NUMA overrides from host
// NUMA node IDs to guest NUMA cell IDs.
func transformDRAOverridesToGuestCells(overrides map[string]uint32, hostToGuest map[uint32]uint32) {
	for pciAddr, hostNUMA := range overrides {
		if guestNUMA, ok := hostToGuest[hostNUMA]; ok {
			overrides[pciAddr] = guestNUMA
		} else {
			log.Log.Warningf("Device %s host NUMA %d has no guest cell mapping, removing from overrides", pciAddr, hostNUMA)
			delete(overrides, pciAddr)
		}
	}
}

// injectGuestSLITDistances reads host SLIT (System Locality Information Table)
// distances and injects them as <distances> elements on guest NUMA cells.
func injectGuestSLITDistances(spec *api.DomainSpec, hostToGuest map[uint32]uint32) {
	if spec.CPU.NUMA == nil || len(spec.CPU.NUMA.Cells) <= 1 {
		return
	}

	hostDistances, err := drautil.ReadHostSLITDistances("")
	if err != nil {
		log.Log.Reason(err).Warning("Failed to read host SLIT distances")
		return
	}
	if len(hostDistances) == 0 {
		return
	}

	guestToHost := make(map[uint32]uint32)
	for h, g := range hostToGuest {
		guestToHost[g] = h
	}

	for i := range spec.CPU.NUMA.Cells {
		guestID, err := strconv.ParseUint(spec.CPU.NUMA.Cells[i].ID, 10, 32)
		if err != nil {
			continue
		}
		hostID, ok := guestToHost[uint32(guestID)]
		if !ok {
			continue
		}
		hostDist, ok := hostDistances[int(hostID)]
		if !ok {
			continue
		}

		var siblings []api.NUMACellSibling
		for j := range spec.CPU.NUMA.Cells {
			targetGuestID, err := strconv.ParseUint(spec.CPU.NUMA.Cells[j].ID, 10, 32)
			if err != nil {
				continue
			}
			targetHostID, ok := guestToHost[uint32(targetGuestID)]
			if !ok {
				continue
			}
			if int(targetHostID) < len(hostDist) {
				siblings = append(siblings, api.NUMACellSibling{
					ID:    fmt.Sprintf("%d", targetGuestID),
					Value: uint64(hostDist[targetHostID]), //nolint:gosec // G115: SLIT distances are small positive integers
				})
			}
		}

		if len(siblings) > 0 {
			spec.CPU.NUMA.Cells[i].Distances = &api.NUMACellDistances{Siblings: siblings}
		}
	}
}

func GetVolumeNameByDisk(disk api.Disk) string {
	return disk.Alias.GetName()
}

// GetVolumeNameByTarget returns the volume name associated to the device target in the domain (e.g vda)
func GetVolumeNameByTarget(domain *api.Domain, target string) string {
	for _, d := range domain.Spec.Devices.Disks {
		if d.Target.Device == target {
			return GetVolumeNameByDisk(d)
		}
	}
	return ""
}

func GracePeriodSeconds(vmi *v1.VirtualMachineInstance) int64 {
	gracePeriodSeconds := v1.DefaultGracePeriodSeconds
	if vmi.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriodSeconds = *vmi.Spec.TerminationGracePeriodSeconds
	}
	return gracePeriodSeconds
}

func convertCmdv1SMBIOSToComputeSMBIOS(input *cmdv1.SMBios) *compute.SMBIOS {
	if input == nil {
		return nil
	}

	return &compute.SMBIOS{
		Manufacturer: input.Manufacturer,
		Product:      input.Product,
		Version:      input.Version,
		SKU:          input.Sku,
		Family:       input.Family,
	}
}

func convertEFIConfiguration(input *convertertypes.EFIConfiguration) *compute.EFIConfiguration {
	if input == nil {
		return nil
	}

	return &compute.EFIConfiguration{
		EFICode:                   input.EFICode,
		EFIVars:                   input.EFIVars,
		SecureLoader:              input.SecureLoader,
		UsesFirmwareAutoSelection: input.UsesFirmwareAutoSelection,
	}
}
