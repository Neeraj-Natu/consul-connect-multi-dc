package router

import (
	"fmt"
	consulapi "github.com/hashicorp/consul/api"
	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
	clientset "github.com/weaveworks/flagger/pkg/client/clientset/versioned"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
)

// ConsulConnectRouter is managing Consul connect splitters
type ConsulConnectRouter struct {
	kubeClient          kubernetes.Interface
	consulClient        *consulapi.Client
	consulClientFactory func(string) *consulapi.Client
	flaggerClient       clientset.Interface
	logger              *zap.SugaredLogger
}

// Reconcile creates or updates the Consul Connect resolver
func (cr *ConsulConnectRouter) Reconcile(canary *flaggerv1.Canary) error {
	err := cr.reconcileResolver(canary)
	if err != nil {
		return err
	}

	err = cr.reconcileSplitter(canary)
	if err != nil {
		return err
	}

	//err = cr.reconcileHealth(canary)
	//if err != nil {
	//	return err
	//}

	return nil
}

func (cr *ConsulConnectRouter) updateSplitter(canary *flaggerv1.Canary, primaryWeight float32, secondaryWeight float32) error {
	apexName, _, _ := canary.GetServiceNames()

	var splits []consulapi.ServiceSplit

	if primaryWeight > 0.001 {
		splits = append(splits,
			consulapi.ServiceSplit{
				Weight:        primaryWeight,
				Service:       apexName,
				ServiceSubset: "primary",
			})
	}

	if secondaryWeight > 0.001 {
		splits = append(splits,
			consulapi.ServiceSplit{
				Weight:        secondaryWeight,
				Service:       apexName,
				ServiceSubset: "canary",
			})
	}

	splitter := &consulapi.ServiceSplitterConfigEntry{
		Kind:   consulapi.ServiceSplitter,
		Name:   apexName,
		Splits: splits,
	}

	_, _, err := cr.consulClient.ConfigEntries().Set(splitter, nil)
	if err != nil {
		return fmt.Errorf("Not able to set service splitter %s.%s error %w", apexName, canary.Namespace, err)
	}

	return nil
}

func (cr *ConsulConnectRouter) reconcileHealth(canary *flaggerv1.Canary) error {
	apexName, _, _ := canary.GetServiceNames()

	err := cr.reconcileHealthCheck(apexName)
	if err != nil {
		cr.logger.Warn("Failed to reconcile health check %v", err)
	}

	err = cr.reconcileExposedPaths(apexName)
	if err != nil {
		cr.logger.Warn("Failed to reconcile health check %v", err)
	}
	return nil
}

func (cr *ConsulConnectRouter) reconcileHealthCheck(apexName string) error {
	proxyName := apexName + "-sidecar-proxy"
	services, _, err := cr.consulClient.Catalog().Service(proxyName, "", nil)
	if err != nil {
		return err
	}

	for _, svc := range services {
		cr.registerCheck(svc)
	}
	return nil
}

func (cr *ConsulConnectRouter) registerCheck(svc *consulapi.CatalogService) {
	checkToAdd := consulapi.AgentServiceCheck{
		CheckID:  "flaggerCheck:" + svc.ServiceID,
		Interval: "10s",
		HTTP:     "http://" + svc.ServiceAddress + ":24999/healthz",
	}

	client := cr.consulClientFactory(svc.Address)

	service, _, err := client.Agent().Service(svc.ServiceID, nil)
	if err != nil {
		cr.logger.Warnf("Failed to fetch service %s", svc.ServiceID)
		return
	}
	if service == nil {
		cr.logger.Infof("Side car proxy is not registered yet")
		return
	}

	checks, err := client.Agent().ChecksWithFilter("ServiceID == \"" + svc.ServiceID + "\"")
	if err != nil {
		cr.logger.Warnf("Failed to fetch checks for %s", svc.ServiceID)
		return
	}

	hasCheck := false
	cr.logger.Infof("Check to add: %s %s", checkToAdd.CheckID, checkToAdd.HTTP)
	for _, v := range checks {
		cr.logger.Infof("Check: %s %s", v.CheckID, v.Definition.HTTP)
		if v.CheckID == checkToAdd.CheckID {
			cr.logger.Infof("Already have health check")
			hasCheck = true
			break
		}
	}

	if !hasCheck {
		registration := cr.newRegistration(service)

		cr.logger.Info("Adding check")
		registration.Check = &checkToAdd

		err = client.Agent().ServiceRegister(&registration)
		if err != nil {
			cr.logger.Warnf("Failed to fetch update %s", service)
			return
		}
	}
}

