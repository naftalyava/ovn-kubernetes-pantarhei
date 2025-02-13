package node

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// NodeAllocator acts on node events handed off by the cluster network
// controller and does the following:
//   - allocates subnet from the cluster subnet pool. It also allocates subnets
//     from the hybrid overlay subnet pool if hybrid overlay is enabled.
//     It stores these allocated subnets in the node annotation.
//     Only for the default or layer3 networks.
//   - stores the network id in each node's annotation.
type NodeAllocator struct {
	kube       kube.Interface
	nodeLister listers.NodeLister

	clusterSubnetAllocator       SubnetAllocator
	hybridOverlaySubnetAllocator SubnetAllocator

	// unique id of the network
	networkID int

	netInfo util.NetInfo
}

func NewNodeAllocator(networkID int, netInfo util.NetInfo, nodeLister listers.NodeLister, kube kube.Interface) *NodeAllocator {
	na := &NodeAllocator{
		kube:                         kube,
		nodeLister:                   nodeLister,
		networkID:                    networkID,
		netInfo:                      netInfo,
		clusterSubnetAllocator:       NewSubnetAllocator(),
		hybridOverlaySubnetAllocator: NewSubnetAllocator(),
	}

	if na.hasNodeSubnetAllocation() {
		na.clusterSubnetAllocator = NewSubnetAllocator()
	}

	if na.hasHybridOverlayAllocation() {
		na.hybridOverlaySubnetAllocator = NewSubnetAllocator()
	}

	return na
}

func (na *NodeAllocator) Init() error {
	if !na.hasNodeSubnetAllocation() {
		return nil
	}

	clusterSubnets := na.netInfo.Subnets()

	for _, clusterSubnet := range clusterSubnets {
		if err := na.clusterSubnetAllocator.AddNetworkRange(clusterSubnet.CIDR, clusterSubnet.HostSubnetLength); err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to cluster subnet allocator", clusterSubnet.CIDR)
	}

	if na.hasHybridOverlayAllocation() {
		for _, hoSubnet := range config.HybridOverlay.ClusterSubnets {
			if err := na.hybridOverlaySubnetAllocator.AddNetworkRange(hoSubnet.CIDR, hoSubnet.HostSubnetLength); err != nil {
				return err
			}
			klog.V(5).Infof("Added network range %s to hybrid overlay subnet allocator", hoSubnet.CIDR)
		}
	}

	// update metrics for cluster subnets
	na.recordSubnetCount()

	return nil
}

func (na *NodeAllocator) hasHybridOverlayAllocation() bool {
	return config.HybridOverlay.Enabled && !na.netInfo.IsSecondary()
}

func (na *NodeAllocator) recordSubnetCount() {
	// only for the default network
	if !na.netInfo.IsSecondary() {
		v4count, v6count := na.clusterSubnetAllocator.Count()
		metrics.RecordSubnetCount(float64(v4count), float64(v6count))
	}
}

func (na *NodeAllocator) recordSubnetUsage() {
	// only for the default network
	if !na.netInfo.IsSecondary() {
		v4used, v6used := na.clusterSubnetAllocator.Usage()
		metrics.RecordSubnetUsage(float64(v4used), float64(v6used))
	}
}

