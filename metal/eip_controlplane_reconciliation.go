package metal

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
	"k8s.io/klog/v2"
)

const (
	controlPlaneLabel        = "node-role.kubernetes.io/master"
	externalServiceName      = "cloud-provider-equinix-metal-kubernetes-external"
	externalServiceNamespace = "kube-system"
	metallbAnnotation        = "metallb.universe.tf/address-pool"
	metallbDisabledtag       = "disabled-metallb-do-not-use-any-address-pool"
)

/*
 controlPlaneEndpointManager checks the availability of an elastic IP for
 the control plane and if it exists the reconciliation guarantees that it is
 attached to a healthy control plane.

 The general steps are:
 1. Check if the passed ElasticIP tags returns a valid Elastic IP via Equinix Metal API.
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
	inProcess         bool
	apiServerPort     int32 // node on which the EIP is listening
	nodeAPIServerPort int32 // port on which the api server is listening on the control plane nodes
	eipTag            string
	instances         cloudInstances
	deviceIPSrv       packngo.DeviceIPService
	ipResSvr          packngo.ProjectIPService
	projectID         string
	httpClient        *http.Client
	k8sclient         kubernetes.Interface
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

func (m *controlPlaneEndpointManager) reconcileNodes(ctx context.Context, nodes []*v1.Node, mode UpdateMode) error {
	klog.V(2).Info("controlPlaneEndpoint.reconcile: new reconciliation")
	if m.inProcess {
		klog.V(2).Info("controlPlaneEndpoint.reconcileNodes: already in process, not starting a new one")
		return nil
	}
	// must have figured out the node port first, or nothing to do
	if m.apiServerPort == 0 {
		return errors.New("control plane apiserver port not provided or determined, cannot check, will try again on next loop")
	}
	m.inProcess = true
	defer func() {
		m.inProcess = false
	}()
	if m.eipTag == "" {
		return errors.New("control plane loadbalancer elastic ip tag is empty. Nothing to do")
	}
	ipList, _, err := m.ipResSvr.List(m.projectID, &packngo.ListOptions{
		Includes: []string{"assignments"},
	})
	if err != nil {
		return err
	}
	controlPlaneEndpoint := ipReservationByAllTags([]string{m.eipTag}, ipList)
	if controlPlaneEndpoint == nil {
		// IP NOT FOUND nothing to do here.
		klog.Errorf("elastic IP not found. Please verify you have one with the expected tag: %s", m.eipTag)
		return err
	}
	if len(controlPlaneEndpoint.Assignments) > 1 {
		return fmt.Errorf("the elastic ip %s has more than one node assigned to it and this is currently not supported. Fix it manually unassigning devices", controlPlaneEndpoint.ID)
	}
	healthCheckURL := fmt.Sprintf("https://%s:%d/healthz", controlPlaneEndpoint.Address, m.apiServerPort)
	klog.Infof("healthcheck elastic ip %s", healthCheckURL)
	req, err := http.NewRequest("GET", healthCheckURL, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient.Do(req)
	// if there was no error, ensure we close
	if err == nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusOK {
		if err != nil {
			klog.Errorf("http client error during healthcheck, will try to reassign to a healthy node. err \"%s\"", err)
		}
		// filter down to only those nodes that are tagged as control plane
		cpNodes := []*v1.Node{}
		for _, n := range nodes {
			if _, ok := n.Labels[controlPlaneLabel]; ok {
				cpNodes = append(cpNodes, n)
				klog.V(2).Infof("adding control plane node %s", n.Name)
			}
		}
		if err := m.reassign(ctx, cpNodes, controlPlaneEndpoint, healthCheckURL); err != nil {
			klog.Errorf("error reassigning control plane endpoint to a different device. err \"%s\"", err)
			return err
		}
	}
	return nil
}

func (m *controlPlaneEndpointManager) reassign(ctx context.Context, nodes []*v1.Node, ip *packngo.IPAddressReservation, eipURL string) error {
	klog.V(2).Info("controlPlaneEndpoint.reassign")
	// must have figured out the node port first, or nothing to do
	if m.nodeAPIServerPort == 0 {
		return errors.New("control plane node apiserver port not yet determined, cannot reassign, will try again on next loop")
	}
	for _, node := range nodes {
		addresses, err := m.instances.NodeAddresses(ctx, types.NodeName(node.Name))
		if err != nil {
			return err
		}

		// I decided to iterate over all the addresses assigned to the node to avoid network misconfiguration
		// The first one for example is the node name, and if the hostname is not well configured it will never work.
		for _, a := range addresses {
			if a.Type == "Hostname" {
				klog.V(2).Infof("skipping address check of type %s: %s", a.Type, a.Address)
				continue
			}
			healthCheckAddress := fmt.Sprintf("https://%s:%d/healthz", a.Address, m.nodeAPIServerPort)
			if healthCheckAddress == eipURL {
				klog.V(2).Infof("skipping address check for EIP on this node: %s", eipURL)
				continue
			}
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
				klog.Infof("control plane endpoint assigned to new device %s", node.Name)
				return nil
			}
			klog.Infof("will not assign control plane endpoint to new device %s: returned http code %d", node.Name, resp.StatusCode)
		}
	}
	return errors.New("ccm didn't find a good candidate for IP allocation. Cluster is unhealthy")
}

func newControlPlaneEndpointManager(eipTag, projectID string, deviceIPSrv packngo.DeviceIPService, ipResSvr packngo.ProjectIPService, i cloudInstances, apiServerPort int32) *controlPlaneEndpointManager {
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
func (m *controlPlaneEndpointManager) reconcileServices(ctx context.Context, svcs []*v1.Service, mode UpdateMode) error {
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
	controlPlaneEndpoint := ipReservationByAllTags([]string{m.eipTag}, ipList)
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

		// get the target port
		existingPorts := svc.Spec.Ports
		if len(existingPorts) < 1 {
			return errors.New("default/kubernetes service does not have any ports defined")
		}

		// track which port the kube-apiserver actually is listening on
		m.nodeAPIServerPort = existingPorts[0].TargetPort.IntVal
		// did we set a specific port, or did we request that it just be left as is?
		if m.apiServerPort == 0 {
			m.apiServerPort = m.nodeAPIServerPort
		}

		// get the endpoints for this service
		eps := m.k8sclient.CoreV1().Endpoints(svc.Namespace)
		ep, err := eps.Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil {
			klog.V(2).Infof("failed to get endpoints %s: %v", svc.Name, err)
			return fmt.Errorf("failed to get endpoints %s: %v", svc.Name, err)
		}
		// two options:
		// - our endpoints already exists: just copy the endpoints
		// - our endpoints does not exist: create it
		epExisted := true
		myeps := m.k8sclient.CoreV1().Endpoints(externalServiceNamespace)
		myep, err := myeps.Get(ctx, externalServiceName, metav1.GetOptions{})
		if err != nil {
			klog.Infof("endpoint %s/%s did not yet exist, creating", externalServiceNamespace, externalServiceName)
			myep = &v1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:      externalServiceName,
					Namespace: externalServiceNamespace,
				},
			}
			epExisted = false
		}

		myep.Subsets = []v1.EndpointSubset{}
		for _, s := range ep.Subsets {
			copiedSubset := s.DeepCopy()
			myep.Subsets = append(myep.Subsets, *copiedSubset)
		}

		// save the endpoints
		if epExisted {
			if _, err := myeps.Update(ctx, myep, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("failed to update my endpoints: %v", err)
				return fmt.Errorf("failed to update my endpoints: %v", err)
			}
		} else {
			if _, err := myeps.Create(ctx, myep, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create my endpoints: %v", err)
				return fmt.Errorf("failed to create my endpoints: %v", err)
			}
		}

		// now for my service
		ports := []v1.ServicePort{}
		for _, p := range existingPorts {
			copiedPort := p.DeepCopy()
			ports = append(ports, *copiedPort)
		}
		// set the port on which to listen
		ports[0].Port = m.apiServerPort

		externalService := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: externalServiceName,
				Annotations: map[string]string{
					metallbAnnotation: metallbDisabledtag,
				},
				Namespace: externalServiceNamespace,
			},
			Spec: v1.ServiceSpec{
				Type:           v1.ServiceTypeLoadBalancer,
				LoadBalancerIP: eip,
				Ports:          ports,
			},
		}

		// did it already exist? Then update it
		svcIntf := m.k8sclient.CoreV1().Services(externalServiceNamespace)
		var updatedService *v1.Service
		if updatedService, err = svcIntf.Get(ctx, externalServiceName, metav1.GetOptions{}); err == nil {
			klog.V(2).Infof("service %s already exists, just updating", externalServiceName)
			// we do not want to override everything, as there is important information we need
			updatedService.Spec.LoadBalancerIP = externalService.Spec.LoadBalancerIP
			updatedService.Spec.Ports = externalService.Spec.Ports
			if _, err := svcIntf.Update(ctx, updatedService, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("failed to update service: %v", err)
				return fmt.Errorf("failed to update service: %v", err)
			}
		} else {
			klog.V(2).Infof("service %s did not exist, creating", externalServiceName)
			if updatedService, err = svcIntf.Create(ctx, externalService, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create service: %v", err)
				return fmt.Errorf("failed to create service: %v", err)
			}
		}
		if updatedService, err = svcIntf.Get(ctx, externalServiceName, metav1.GetOptions{}); err != nil {
			klog.Errorf("could not get service %s for status update: %v", externalServiceName, err)
			return fmt.Errorf("could not get service %s for status update: %v", externalServiceName, err)
		}
		// and finally update status
		updatedService.Status = v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: eip},
				},
			},
		}
		if _, err := svcIntf.UpdateStatus(ctx, updatedService, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("failed to update service status: %v", err)
			return fmt.Errorf("failed to update service status: %v", err)
		}
		return nil
	}
	// every sync should find default/kubernetes
	if mode == ModeSync {
		return fmt.Errorf("Service default/kubernetes not found")
	}
	return nil
}