func (cr *ConsulConnectRouter) newRegistration(service *consulapi.AgentService) consulapi.AgentServiceRegistration {
	registration := consulapi.AgentServiceRegistration{
		Kind:              service.Kind,
		ID:                service.ID,
		Name:              service.Service,
		Tags:              service.Tags,
		Port:              service.Port,
		Address:           service.Address,
		TaggedAddresses:   service.TaggedAddresses,
		EnableTagOverride: service.EnableTagOverride,
		Meta:              service.Meta,
		Weights:           &service.Weights,
		Proxy:             service.Proxy,
		Connect:           service.Connect,
		Namespace:         service.Namespace,
	}
	return registration
}

func (cr *ConsulConnectRouter) reconcileExposedPaths(apexName string) error {
	proxyName := apexName + "-sidecar-proxy"

	services, _, err := cr.consulClient.Catalog().Service(proxyName, "", nil)
	if err != nil {
		return err
	}

	for _, svc := range services {
		cr.registerExposedPath(svc)
	}
	return nil
}

func (cr *ConsulConnectRouter) registerExposedPath(svc *consulapi.CatalogService) {
	pathToAdd := consulapi.ExposePath{
		ListenerPort:  24999,
		Path:          "/healthz",
		LocalPathPort: svc.ServiceProxy.LocalServicePort,
		Protocol:      "http",
	}

	client := cr.consulClientFactory(svc.Address)

	cr.logger.Infof("Fetching %s", svc.ServiceID)
	service, _, err := client.Agent().Service(svc.ServiceID, nil)
	if err != nil {
		cr.logger.Warnf("Failed to fetch service %s", svc.ServiceID)
		return
	}
	if service == nil {
		cr.logger.Infof("Side car proxy is not registered yet")
		return
	}

	hasPath := false
	if service.Proxy != nil {
		cr.logger.Infof("Paths: %s", service.Proxy.Expose.Paths)
		for _, v := range service.Proxy.Expose.Paths {
			if comparePaths(v, pathToAdd) {
				cr.logger.Infof("Already have /healthz exposed")
				hasPath = true
				break
			}
		}
	}

	if !hasPath {
		cr.logger.Info("Adding exposed path")
		if service.Proxy == nil {
			service.Proxy = &consulapi.AgentServiceConnectProxyConfig{
			}
		}
		service.Proxy.Expose.Paths = append(service.Proxy.Expose.Paths, pathToAdd)

		registration := cr.newRegistration(service)

		err = client.Agent().ServiceRegister(&registration)
		if err != nil {
			cr.logger.Warnf("Failed to fetch update %s", service)
			return
		}
	}
}

func comparePaths(a consulapi.ExposePath, b consulapi.ExposePath) bool {
	return a.ListenerPort == b.ListenerPort &&
		a.LocalPathPort == a.LocalPathPort &&
		a.Path == a.Path &&
		a.Protocol == a.Protocol
}