// hybridOverlayNodeEnsureSubnet allocates a subnet and sets the
// hybrid overlay subnet annotation. It returns any newly allocated subnet
// or an error. If an error occurs, the newly allocated subnet will be released.
func (na *NodeAllocator) hybridOverlayNodeEnsureSubnet(node *corev1.Node, annotator kube.Annotator) (*net.IPNet, error) {
	var existingSubnets []*net.IPNet
	// Do not allocate a subnet if the node already has one
	subnet, err := houtil.ParseHybridOverlayHostSubnet(node)
	if err != nil {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s hybrid overlay subnet annotation: %v", node.Name, err)
	} else if subnet != nil {
		existingSubnets = []*net.IPNet{subnet}
	}

	// Allocate a new host subnet for this node
	// FIXME: hybrid overlay is only IPv4 for now due to limitations on the Windows side
	hostSubnets, allocatedSubnets, err := na.allocateNodeSubnets(na.hybridOverlaySubnetAllocator, node.Name, existingSubnets, true, false)
	if err != nil {
		return nil, fmt.Errorf("error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}

	if err := annotator.Set(hotypes.HybridOverlayNodeSubnet, hostSubnets[0].String()); err != nil {
		if e := na.hybridOverlaySubnetAllocator.ReleaseNetworks(node.Name, allocatedSubnets...); e != nil {
			klog.Warningf("Failed to release hybrid over subnet for the node %s from the allocator : %w", node.Name, e)
		}
		return nil, fmt.Errorf("error setting hybrid overlay host subnet: %w", err)
	}

	return hostSubnets[0], nil
}

func (na *NodeAllocator) releaseHybridOverlayNodeSubnet(nodeName string) {
	na.hybridOverlaySubnetAllocator.ReleaseAllNetworks(nodeName)
	klog.Infof("Deleted hybrid overlay HostSubnets for node %s", nodeName)
}

// HandleAddUpdateNodeEvent handles the add or update node event
func (na *NodeAllocator) HandleAddUpdateNodeEvent(node *corev1.Node) error {
	defer na.recordSubnetCount()

	if util.NoHostSubnet(node) {
		if na.hasHybridOverlayAllocation() && houtil.IsHybridOverlayNode(node) {
			annotator := kube.NewNodeAnnotator(na.kube, node.Name)
			allocatedSubnet, err := na.hybridOverlayNodeEnsureSubnet(node, annotator)
			if err != nil {
				return fmt.Errorf("failed to update node %s hybrid overlay subnet annotation: %v", node.Name, err)
			}
			if err := annotator.Run(); err != nil {
				// Release allocated subnet if any errors occurred
				if allocatedSubnet != nil {
					na.releaseHybridOverlayNodeSubnet(node.Name)
				}
				return fmt.Errorf("failed to set hybrid overlay annotations for node %s: %v", node.Name, err)
			}
		}
		return nil
	}

	return na.syncNodeNetworkAnnotations(node)
}

// syncNodeNetworkAnnotations does 2 things
//   - syncs the node's allocated subnets in the node subnet annotation
//   - syncs the network id in the node network id annotation
func (na *NodeAllocator) syncNodeNetworkAnnotations(node *corev1.Node) error {
	networkName := na.netInfo.GetNetworkName()

	networkID, err := util.ParseNetworkIDAnnotation(node, networkName)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s network id annotations for network %s : %v", node.Name, networkName, err)
	}

	updatedSubnetsMap := map[string][]*net.IPNet{}
	var validExistingSubnets, allocatedSubnets []*net.IPNet
	if na.hasNodeSubnetAllocation() {
		existingSubnets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			// Log the error and try to allocate new subnets
			klog.Warningf("Failed to get node %s host subnets annotations for network %s : %v", node.Name, networkName, err)
		}

		// On return validExistingSubnets will contain any valid subnets that
		// were already assigned to the node. allocatedSubnets will contain
		// any newly allocated subnets required to ensure that the node has one subnet
		// from each enabled IP family.
		ipv4Mode, ipv6Mode := na.netInfo.IPMode()
		validExistingSubnets, allocatedSubnets, err = na.allocateNodeSubnets(na.clusterSubnetAllocator, node.Name, existingSubnets, ipv4Mode, ipv6Mode)
		if err != nil {
			return err
		}

		// If the existing subnets weren't OK, or new ones were allocated, update the node annotation.
		// This happens in a couple cases:
		// 1) new node: no existing subnets and one or more new subnets were allocated
		// 2) dual-stack to single-stack conversion: two existing subnets but only one will be valid, and no allocated subnets
		// 3) bad subnet annotation: one more existing subnets will be invalid and might have allocated a correct one
		if len(existingSubnets) != len(validExistingSubnets) || len(allocatedSubnets) > 0 {
			updatedSubnetsMap[networkName] = validExistingSubnets
		}
	}

	// Also update the node annotation if the networkID doesn't match
	if len(updatedSubnetsMap) > 0 || na.networkID != networkID {
		err = na.updateNodeNetworkAnnotationsWithRetry(node.Name, updatedSubnetsMap, na.networkID)
		if err != nil {
			if errR := na.clusterSubnetAllocator.ReleaseNetworks(node.Name, allocatedSubnets...); errR != nil {
				klog.Warningf("Error releasing node %s subnets: %v", node.Name, errR)
			}
			return err
		}
	}

	return nil
}

// HandleDeleteNode handles the delete node event
func (na *NodeAllocator) HandleDeleteNode(node *corev1.Node) error {
	if na.hasHybridOverlayAllocation() {
		na.releaseHybridOverlayNodeSubnet(node.Name)
		return nil
	}

	if na.hasNodeSubnetAllocation() {
		na.clusterSubnetAllocator.ReleaseAllNetworks(node.Name)
		na.recordSubnetCount()
	}

	return nil
}

