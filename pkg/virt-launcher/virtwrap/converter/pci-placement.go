package converter

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/utils/ptr"
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	maxExpanderBusNr = 255
)

// DevicePlacementOverride provides explicit NUMA node and PCIe root complex
// assignments for a PCI device. Presence in the overrides map means this
// device's placement is determined by metadata rather than sysfs.
type DevicePlacementOverride struct {
	NUMANode uint32
	PCIeRoot string // empty = group by NUMA node only
}

type pciNUMAPlacementOptions struct {
	strict             bool
	placementOverrides map[string]DevicePlacementOverride
}

// PCIPlacementOption configures PCI NUMA-aware placement behavior.
type PCIPlacementOption func(*pciNUMAPlacementOptions)

// WithStrictPCINUMAPlacement makes placement fail when a PCI host device cannot
// be represented in the NUMA-aware PCI topology.
func WithStrictPCINUMAPlacement() PCIPlacementOption {
	return func(options *pciNUMAPlacementOptions) {
		options.strict = true
	}
}

// WithDevicePlacementOverrides supplies explicit NUMA node and PCIe root
// complex assignments for PCI devices, keyed by PCI address. Devices with
// a pcieRoot are grouped by root complex; others fall back to NUMA grouping.
func WithDevicePlacementOverrides(overrides map[string]DevicePlacementOverride) PCIPlacementOption {
	return func(options *pciNUMAPlacementOptions) {
		options.placementOverrides = overrides
	}
}

