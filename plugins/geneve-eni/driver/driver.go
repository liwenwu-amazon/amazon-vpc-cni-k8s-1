// Copyright 2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package driver

import (
	"net"
	"syscall"

	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/containernetworking/cni/pkg/ns"
	"github.com/vishvananda/netlink"

	log "github.com/cihub/seelog"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipwrapper"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/netlinkwrapper"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/networkutils"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/nswrapper"
)

const (
	// ip rules priority and leave 512 gap for future
	toContainerRulePriority = 512
	// 1024 is reserved for (ip rule not to <vpc's subnet> table main)
	fromContainerRulePriority = 1536

	// main routing table number
	mainRouteTable = unix.RT_TABLE_MAIN
	// MTU of veth - ENI MTU defined in pkg/networkutils/network.go
	ethernetMTU = 9001

	retryAddLinInterval = 1 * time.Second
)

// NetworkAPIs defines network API calls
type NetworkAPIs interface {
	SetupNS(hostVethName string, contVethName string, geneveName string, brName string, netnsPath string, addr *net.IPNet, table int,
		vpcCIDRs []string, useExternalSNAT bool, cgwIP string, vni string, port int32) error
	TeardownNS(addr *net.IPNet, table int, hostVethName string, contVethName string, geneveName string) error
}

type linuxNetwork struct {
	netLink netlinkwrapper.NetLink
	ns      nswrapper.NS
}

// New creates linuxNetwork object
func New() NetworkAPIs {
	return &linuxNetwork{
		netLink: netlinkwrapper.NewNetLink(),
		ns:      nswrapper.NewNS(),
	}
}

// createVethPairContext wraps the parameters and the method to create the
// veth pair to attach the container namespace
type createVethPairContext struct {
	contVethName string
	hostVethName string
	addr         *net.IPNet
	netLink      netlinkwrapper.NetLink
	ip           ipwrapper.IP
	hwAddr       net.HardwareAddr
}

func newCreateVethPairContext(contVethName string, hostVethName string, hwAddr net.HardwareAddr, addr *net.IPNet) *createVethPairContext {
	return &createVethPairContext{
		contVethName: contVethName,
		hostVethName: hostVethName,
		addr:         addr,
		netLink:      netlinkwrapper.NewNetLink(),
		ip:           ipwrapper.NewIP(),
		hwAddr:       hwAddr,
	}
}

// run defines the closure to execute within the container's namespace to
// create the veth pair
func (createVethContext *createVethPairContext) run(hostNS ns.NetNS) error {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:  createVethContext.contVethName,
			Flags: net.FlagUp,
			MTU:   ethernetMTU,
		},
		PeerName: createVethContext.hostVethName,
	}

	if err := createVethContext.netLink.LinkAdd(veth); err != nil {
		return err
	}

	hostVeth, err := createVethContext.netLink.LinkByName(createVethContext.hostVethName)
	if err != nil {
		return errors.Wrapf(err, "setup NS network: failed to find link %q", createVethContext.hostVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = createVethContext.netLink.LinkSetUp(hostVeth); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", createVethContext.hostVethName)
	}

	contVeth, err := createVethContext.netLink.LinkByName(createVethContext.contVethName)
	if err != nil {
		return errors.Wrapf(err, "setup NS network: failed to find link %q", createVethContext.contVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = createVethContext.netLink.LinkSetUp(contVeth); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", createVethContext.contVethName)
	}

	// Add a connected route to a dummy next hop (169.254.1.1)
	// # ip route show
	// default via 169.254.1.1 dev eth0
	// 169.254.1.1 dev eth0
	gw := net.IPv4(169, 254, 1, 1)
	gwNet := &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}

	if err = createVethContext.netLink.RouteAdd(&netlink.Route{
		LinkIndex: contVeth.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       gwNet}); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add default gateway")
	}

	// Add a default route via dummy next hop(169.254.1.1). Then all outgoing traffic will be routed by this
	// default route via dummy next hop (169.254.1.1).
	if err = createVethContext.ip.AddDefaultRoute(gwNet.IP, contVeth); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add default route")
	}

	if err = createVethContext.netLink.AddrAdd(contVeth, &netlink.Addr{IPNet: createVethContext.addr}); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to add IP addr to %q", createVethContext.contVethName)
	}

	// add static ARP entry for default gateway
	// we are using routed mode on the host and container need this static ARP entry to resolve its default gateway.
	neigh := &netlink.Neigh{
		LinkIndex:    contVeth.Attrs().Index,
		State:        netlink.NUD_PERMANENT,
		IP:           gwNet.IP,
		HardwareAddr: createVethContext.hwAddr,
	}

	if err = createVethContext.netLink.NeighAdd(neigh); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add static ARP")
	}

	// Now that the everything has been successfully set up in the container, move the "host" end of the
	// veth into the host namespace.
	if err = createVethContext.netLink.LinkSetNsFd(hostVeth, int(hostNS.Fd())); err != nil {
		return errors.Wrap(err, "setup NS network: failed to move veth to host netns")
	}
	return nil
}

