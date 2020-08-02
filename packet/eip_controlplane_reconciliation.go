package packet

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"errors"

	"github.com/packethost/packngo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

const (
	controlPlaneLabel = "node-role.kubernetes.io/master"
)

/*
 controlPlaneEndpointManager checks the availability of an elastic IP for
 the control plane and if it exists the reconciliation guarantees that it is
 attached to a healthy control plane.

 The general steps are:
 1. Check if the passed ElasticIP tags returns a valid Elastic IP via Packet API.
 2. If there is NOT an ElasticIP with those tags just end the reconciliation
 3. If there is an ElasticIP use the kubernetes client-go to check if it
 returns a valid response
 4. If the response returned via client-go is good we do not need to do anything
 5. If the response if wrong or it terminated it means that the device behind
 the ElasticIP is not working correctly and we have to find a new one.
 6. Ping the other control plane available in the cluster, if one of them work
 assign the ElasticIP to that device.
 7. If NO Control Planes succeed, the cluster is unhealthy and the
 reconciliation terminates without changing the current state of the system.
*/
type controlPlaneEndpointManager struct {
	inProcess     bool
	apiServerPort int
	eipTag        string
	instances     cloudInstances
	deviceIPSrv   packngo.DeviceIPService
	ipResSvr      packngo.ProjectIPService
	projectID     string
	httpClient    *http.Client
	k8sclient     kubernetes.Interface
}

func (m *controlPlaneEndpointManager) name() string {
	return "controlPlaneEndpointManager"
}

func (m *controlPlaneEndpointManager) init(k8sclient kubernetes.Interface) error {
	m.k8sclient = k8sclient
	klog.V(2).Info("controlPlaneEndpointManager.init(): enabling BGP on project")
	return nil
}

func (m *controlPlaneEndpointManager) nodeReconciler() nodeReconciler {
	return m.reconcileNodes
}
func (m *controlPlaneEndpointManager) serviceReconciler() serviceReconciler {
	return m.reconcileServices
}

func (m *controlPlaneEndpointManager) reconcileNodes(nodes []*v1.Node, remove bool) error {
	klog.V(2).Info("controlPlaneEndpoint.reconcile: new reconciliation")
	if m.inProcess {
		klog.V(2).Info("controlPlaneEndpoint.reconcileNodes: already in process, not starting a new one")
		return nil
	}
	m.inProcess = true
	defer func() {
		m.inProcess = false
	}()
	if m.eipTag == "" {
		return errors.New("elastic ip tag is empty. Nothing to do")
	}
	ipList, _, err := m.ipResSvr.List(m.projectID, &packngo.ListOptions{
		Includes: []string{"assignments"},
	})
	if err != nil {
		return err
	}
	controlPlaneEndpoint := ipReservationByTags([]string{m.eipTag}, ipList)
	if controlPlaneEndpoint == nil {
		// IP NOT FOUND nothing to do here.
		klog.Errorf("elastic IP not found. Please verify you have one with the expected tag: %s", m.eipTag)
		return err
	}
	if len(controlPlaneEndpoint.Assignments) > 1 {
		return fmt.Errorf("the elastic ip %s has more than one node assigned to it and this is currently not supported. Fix it manually unassigning devices", controlPlaneEndpoint.ID)
	}
	klog.Infof("healthcheck elastic ip %s", fmt.Sprintf("https://%s:%d/healthz", controlPlaneEndpoint.Address, m.apiServerPort))
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s:%d/healthz", controlPlaneEndpoint.Address, m.apiServerPort), nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err != nil {
			klog.Errorf("http client error during healthcheck. err \"%s\"", err)
		}
		// filter down to only those nodes that are tagged as control plane
		cpNodes := []*v1.Node{}
		for _, n := range nodes {
			if _, ok := n.Labels[controlPlaneLabel]; ok {
				cpNodes = append(cpNodes, n)
				klog.V(2).Infof("adding control plane node %s", n.Name)
			}
		}
		if err := m.reassign(context.Background(), cpNodes, controlPlaneEndpoint); err != nil {
			klog.Errorf("error reassigning control plane endpoint to a different device. err \"%s\"", err)
			return err
		}
	}
	defer resp.Body.Close()
	return nil
}