func (na *NodeAllocator) Sync(nodes []interface{}) error {
	if !na.hasNodeSubnetAllocation() {
		return nil
	}

	defer na.recordSubnetUsage()

	networkName := na.netInfo.GetNetworkName()

	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		if util.NoHostSubnet(node) {
			if na.hasHybridOverlayAllocation() && houtil.IsHybridOverlayNode(node) {
				// this is a hybrid overlay node so mark as allocated from the hybrid overlay subnet allocator
				hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
				if err != nil {
					klog.Errorf("Failed to parse hybrid overlay for node %s: %w", node.Name, err)
				} else if hostSubnet != nil {
					klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnet)
					if err := na.hybridOverlaySubnetAllocator.ReleaseNetworks(node.Name, hostSubnet); err != nil {
						klog.Errorf("Failed to mark the subnet %v as allocated in the hybrid subnet allocator for node %s: %v", hostSubnet, node.Name, err)
					}
				}
			}
		} else {
			hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, networkName)
			if len(hostSubnets) > 0 {
				klog.V(5).Infof("Node %s contains subnets: %v for network : %s", node.Name, hostSubnets, networkName)
				if err := na.clusterSubnetAllocator.MarkAllocatedNetworks(node.Name, hostSubnets...); err != nil {
					klog.Errorf("Failed to mark the subnet %v as allocated in the cluster subnet allocator for node %s: %v", hostSubnets, node.Name, err)
				}
			} else {
				klog.V(5).Infof("Node %s contains no subnets for network : %s", node.Name, networkName)
			}
		}
	}

	return nil
}

// updateNodeNetworkAnnotationsWithRetry will update the node's subnet annotation and network id annotation
func (na *NodeAllocator) updateNodeNetworkAnnotationsWithRetry(nodeName string, hostSubnetsMap map[string][]*net.IPNet, networkId int) error {
	// Retry if it fails because of potential conflict which is transient. Return error in the
	// case of other errors (say temporary API server down), and it will be taken care of by the
	// retry mechanism.
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		node, err := na.nodeLister.Get(nodeName)
		if err != nil {
			return err
		}

		cnode := node.DeepCopy()
		for netName, hostSubnets := range hostSubnetsMap {
			cnode.Annotations, err = util.UpdateNodeHostSubnetAnnotation(cnode.Annotations, hostSubnets, netName)
			if err != nil {
				return fmt.Errorf("failed to update node %q annotation subnet %s",
					node.Name, util.JoinIPNets(hostSubnets, ","))
			}
		}

		networkName := na.netInfo.GetNetworkName()

		cnode.Annotations, err = util.UpdateNetworkIDAnnotation(cnode.Annotations, networkName, networkId)
		if err != nil {
			return fmt.Errorf("failed to update node %q network id annotation %d for network %s",
				node.Name, networkId, networkName)
		}
		// It is possible to update the node annotations using status subresource
		// because changes to metadata via status subresource are not restricted for nodes.
		return na.kube.UpdateNodeStatus(cnode)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update node %s annotation", nodeName)
	}
	return nil
}

// Cleanup the subnet annotations from the node
func (na *NodeAllocator) Cleanup(netName string) error {
	networkName := na.netInfo.GetNetworkName()

	// remove hostsubnet annotation for this network
	klog.Infof("Remove node-subnets annotation for network %s on all nodes", networkName)
	existingNodes, err := na.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error in retrieving the nodes: %v", err)
	}

	for _, node := range existingNodes {
		if util.NoHostSubnet(node) {
			// Secondary network subnet is not allocated for a nohost subnet node
			klog.V(5).Infof("Node %s is not managed by OVN", node.Name)
			continue
		}

		hostSubnetsMap := map[string][]*net.IPNet{networkName: nil}
		// passing util.InvalidNetworkID deletes the network id annotation for the network.
		err = na.updateNodeNetworkAnnotationsWithRetry(node.Name, hostSubnetsMap, util.InvalidNetworkID)
		if err != nil {
			return fmt.Errorf("failed to clear node %q subnet annotation for network %s",
				node.Name, networkName)
		}

		na.clusterSubnetAllocator.ReleaseAllNetworks(node.Name)
	}

	return nil
}