// SetupNS wires up linux networking for Pod's network
func (os *linuxNetwork) SetupNS(hostVethName string, contVethName string, geneveName string, brName string, netnsPath string,
	addr *net.IPNet, table int, vpcCIDRs []string, useExternalSNAT bool, cgwIP string, vni string, port int32) error {
	log.Debugf("SetupNS: hostVethName=%s,contVethName=%s, netnsPath=%s table=%d\n", hostVethName, contVethName, netnsPath, table)
	return setupNS(hostVethName, contVethName, geneveName, brName, netnsPath, addr, table, vpcCIDRs, useExternalSNAT, os.netLink, os.ns, cgwIP, vni, port)
}

func setupNS(hostVethName string, contVethName string, geneveName string, brName string, netnsPath string, addr *net.IPNet, table int, vpcCIDRs []string,
	useExternalSNAT bool, netLink netlinkwrapper.NetLink, ns nswrapper.NS,
	cgwIP string, vni string, port int32) error {

	portStr := strconv.Itoa(int(port))
	// set up geneve interface
	cmd := exec.Command("ip", "link", "add", "dev", geneveName,
		"type", "geneve", "remote", cgwIP, "vni", vni, "udpcsum", "dstport", portStr)

	cmd.Stdin = strings.NewReader("some input")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		log.Errorf("setupNS: %v", err)
	}

	var geneveVeth netlink.Link
	retry := 0
	for {
		retry++

		if retry > 5 {
			return errors.Wrapf(err, "setup NS network: failed to find link %q", geneveName)
		}

		geneveVeth, err = netLink.LinkByName(geneveName)
		if err != nil {
			log.Errorf("Not able to  Setup NS network: failed to find link %q (attempt %d/%d): %v", geneveName, retry, 5, err)
			time.Sleep(retryAddLinInterval)
			continue
		} else {
			log.Infof("SetupNS network: found %v", geneveName)
			break
		}
	}

	if err = netLink.LinkSetUp(geneveVeth); err != nil {
		log.Errorf("SetupNS network: failed to set link ink %q up", geneveName)
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", geneveName)
	}

	// Clean up if hostVeth exists.
	if oldHostVeth, err := netLink.LinkByName(hostVethName); err == nil {
		if err = netLink.LinkDel(oldHostVeth); err != nil {
			return errors.Wrapf(err, "setup NS network: failed to delete old hostVeth %q", hostVethName)
		}
		log.Debugf("Clean up  old hostVeth: %v\n", hostVethName)
	}

	// generate fake mac address
	phyAddr, err := net.ParseMAC("00:01:02:03:04:05")
	if err != nil {
		log.Errorf("wrong mac: %v", err)

	}

	createVethContext := newCreateVethPairContext(contVethName, hostVethName, phyAddr, addr)
	if err = ns.WithNetNSPath(netnsPath, createVethContext.run); err != nil {
		log.Errorf("Failed to setup NS network %v", err)
		return errors.Wrap(err, "setup NS network: failed to setup NS network")
	}

	hostVeth, err := netLink.LinkByName(hostVethName)
	if err != nil {
		return errors.Wrapf(err, "setup NS network: failed to find link %q", hostVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = netLink.LinkSetUp(hostVeth); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", hostVethName)
	}

	// setup bridge
	br, err := ensureBridge(brName, 1500, true)
	if err != nil {
		log.Errorf("setupNS network, failed to create bridge %q: %v", brName, err)
		return fmt.Errorf("failed to create bridge %q: %v", brName, err)
	}

	// linkt to brige
	// connect host veth end to the bridge
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		log.Errorf("setup NS network: failed to connect %q to bridge %v: %v", hostVeth.Attrs().Name, br.Attrs().Name, err)
		return fmt.Errorf("failed to connect %q to bridge %v: %v", hostVeth.Attrs().Name, br.Attrs().Name, err)
	}

	if err := netlink.LinkSetMaster(geneveVeth, br); err != nil {
		log.Errorf("setup NS network: failed to connect %q to bridge %v: %v", geneveVeth.Attrs().Name, br.Attrs().Name, err)
		return fmt.Errorf("failed to connect %q to bridge %v: %v", geneveVeth.Attrs().Name, br.Attrs().Name, err)
	}

	log.Debugf("Setup host route outgoing hostVeth, LinkIndex %d\n", hostVeth.Attrs().Index)
	addrHostAddr := &net.IPNet{
		IP:   addr.IP,
		Mask: net.CIDRMask(32, 32)}

	// Add host route
	if err = netLink.RouteAdd(&netlink.Route{
		LinkIndex: hostVeth.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       addrHostAddr}); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add host route")
	}
	toContainerFlag := true
	err = addContainerRule(netLink, toContainerFlag, addr, toContainerRulePriority, mainRouteTable)

	if err != nil {
		log.Errorf("Failed to add toContainer rule for %s err=%v, ", addr.String(), err)
		return errors.Wrap(err, "setup NS network: failed to add toContainer")
	}

	log.Infof("Added toContainer rule for %s", addr.String())

	// add from-pod rule, only need it when it is not primary ENI
	if table > 0 {
		if useExternalSNAT {
			// add rule: 1536: from <podIP> use table <table>
			toContainerFlag = false
			err = addContainerRule(netLink, toContainerFlag, addr, fromContainerRulePriority, table)

			if err != nil {
				log.Errorf("Failed to add fromContainer rule for %s err: %v", addr.String(), err)
				return errors.Wrap(err, "add NS network: failed to add fromContainer rule")
			}
			log.Infof("Added rule priority %d from %s table %d", fromContainerRulePriority, addr.String(), table)
		} else {
			// add rule: 1536: list of from <podIP> to <vpcCIDR> use table <table>
			for _, cidr := range vpcCIDRs {
				podRule := netLink.NewRule()
				_, podRule.Dst, _ = net.ParseCIDR(cidr)
				podRule.Src = addr
				podRule.Table = table
				podRule.Priority = fromContainerRulePriority

				err = netLink.RuleAdd(podRule)
				if err != nil {
					log.Errorf("Failed to add pod IP rule: %v", err)
					return errors.Wrapf(err, "UpdateRuleListBySrc: failed to add pod rule")
				}
				var toDst string

				if podRule.Dst != nil {
					toDst = podRule.Dst.String()
				}
				log.Infof("Successfully added pod rule[%v] to %s", podRule, toDst)
			}
		}
	}
	return nil
}