func (m *controlPlaneEndpointManager) reassign(ctx context.Context, nodes []*v1.Node, ip *packngo.IPAddressReservation) error {
	klog.V(2).Info("controlPlaneEndpoint.reassign")
	for _, node := range nodes {
		addresses, err := m.instances.NodeAddresses(ctx, types.NodeName(node.Name))
		if err != nil {
			return err
		}

		// I decided to iterate over all the addresses assigned to the node to avoid network missconfiguration
		// The first one for example is the node name, and if the hostname is not well configured it will never work.
		for _, a := range addresses {
			if a.Type == "Hostname" {
				klog.V(2).Infof("skipping address check of type Hostname: %s", a.Address)
				continue
			}
			healthCheckAddress := fmt.Sprintf("https://%s:%d/healthz", a.Address, m.apiServerPort)
			klog.Infof("healthcheck node %s", healthCheckAddress)
			req, err := http.NewRequest("GET", healthCheckAddress, nil)
			if err != nil {
				klog.Errorf("healthcheck failed for node %s. err \"%s\"", node.Name, err)
				continue
			}
			resp, err := m.httpClient.Do(req)

			if err != nil {
				if err != nil {
					klog.Errorf("http client error during healthcheck. err \"%s\"", err)
				}
				continue
			}

			// We have a healthy node, this is the candidate to receive the EIP
			if resp.StatusCode == http.StatusOK {
				deviceID, err := m.instances.InstanceID(ctx, types.NodeName(node.Name))
				if err != nil {
					return err
				}
				if len(ip.Assignments) == 1 {
					if _, err := m.deviceIPSrv.Unassign(ip.Assignments[0].ID); err != nil {
						return err
					}
				}
				if _, _, err := m.deviceIPSrv.Assign(deviceID, &packngo.AddressStruct{
					Address: ip.Address,
				}); err != nil {
					return err
				}
				klog.V(2).Infof("control plane endpoint assigned to new device %s", node.Name)
				return nil
			}
		}
	}
	return errors.New("ccm didn't find a good candidate for IP allocation. Cluster is unhealthy")
}

func newControlPlaneEndpointManager(eipTag, projectID string, deviceIPSrv packngo.DeviceIPService, ipResSvr packngo.ProjectIPService, i cloudInstances, apiServerPort int) *controlPlaneEndpointManager {
	return &controlPlaneEndpointManager{
		httpClient: &http.Client{
			Timeout: time.Second * 5,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}},
		eipTag:        eipTag,
		projectID:     projectID,
		instances:     i,
		ipResSvr:      ipResSvr,
		deviceIPSrv:   deviceIPSrv,
		apiServerPort: apiServerPort,
	}
}

// reconcileServices ensure that our Elastic IP is assigned as `externalIPs` for
// the `default/kubernetes` service
func (m *controlPlaneEndpointManager) reconcileServices(svcs []*v1.Service, remove bool) error {
	if m.eipTag == "" {
		return errors.New("elastic ip tag is empty. Nothing to do")
	}

	var err error
	// get IP address reservations and check if they any exists for this svc
	ipList, _, err := m.ipResSvr.List(m.projectID, &packngo.ListOptions{
		Includes: []string{"assignments"},
	})
	if err != nil {
		return err
	}
	controlPlaneEndpoint := ipReservationByTags([]string{m.eipTag}, ipList)
	if controlPlaneEndpoint == nil {
		// IP NOT FOUND nothing to do here.
		klog.Errorf("elastic IP not found. Please verify you have one with the expected tag: %s", m.eipTag)
		return err
	}
	if len(controlPlaneEndpoint.Assignments) > 1 {
		return fmt.Errorf("the elastic ip %s has more than one node assigned to it and this is currently not supported. Fix it manually unassigning devices", controlPlaneEndpoint.ID)
	}

	// for ease of use
	eip := controlPlaneEndpoint.Address

	for _, svc := range svcs {
		// only take default/kubernetes
		if svc.Namespace != "default" || svc.Name != "kubernetes" {
			continue
		}

		for _, ip := range svc.Spec.ExternalIPs {
			if ip == eip {
				klog.V(2).Infof("EIP %s already as external IP, nothing to do", eip)
				return nil
			}
		}

		// if we made it here, there was no match, so add it

		// and save
		intf := m.k8sclient.CoreV1().Services(svc.Namespace)
		existing, err := intf.Get(svc.Name, metav1.GetOptions{})
		if err != nil || existing == nil {
			klog.V(2).Infof("failed to get latest for service %s: %v", svc.Name, err)
			return fmt.Errorf("failed to get latest for service %s: %v", svc.Name, err)
		}
		klog.V(2).Infof("adding EIP %s", eip)
		existing.Spec.ExternalIPs = append(svc.Spec.ExternalIPs, eip)

		_, err = intf.Update(existing)
		return err
	}
	return fmt.Errorf("Service default/kubernetes not found")
}