// allocateNodeSubnets either validates existing node subnets against the allocators
// ranges, or allocates new subnets if the node doesn't have any yet, or returns an error
func (na *NodeAllocator) allocateNodeSubnets(allocator SubnetAllocator, nodeName string, existingSubnets []*net.IPNet, ipv4Mode, ipv6Mode bool) ([]*net.IPNet, []*net.IPNet, error) {
	allocatedSubnets := []*net.IPNet{}

	// OVN can work in single-stack or dual-stack only.
	expectedHostSubnets := 1
	// if dual-stack mode we expect one subnet per each IP family
	if ipv4Mode && ipv6Mode {
		expectedHostSubnets = 2
	}

	klog.Infof("Expected %d subnets on node %s, found %d: %v", expectedHostSubnets, nodeName, len(existingSubnets), existingSubnets)

	// If any existing subnets the node has are valid, mark them as reserved.
	// The node might have invalid or already-reserved subnets, or it might
	// have more subnets than configured in OVN (like for dual-stack to/from
	// single-stack conversion).
	// filter in place slice
	// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	foundIPv4 := false
	foundIPv6 := false
	n := 0
	for _, subnet := range existingSubnets {
		if (ipv4Mode && utilnet.IsIPv4CIDR(subnet) && !foundIPv4) || (ipv6Mode && utilnet.IsIPv6CIDR(subnet) && !foundIPv6) {
			if err := allocator.MarkAllocatedNetworks(nodeName, subnet); err == nil {
				klog.Infof("Valid subnet %v allocated on node %s", subnet, nodeName)
				existingSubnets[n] = subnet
				n++
				if utilnet.IsIPv4CIDR(subnet) {
					foundIPv4 = true
				} else if utilnet.IsIPv6CIDR(subnet) {
					foundIPv6 = true
				}
				continue
			}
		}
		// this subnet is no longer needed; release it
		klog.Infof("Releasing unused or invalid subnet %v on node %s", subnet, nodeName)
		if err := allocator.ReleaseNetworks(nodeName, subnet); err != nil {
			klog.Warningf("Failed to release subnet %v on node %s: %v", subnet, nodeName, err)
		}
	}
	// recreate existingSubnets with the valid subnets
	existingSubnets = existingSubnets[:n]

	// Node has enough valid subnets already allocated
	if len(existingSubnets) == expectedHostSubnets {
		klog.Infof("Allowed existing subnets %v on node %s", existingSubnets, nodeName)
		return existingSubnets, allocatedSubnets, nil
	}

	// Release allocated subnets on error
	releaseAllocatedSubnets := true
	defer func() {
		if releaseAllocatedSubnets {
			for _, subnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s", subnet, nodeName)
				if errR := allocator.ReleaseNetworks(nodeName, subnet); errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s: %v", subnet, nodeName, errR)
				}
			}
		}
	}()

	// allocateOneSubnet is a helper to process the result of a subnet allocation
	allocateOneSubnet := func(allocatedHostSubnet *net.IPNet, allocErr error) error {
		if allocErr != nil {
			return fmt.Errorf("error allocating network for node %s: %v", nodeName, allocErr)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s", allocatedHostSubnet, nodeName)
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
		}
		return nil
	}

	// allocate new subnets if needed
	if ipv4Mode && !foundIPv4 {
		if err := allocateOneSubnet(allocator.AllocateIPv4Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}
	if ipv6Mode && !foundIPv6 {
		if err := allocateOneSubnet(allocator.AllocateIPv6Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}

	// check if we were able to allocate the new subnets require
	// this can only happen if OVN is not configured correctly
	// so it will require a reconfiguration and restart.
	wantedSubnets := expectedHostSubnets - len(existingSubnets)
	if wantedSubnets > 0 && len(allocatedSubnets) != wantedSubnets {
		return nil, nil, fmt.Errorf("error allocating networks for node %s: %d subnets expected only new %d subnets allocated",
			nodeName, expectedHostSubnets, len(allocatedSubnets))
	}

	hostSubnets := append(existingSubnets, allocatedSubnets...)
	klog.Infof("Allocated Subnets %v on Node %s", hostSubnets, nodeName)

	// Success; prevent the release-on-error from triggering and return all node subnets
	releaseAllocatedSubnets = false
	return hostSubnets, allocatedSubnets, nil
}

func (na *NodeAllocator) hasNodeSubnetAllocation() bool {
	// we only allocate subnets for L3 secondary network or default network
	return na.netInfo.TopologyType() == types.Layer3Topology || !na.netInfo.IsSecondary()
}