func addContainerRule(netLink netlinkwrapper.NetLink, isToContainer bool, addr *net.IPNet, priority int, table int) error {
	containerRule := netLink.NewRule()

	if isToContainer {
		containerRule.Dst = addr
	} else {
		containerRule.Src = addr
	}
	containerRule.Table = table
	containerRule.Priority = priority

	err := netLink.RuleDel(containerRule)
	if err != nil && !containsNoSuchRule(err) {
		return errors.Wrapf(err, "add NS network: failed to delete old container rule for %s", addr.String())
	}

	err = netLink.RuleAdd(containerRule)
	if err != nil {
		return errors.Wrapf(err, "add NS network: failed to add container rule  for %s", addr.String())
	}
	return nil
}

// TeardownPodNetwork cleanup ip rules
func (os *linuxNetwork) TeardownNS(addr *net.IPNet, table int, hostVethName string, geneveName string, brName string) error {
	log.Debugf("TeardownNS: addr %s, table %d", addr.String(), table)
	return tearDownNS(addr, table, os.netLink, hostVethName, geneveName, brName)
}

func tearDownNS(addr *net.IPNet, table int, netLink netlinkwrapper.NetLink, hostVethName string, geneveName string, brName string) error {

	// remove geneveName from bridge
	geneveVeth, err := netLink.LinkByName(geneveName)

	if err != nil {
		log.Errorf("tearDownNS network: failed to find link %q", geneveName)
	} else {
		err = netlink.LinkSetNoMaster(geneveVeth)
		log.Infof("tearDownNS network: brctl del br intf(geneveVeth: %v), error: %v", geneveName, err)
		err = netlink.LinkDel(geneveVeth)
		log.Infof("tearDownNS network: netlink.LinkDel %v, error: %v", geneveName, err)
	}

	br, err := bridgeByName(brName)

	if err != nil {
		log.Errorf("tearDownNS network: Failed to find bridge %v: error:%v", brName, err)
	} else {
		err = netlink.LinkDel(br)
		log.Infof("tearDownNS network: netlink.LinkDel %v, error: %v", br, err)
	}

	// remove to-pod rule
	toContainerRule := netLink.NewRule()
	toContainerRule.Dst = addr
	toContainerRule.Priority = toContainerRulePriority
	err = netLink.RuleDel(toContainerRule)

	if err != nil {
		log.Errorf("Failed to delete toContainer rule for %s err %v", addr.String(), err)
	} else {
		log.Infof("Delete toContainer rule for %s ", addr.String())
	}

	if table > 0 {
		// remove from-pod rule only for non main table
		err := deleteRuleListBySrc(*addr)
		if err != nil {
			log.Errorf("Failed to delete fromContainer for %s %v", addr.String(), err)
			return errors.Wrapf(err, "delete NS network: failed to delete fromContainer rule for %s", addr.String())
		}
		log.Infof("Delete fromContainer rule for %s in table %d", addr.String(), table)
	}

	addrHostAddr := &net.IPNet{
		IP:   addr.IP,
		Mask: net.CIDRMask(32, 32)}

	// cleanup host route:
	if err = netLink.RouteDel(&netlink.Route{
		Scope: netlink.SCOPE_LINK,
		Dst:   addrHostAddr}); err != nil {
		log.Errorf("delete NS network: failed to delete host route for %s, %v", addr.String(), err)
	}
	return nil
}

func deleteRuleListBySrc(src net.IPNet) error {
	networkClient := networkutils.New()
	return networkClient.DeleteRuleListBySrc(src)
}

func containsNoSuchRule(err error) bool {
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.ENOENT
	}
	return false
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func ensureBridge(brName string, mtu int, promiscMode bool) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}

	err := netlink.LinkAdd(br)
	if err != nil && err != syscall.EEXIST {
		return nil, fmt.Errorf("could not add %q: %v", brName, err)
	}

	if promiscMode {
		if err := netlink.SetPromiscOn(br); err != nil {
			return nil, fmt.Errorf("could not set promiscuous mode on %q: %v", brName, err)
		}
	}

	// Re-fetch link to read all attributes and if it already existed,
	// ensure it's really a bridge with similar configuration
	br, err = bridgeByName(brName)
	if err != nil {
		return nil, err
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	return br, nil
}