func (cr *ConsulConnectRouter) reconcileResolver(canary *flaggerv1.Canary) error {
	apexName, primaryName, _ := canary.GetServiceNames()

	dcs, err := cr.consulClient.Catalog().Datacenters()
	if err != nil {
		cr.logger.Warnf("Failed to fetch dc list %v", err)
		dcs = make([]string, 0)
	}
	if len(dcs) >= 1 {
		dcs = dcs[1:]
	}

	resolver := &consulapi.ServiceResolverConfigEntry{
		Kind:          consulapi.ServiceResolver,
		Name:          apexName,
		DefaultSubset: "primary",
		Subsets: map[string]consulapi.ServiceResolverSubset{
			"primary": {
				Filter: "Service.ID matches \"" + primaryName + "-.+\"",
			},
			"canary": {
				Filter: "Service.ID not matches \"" + primaryName + "-.+\"",
			},
		},
	}

	if len(dcs) > 0 {
		resolver.Failover = make(map[string]consulapi.ServiceResolverFailover)
		resolver.Failover["primary"] = consulapi.ServiceResolverFailover{
			Service:       apexName,
			ServiceSubset: "primary",
			Datacenters:   dcs,
		}

		resolver.Failover["canary"] = consulapi.ServiceResolverFailover{
			Service:       apexName,
			ServiceSubset: "canary",
			Datacenters:   dcs,
		}
	}

	result, _, err := cr.consulClient.ConfigEntries().Set(resolver, nil)
	if err != nil {
		return fmt.Errorf("Failure during creation of service resolver %s.%s error: %w", apexName, canary.Namespace, err)
	}

	if !result {
		return fmt.Errorf("Not able to create service resolver %s.%s", apexName, canary.Namespace)
	}

	return nil
}

func (cr *ConsulConnectRouter) reconcileSplitter(canary *flaggerv1.Canary) error {
	apexName, _, _ := canary.GetServiceNames()

	_, _, err := cr.consulClient.ConfigEntries().Get(consulapi.ServiceSplitter, apexName, nil)
	if err != nil {
		err = cr.updateSplitter(canary, 100.0, 0.0)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetRoutes returns the destinations weight for primary and canary
func (cr *ConsulConnectRouter) GetRoutes(canary *flaggerv1.Canary) (
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
	err error,
) {
	apexName, _, _ := canary.GetServiceNames()

	entry, _, err := cr.consulClient.ConfigEntries().Get(consulapi.ServiceSplitter, apexName, nil)

	if err != nil {
		err = fmt.Errorf("Service splitter %s.%s not found error %w", apexName, canary.Namespace, err)
		return
	}

	readSplitter, ok := entry.(*consulapi.ServiceSplitterConfigEntry)
	if !ok {
		err = fmt.Errorf("Bad service splitter %s.%s", apexName, canary.Namespace)
		return
	}

	for _, split := range readSplitter.Splits {
		if split.ServiceSubset == "primary" {
			primaryWeight = int(split.Weight)
		}
		if split.ServiceSubset == "canary" {
			canaryWeight = int(split.Weight)
		}
	}

	if primaryWeight == 0 && canaryWeight == 0 {
		err = fmt.Errorf("Service splitter %s.%s does not contain routes for %s-primary and %s-canary",
			apexName, canary.Namespace, apexName, apexName)
	}

	return
}

// SetRoutes updates the destinations weight for primary and canary
func (cr *ConsulConnectRouter) SetRoutes(
	canary *flaggerv1.Canary,
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
) error {
	return cr.updateSplitter(canary, float32(primaryWeight), float32(canaryWeight))
}

func (cr *ConsulConnectRouter) UpdateConsulConfig(name string, kind string, updateFn func(config consulapi.ConfigEntry) (bool, error)) error {
	for true {
		entry, meta, err := cr.consulClient.ConfigEntries().Get(kind, name, nil)
		if err != nil {
			return err
		}

		ok, err := updateFn(entry)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		ok, _, err = cr.consulClient.ConfigEntries().CAS(entry, meta.LastIndex, nil)
		if err != nil {
			return err
		}

		if ok {
			return nil
		}
	}
	return nil
}