// iteratePCIAddresses invokes the callback function for each PCI device specified in the domain
func iteratePCIAddresses(spec *api.DomainSpec, callback func(address *api.Address) (*api.Address, error)) (err error) {
	fn := func(address *api.Address) (*api.Address, error) {
		if address == nil || address.Type == "" || address.Type == api.AddressPCI {
			return callback(address)
		}
		return address, nil
	}
	for i, iface := range spec.Devices.Interfaces {
		spec.Devices.Interfaces[i].Address, err = fn(iface.Address)
		if err != nil {
			return err
		}
	}
	for i, hostDev := range spec.Devices.HostDevices {
		if hostDev.Type != api.HostDevicePCI {
			continue
		}
		spec.Devices.HostDevices[i].Address, err = fn(hostDev.Address)
		if err != nil {
			return err
		}
	}
	for i, controller := range spec.Devices.Controllers {
		// pci-root, pcie-root and pcie-expander-bus devices can by definition not have a PCI address
		if controller.Model == "pci-root" ||
			controller.Model == api.ControllerModelPCIeRoot ||
			controller.Model == api.ControllerModelPCIeExpanderBus {
			continue
		}
		spec.Devices.Controllers[i].Address, err = fn(controller.Address)
		if err != nil {
			return err
		}
	}
	for i, disk := range spec.Devices.Disks {
		if disk.Target.Bus != v1.DiskBusVirtio {
			continue
		}
		spec.Devices.Disks[i].Address, err = fn(disk.Address)
		if err != nil {
			return err
		}
	}
	for i, input := range spec.Devices.Inputs {
		if input.Bus != v1.VirtIO {
			continue
		}
		spec.Devices.Inputs[i].Address, err = fn(input.Address)
		if err != nil {
			return err
		}
	}
	for i, watchdog := range spec.Devices.Watchdogs {
		spec.Devices.Watchdogs[i].Address, err = fn(watchdog.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Rng != nil {
		spec.Devices.Rng.Address, err = fn(spec.Devices.Rng.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Ballooning != nil {
		spec.Devices.Ballooning.Address, err = fn(spec.Devices.Ballooning.Address)
		if err != nil {
			return err
		}
	}
	return nil
}

func CountPCIDevices(spec *api.DomainSpec) (count int, err error) {
	err = iteratePCIAddresses(spec, func(address *api.Address) (*api.Address, error) {
		count++
		return address, nil
	})
	return count, err
}

func PlacePCIDevicesOnRootComplex(spec *api.DomainSpec) (err error) {
	assigner := newRootSlotAssigner()
	return iteratePCIAddresses(spec, assigner.PlacePCIDeviceAtNextSlot)
}

func (p *pciRootSlotAssigner) nextSlot() (int, error) {
	slot := p.slot + 1
	// reserved slots are:
	// slot 0
	// slot 1 for VGA
	// slot 0x1f for a sata controller from  qemu
	// slot 0x1b for the first ich9 sound card
	switch slot {
	case 0, 0x01:
		slot = 0x02
	case 0x1f, 0x1b:
		slot = slot + 1
	}

	if slot >= 0x20 {
		return slot, fmt.Errorf("No space left on the root PCI bus.")
	}
	p.slot = slot
	return slot, nil
}

func newRootSlotAssigner() *pciRootSlotAssigner {
	return &pciRootSlotAssigner{slot: -1}
}

type pciRootSlotAssigner struct {
	slot int
}

// newPCIAddress creates a PCI address with the specified bus and slot.
func newPCIAddress(bus string, slot string) *api.Address {
	return &api.Address{
		Type:     api.AddressPCI,
		Domain:   "0x0000",
		Bus:      bus,
		Slot:     slot,
		Function: "0x0",
	}
}

func (p *pciRootSlotAssigner) PlacePCIDeviceAtNextSlot(address *api.Address) (*api.Address, error) {
	if address == nil {
		address = &api.Address{}
	}

	// keep explicit requests for pci addresses
	if address.Domain != "" {
		return address, nil
	}

	slot, err := p.nextSlot()
	if err != nil {
		return nil, err
	}
	address.Type = api.AddressPCI
	address.Domain = "0x0000"
	address.Bus = "0x00"
	address.Slot = fmt.Sprintf("%#02x", slot)
	address.Function = "0x0"
	return address, nil
}

// numaAwareTopology represents the PCIe topology for a specific NUMA node.
type numaAwareTopology struct {
	expanderBus               *api.Controller
	rootPorts                 []*api.Controller
	addressPerDeviceSourcePCI map[string]*api.Address
}

// expanderBusAssigner manages the assignment of PCIe expander buses and
// NUMA aligned device placement.
type expanderBusAssigner struct {
	domainSpec              *api.DomainSpec
	controllerIndex         uint32
	controllerCount         uint32
	topologyMap             map[string]*numaAwareTopology
	topologyKeyToNUMA       map[string]uint32
	devices                 map[string]*api.HostDevice
	devicesNUMANodes        map[string]uint32
	deviceNUMANodeOverrides map[string]uint32
	devicePCIeRootOverrides map[string]string
	isolatedDevices         map[string]*api.IOMMUDevice
	strict                  bool
	strictFailures          []string

	// lastAssignedBusNr tracks the last assigned bus number for expander buses.
	// It starts from maxExpanderBusNr and decreases as expander buses are assigned
	// to ensure controller indices don't conflict with expander bus number space.
	lastAssignedBusNr uint32
}

func getCurrentControllerIndex(domainSpec *api.DomainSpec) uint32 {
	maxIndex := uint32(0)
	for _, controller := range domainSpec.Devices.Controllers {
		if idx, err := strconv.ParseUint(controller.Index, 10, 32); err == nil {
			if uint32(idx) > maxIndex {
				maxIndex = uint32(idx)
			}
		} else {
			log.Log.Warningf("failed to parse controller index '%s': %v", controller.Index, err)
		}
	}
	return maxIndex
}

// newExpanderBusAssigner creates a new PCIe expander bus assigner.
func newExpanderBusAssigner(domainSpec *api.DomainSpec, opts ...PCIPlacementOption) *expanderBusAssigner {
	return newExpanderBusAssignerWithOptions(domainSpec, nil, nil, opts...)
}

func newExpanderBusAssignerWithOptions(domainSpec *api.DomainSpec, isolatedDevices map[string]*api.IOMMUDevice, numaOverrides map[string]uint32, opts ...PCIPlacementOption) *expanderBusAssigner {
	options := pciNUMAPlacementOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	pcieRootOverrides := make(map[string]string)
	if options.placementOverrides != nil {
		if numaOverrides == nil {
			numaOverrides = make(map[string]uint32, len(options.placementOverrides))
		}
		for addr, ovr := range options.placementOverrides {
			numaOverrides[addr] = ovr.NUMANode
			if ovr.PCIeRoot != "" {
				pcieRootOverrides[addr] = ovr.PCIeRoot
			}
		}
	}

	currentControllerIndex := getCurrentControllerIndex(domainSpec)
	log.Log.Infof("Current max controller index: %d", currentControllerIndex)

	assigner := &expanderBusAssigner{
		domainSpec:              domainSpec,
		topologyMap:             make(map[string]*numaAwareTopology),
		topologyKeyToNUMA:       make(map[string]uint32),
		devices:                 make(map[string]*api.HostDevice),
		devicesNUMANodes:        make(map[string]uint32),
		deviceNUMANodeOverrides: numaOverrides,
		devicePCIeRootOverrides: pcieRootOverrides,
		isolatedDevices:         isolatedDevices,
		strict:                  options.strict,
		controllerIndex:         currentControllerIndex,
		controllerCount:         0,
		lastAssignedBusNr:       maxExpanderBusNr,
	}

	return assigner
}

// PlacePCIDevicesWithNUMAAlignment places PCI devices in the domainSpec with
// NUMA alignment using PCIe expander buses. It modifies the domainSpec in place
// or leaves it unchanged in case of an error.
func PlacePCIDevicesWithNUMAAlignment(domainSpec *api.DomainSpec, opts ...PCIPlacementOption) error {
	assigner := newExpanderBusAssigner(domainSpec, opts...)
	return assigner.PlaceNumaAlignedDevices()
}

func (a *expanderBusAssigner) createController(model string, parentBus string, slot uint32, numaNode *uint32) *api.Controller {
	a.controllerIndex++
	a.controllerCount++

	controller := &api.Controller{
		Type:  api.ControllerTypePCI,
		Index: fmt.Sprint(a.controllerIndex),
		Model: model,
	}

	// PCIe expander bus doesn't have a PCI address and has a NUMA target
	if model == api.ControllerModelPCIeExpanderBus {
		controller.Target = &api.ControllerTarget{
			NUMANode: numaNode,
		}
		return controller
	}

	// All other controllers have PCI addresses
	slotStr := "0x00"
	if slot > 0 {
		slotStr = fmt.Sprintf("%#02x", slot)
	}

	controller.Address = newPCIAddress(parentBus, slotStr)

	return controller
}

func (a *expanderBusAssigner) addDevices(devices []api.HostDevice) error {
	var pciAddresses []string
	devicesByAddress := make(map[string]*api.HostDevice)
	a.strictFailures = nil

	for i := range devices {
		if devices[i].Type != api.HostDevicePCI {
			continue
		}

		if devices[i].Source.Address == nil {
			a.handlePlacementWarning("PCI host device has no source address, skipping pcie-expander-bus assignment")
			continue
		}

		address := hardware.PCIAddressToString(devices[i].Source.Address)
		if hasGuestPCIAddress(devices[i].Address) {
			a.handlePlacementWarning("device %s already has a guest PCI address, skipping pcie-expander-bus assignment", address)
			continue
		}

		pciAddresses = append(pciAddresses, address)
		devicesByAddress[address] = &devices[i]
	}

	slices.Sort(pciAddresses)
	devicesNUMANodes, warnings := hardware.LookupDevicesNumaNodesWithWarnings(pciAddresses, a.domainSpec)
	warningsByAddress := make(map[string][]string)
	for _, warning := range warnings {
		log.Log.Warningf("PCI NUMA-aware placement: %s", warning.String())
		if warning.PCIAddress != "" {
			warningsByAddress[warning.PCIAddress] = append(warningsByAddress[warning.PCIAddress], warning.Reason)
		} else if a.strict {
			a.addStrictFailure(warning.String())
		}
	}

	for _, address := range pciAddresses {
		device := devicesByAddress[address]
		if numaNode, exists := a.deviceNUMANodeOverrides[address]; exists {
			a.devices[address] = device
			a.devicesNUMANodes[address] = numaNode
			continue
		}

		if numaNode, exists := devicesNUMANodes[address]; exists {
			a.devices[address] = device
			a.devicesNUMANodes[address] = numaNode
		} else {
			reason := "no guest NUMA node mapping is available"
			if warningReasons := warningsByAddress[address]; len(warningReasons) > 0 {
				reason = strings.Join(warningReasons, "; ")
			}
			a.handlePlacementWarning("device %s cannot be placed on a NUMA-aware PCIe topology: %s", address, reason)
		}
	}

	if len(a.strictFailures) > 0 {
		return fmt.Errorf("strict PCI NUMA-aware placement failed: %s", strings.Join(a.strictFailures, "; "))
	}
	return nil
}

func (a *expanderBusAssigner) handlePlacementWarning(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	log.Log.Warningf("%s", message)
	a.addStrictFailure(message)
}

func (a *expanderBusAssigner) addStrictFailure(message string) {
	if a.strict {
		a.strictFailures = append(a.strictFailures, message)
	}
}

const numaFallbackPrefix = "__numa:"

// topologyDeviceGroups maps topology keys to host devices.
type topologyDeviceGroups map[string][]*api.HostDevice

// topologyKey returns the grouping key for a device. Devices with a pcieRoot
// override are grouped by root complex; others fall back to NUMA node.
func (a *expanderBusAssigner) topologyKey(addressKey string) (string, bool) {
	if root, ok := a.devicePCIeRootOverrides[addressKey]; ok && root != "" {
		return root, true
	}
	if numaNode, ok := a.devicesNUMANodes[addressKey]; ok {
		return fmt.Sprintf("%s%d", numaFallbackPrefix, numaNode), true
	}
	return "", false
}

// groupDevicesByTopology groups devices by PCIe root complex (primary) or
// NUMA node (fallback). Validates that devices sharing a pcieRoot have
// consistent NUMA nodes.
func (a *expanderBusAssigner) groupDevicesByTopology() topologyDeviceGroups {
	groups := make(topologyDeviceGroups)
	pcieRootNUMA := make(map[string]uint32)

	for _, addressKey := range sortedKeys(a.devices) {
		device := a.devices[addressKey]
		key, ok := a.topologyKey(addressKey)
		if !ok {
			continue
		}

		numaNode := a.devicesNUMANodes[addressKey]

		if root, hasPCIe := a.devicePCIeRootOverrides[addressKey]; hasPCIe && root != "" {
			if existingNUMA, seen := pcieRootNUMA[root]; seen && existingNUMA != numaNode {
				log.Log.Warningf(
					"PCIe root %s has devices on NUMA %d and %d, using %d",
					root, existingNUMA, numaNode, existingNUMA)
				numaNode = existingNUMA
				a.devicesNUMANodes[addressKey] = numaNode
			} else {
				pcieRootNUMA[root] = numaNode
			}
		}

		a.topologyKeyToNUMA[key] = numaNode
		groups[key] = append(groups[key], device)
	}

	return groups
}

// getNumaAwareTopology handles NUMA aware topology retrieval or creation
// from the topology map. It creates an expander bus if the topology for that
// key doesn't exist and returns that topology.
func (a *expanderBusAssigner) getNumaAwareTopology(topoKey string) *numaAwareTopology {
	topology, exists := a.topologyMap[topoKey]
	if !exists {
		numaNode := a.topologyKeyToNUMA[topoKey]
		topology = &numaAwareTopology{
			expanderBus:               a.createController(api.ControllerModelPCIeExpanderBus, "", 0, &numaNode),
			addressPerDeviceSourcePCI: make(map[string]*api.Address),
		}
		a.topologyMap[topoKey] = topology
	}
	return topology
}

// addRootPort creates a PCIe root port and adds it to the topology.
func (a *expanderBusAssigner) addRootPort(topology *numaAwareTopology, parentBus string) *api.Controller {
	slot := uint32(len(topology.rootPorts))
	rootPort := a.createController(api.ControllerModelPCIeRootPort, parentBus, slot, nil)
	topology.rootPorts = append(topology.rootPorts, rootPort)
	return rootPort
}

// placeDevice creates a root port and assigns the device directly to it.
func (a *expanderBusAssigner) placeDevice(topology *numaAwareTopology, device *api.HostDevice) error {
	if a.controllerIndex >= a.lastAssignedBusNr-1 {
		return fmt.Errorf("insufficient bus numbers for NUMA-aligned PCIe topology: current controller index %d, last assigned expander bus number %d",
			a.controllerIndex, a.lastAssignedBusNr)
	}

	rootPort := a.addRootPort(topology, topology.expanderBus.Index)
	sourceAddress := hardware.PCIAddressToString(device.Source.Address)
	topology.addressPerDeviceSourcePCI[sourceAddress] = newPCIAddress(rootPort.Index, "0x00")

	return nil
}

func (a *expanderBusAssigner) createDeviceTopology(numaNode uint32) *numaAwareTopology {
	return &numaAwareTopology{
		expanderBus:               a.createController(api.ControllerModelPCIeExpanderBus, "", 0, &numaNode),
		addressPerDeviceSourcePCI: make(map[string]*api.Address),
	}
}

// buildTopology groups devices by PCIe root complex (or NUMA node as fallback)
// using a pcie-expander-bus per group. Within a pcie-expander-bus one
// pcie-root-port per device is created.
//
// pcie-expander-bus (one per pcieRoot/NUMA group) -> pcie-root-port (one per device) -> device
func (a *expanderBusAssigner) buildTopology() error {
	topoGroups := a.groupDevicesByTopology()

	for _, topoKey := range sortedKeys(topoGroups) {
		devices := topoGroups[topoKey]
		numaNode := a.topologyKeyToNUMA[topoKey]

		var normalDevices []*api.HostDevice
		for _, device := range devices {
			address := hardware.PCIAddressToString(device.Source.Address)
			iommuDevice, isolated := a.isolatedDevices[address]
			if !isolated {
				normalDevices = append(normalDevices, device)
				continue
			}

			topology := a.createDeviceTopology(numaNode)
			if err := a.placeDevice(topology, device); err != nil {
				return fmt.Errorf("failed to place isolated device %s: %w", address, err)
			}
			a.assignBusNr(topology)
			if err := a.applyIOMMUDevice(topology, iommuDevice); err != nil {
				return fmt.Errorf("failed to place isolated device %s IOMMU: %w", address, err)
			}
			a.applyTopology(topology)
		}

		if len(normalDevices) == 0 {
			continue
		}

		topology := a.getNumaAwareTopology(topoKey)
		for _, device := range normalDevices {
			if err := a.placeDevice(topology, device); err != nil {
				return fmt.Errorf("failed to place device %s: %w", hardware.PCIAddressToString(device.Source.Address), err)
			}
		}
		a.assignBusNr(topology)
	}

	return nil
}

func (a *expanderBusAssigner) assignBusNr(topology *numaAwareTopology) {
	// Set the busNr of the expander bus so that it has enough space for all
	// its children. We start from 254 (1 expander bus + 1 root port, when one
	// device is aligned with a NUMA node) and go downwards to leave space for
	// system controllers and additional expander buses.
	busNr := maxExpanderBusNr - a.controllerCount + 1
	topology.expanderBus.Target.BusNr = ptr.To(busNr)
	a.lastAssignedBusNr = busNr
}

func (a *expanderBusAssigner) applyIOMMUDevice(topology *numaAwareTopology, iommuDevice *api.IOMMUDevice) error {
	if iommuDevice == nil {
		return nil
	}
	if iommuDevice.Driver == nil {
		iommuDevice.Driver = &api.IOMMUDriver{}
	}
	if topology == nil || topology.expanderBus == nil || topology.expanderBus.Index == "" {
		return fmt.Errorf("missing pcie-expander-bus controller index")
	}
	iommuDevice.Driver.PCIBus = topology.expanderBus.Index
	a.domainSpec.Devices.IOMMU = append(a.domainSpec.Devices.IOMMU, *iommuDevice)
	return nil
}

// PlaceNumaAlignedDevices queues host devices to the assigner and places them
// into a PCIe topology aligned to their NUMA node. It modifies the domainSpec
// in place or leaves it unchanged in case of an error.
func (a *expanderBusAssigner) PlaceNumaAlignedDevices() error {
	if err := a.addDevices(a.domainSpec.Devices.HostDevices); err != nil {
		return err
	}

	if err := a.buildTopology(); err != nil {
		return fmt.Errorf("failed to create PCIe topology with NUMA alignment: %w", err)
	}

	for _, numaKey := range sortedKeys(a.topologyMap) {
		a.applyTopology(a.topologyMap[numaKey])
	}

	return nil
}

func (a *expanderBusAssigner) applyTopology(topology *numaAwareTopology) {
	a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *topology.expanderBus)

	for _, rootPort := range topology.rootPorts {
		a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *rootPort)
	}

	for _, sourceAddress := range sortedKeys(topology.addressPerDeviceSourcePCI) {
		address := topology.addressPerDeviceSourcePCI[sourceAddress]
		if device, exists := a.devices[sourceAddress]; exists {
			device.Address = address
		}
		// If a device was not placed in the topology (e.g. missing vCPU
		// affinity information), we leave it unmodified so that it can be
		// placed by the root slot assigner.
	}
}

func sortedKeys[K cmp.Ordered, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedUint32Keys[T any](values map[uint32]T) []uint32 {
	return sortedKeys(values)
}

func hasGuestPCIAddress(address *api.Address) bool {
	if address == nil {
		return false
	}

	// Any populated PCI coordinate is treated as explicit guest placement.
	// This planner must not complete or relocate partially populated addresses;
	// malformed addresses are left to the normal domain validation path.
	return address.Domain != "" || address.Bus != "" || address.Slot != "" || address.Function != ""
}
