/*
 * Copyright The Kmesh Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package workload

import (
	"fmt"
	"os"
	"strings"

	service_discovery_v3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"kmesh.net/kmesh/api/v2/workloadapi"
	"kmesh.net/kmesh/api/v2/workloadapi/security"
	"kmesh.net/kmesh/bpf/kmesh/bpf2go"
	"kmesh.net/kmesh/pkg/auth"
	"kmesh.net/kmesh/pkg/constants"
	"kmesh.net/kmesh/pkg/controller/config"
	bpf "kmesh.net/kmesh/pkg/controller/workload/bpfcache"
	"kmesh.net/kmesh/pkg/controller/workload/cache"
	"kmesh.net/kmesh/pkg/nets"
)

const (
	LbPolicyRandom    = 0
	KmeshWaypointPort = 15019 // use this fixed port instead of the HboneMtlsPort in kmesh
)

type Processor struct {
	ack *service_discovery_v3.DeltaDiscoveryRequest
	req *service_discovery_v3.DeltaDiscoveryRequest

	hashName *HashName
	// workloads indexer, svc key -> workload id
	endpointsByService map[string]map[string]struct{}
	bpf                *bpf.Cache
	nodeName           string
	WorkloadCache      cache.WorkloadCache
	ServiceCache       cache.ServiceCache
}

func newProcessor(workloadMap bpf2go.KmeshCgroupSockWorkloadMaps) *Processor {
	return &Processor{
		hashName:           NewHashName(),
		endpointsByService: make(map[string]map[string]struct{}),
		bpf:                bpf.NewCache(workloadMap),
		nodeName:           os.Getenv("NODE_NAME"),
		WorkloadCache:      cache.NewWorkloadCache(),
		ServiceCache:       cache.NewServiceCache(),
	}
}

func newDeltaRequest(typeUrl string, names []string, initialResourceVersions map[string]string) *service_discovery_v3.DeltaDiscoveryRequest {
	return &service_discovery_v3.DeltaDiscoveryRequest{
		TypeUrl:                 typeUrl,
		ResourceNamesSubscribe:  names,
		InitialResourceVersions: initialResourceVersions,
		ResponseNonce:           "",
		ErrorDetail:             nil,
		Node:                    config.GetConfig(constants.WorkloadMode).GetNode(),
	}
}

func newAckRequest(rsp *service_discovery_v3.DeltaDiscoveryResponse) *service_discovery_v3.DeltaDiscoveryRequest {
	return &service_discovery_v3.DeltaDiscoveryRequest{
		TypeUrl:                rsp.GetTypeUrl(),
		ResourceNamesSubscribe: []string{},
		ResponseNonce:          rsp.GetNonce(),
		ErrorDetail:            nil,
		Node:                   config.GetConfig(constants.WorkloadMode).GetNode(),
	}
}

func (p *Processor) processWorkloadResponse(rsp *service_discovery_v3.DeltaDiscoveryResponse, rbac *auth.Rbac) {
	var err error

	p.ack = newAckRequest(rsp)
	switch rsp.GetTypeUrl() {
	case AddressType:
		err = p.handleAddressTypeResponse(rsp)
	case AuthorizationType:
		err = p.handleAuthorizationTypeResponse(rsp, rbac)
	default:
		err = fmt.Errorf("unsupported type url %s", rsp.GetTypeUrl())
	}
	if err != nil {
		log.Error(err)
	}
}

func (p *Processor) deletePodFrontendData(uid uint32) error {
	var (
		bk = bpf.BackendKey{}
		bv = bpf.BackendValue{}
		fk = bpf.FrontendKey{}
	)

	bk.BackendUid = uid
	if err := p.bpf.BackendLookup(&bk, &bv); err == nil {
		log.Debugf("Find BackendValue: [%#v]", bv)
		fk.Ip = bv.Ip
		if err = p.bpf.FrontendDelete(&fk); err != nil {
			log.Errorf("FrontendDelete failed: %s", err)
			return err
		}
	}

	return nil
}

func (p *Processor) storePodFrontendData(uid uint32, ip []byte) error {
	var (
		fk = bpf.FrontendKey{}
		fv = bpf.FrontendValue{}
	)

	nets.CopyIpByteFromSlice(&fk.Ip, ip)

	fv.UpstreamId = uid
	if err := p.bpf.FrontendUpdate(&fk, &fv); err != nil {
		log.Errorf("Update frontend map failed, err:%s", err)
		return err
	}

	return nil
}

func (p *Processor) removeWorkloadResource(removedResources []string) error {
	for _, uid := range removedResources {
		p.WorkloadCache.DeleteWorkload(uid)
		if err := p.removeWorkloadFromBpfMap(uid); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) removeWorkloadFromBpfMap(uid string) error {
	var (
		err      error
		bkDelete = bpf.BackendKey{}
	)

	backendUid := p.hashName.StrToNum(uid)
	// for Pod to Pod access, Pod info stored in frontend map, when Pod offline, we need delete the related records
	if err = p.deletePodFrontendData(backendUid); err != nil {
		log.Errorf("deletePodFrontendData failed: %s", err)
		return err
	}

	// 1. find all endpoint keys related to this workload
	if eks := p.bpf.EndpointIterFindKey(backendUid); len(eks) != 0 {
		err = p.deleteEndpointRecords(eks)
		if err != nil {
			goto failed
		}
	}

	bkDelete.BackendUid = backendUid
	if err = p.bpf.BackendDelete(&bkDelete); err != nil {
		log.Errorf("BackendDelete failed: %s", err)
		goto failed
	}
	p.hashName.Delete(uid)

failed:
	return err
}

func (p *Processor) deleteFrontendData(id uint32) error {
	var (
		err error
		fk  = bpf.FrontendKey{}
	)
	if fks := p.bpf.FrontendIterFindKey(id); len(fks) != 0 {
		log.Debugf("Find Key Count %d", len(fks))
		for _, fk = range fks {
			log.Debugf("deleteFrontendData Key [%#v]", fk)
			if err = p.bpf.FrontendDelete(&fk); err != nil {
				log.Errorf("FrontendDelete failed: %s", err)
				return err
			}
		}
	}

	return nil
}

func (p *Processor) removeServiceResource(resources []string) error {
	var (
		err      error
		skDelete = bpf.ServiceKey{}
		svDelete = bpf.ServiceValue{}
		ekDelete = bpf.EndpointKey{}
	)

	for _, name := range resources {
		p.ServiceCache.DeleteService(name)
		serviceId := p.hashName.StrToNum(name)
		skDelete.ServiceId = serviceId
		if err = p.bpf.ServiceLookup(&skDelete, &svDelete); err == nil {
			if err = p.deleteFrontendData(serviceId); err != nil {
				log.Errorf("deleteFrontendData failed: %s", err)
				goto failed
			}

			if err = p.bpf.ServiceDelete(&skDelete); err != nil {
				log.Errorf("ServiceDelete failed: %s", err)
				goto failed
			}

			var i uint32
			for i = 1; i <= svDelete.EndpointCount; i++ {
				ekDelete.ServiceId = serviceId
				ekDelete.BackendIndex = i
				if err = p.bpf.EndpointDelete(&ekDelete); err != nil {
					log.Errorf("EndpointDelete failed: %s", err)
					goto failed
				}
			}
		}
		p.hashName.Delete(name)
	}

failed:
	return err
}

func (p *Processor) storeEndpointWithService(sk *bpf.ServiceKey, sv *bpf.ServiceValue, uid uint32) error {
	var (
		err error
		ek  = bpf.EndpointKey{}
		ev  = bpf.EndpointValue{}
	)
	sv.EndpointCount++
	ek.BackendIndex = sv.EndpointCount
	ek.ServiceId = sk.ServiceId
	ev.BackendUid = uid
	if err = p.bpf.EndpointUpdate(&ek, &ev); err != nil {
		log.Errorf("Update endpoint map failed, err:%s", err)
		return err
	}
	if err = p.bpf.ServiceUpdate(sk, sv); err != nil {
		log.Errorf("Update ServiceUpdate map failed, err:%s", err)
		return err
	}

	return nil
}

func (p *Processor) storeServiceEndpoint(workload_uid string, serviceName string) {
	wls, ok := p.endpointsByService[serviceName]
	if !ok {
		p.endpointsByService[serviceName] = make(map[string]struct{})
		wls = p.endpointsByService[serviceName]
	}

	wls[workload_uid] = struct{}{}
}

func (p *Processor) deleteResidualServicesWithWorkload(workload *workloadapi.Workload, services []string) error {
	var (
		err       error
		serviceId uint32
	)

	if len(services) == 0 {
		return nil
	}

	serviceIds := make(map[uint32]string)
	for _, serviceName := range services {
		serviceId = p.hashName.StrToNum(serviceName)
		serviceIds[serviceId] = serviceName
	}

	workloadUid := p.hashName.StrToNum(workload.GetUid())
	eks := p.bpf.GetServiceEndpointKey(serviceIds, workloadUid)
	err = p.deleteEndpointRecords(eks)
	if err != nil {
		log.Errorf("removeResidualServices delete endpoint failed:%v", err)
	}
	return err
}

func (p *Processor) addNewServicesWithWorkload(workload *workloadapi.Workload, newServices []string) error {
	var (
		err error
		sk  = bpf.ServiceKey{}
		sv  = bpf.ServiceValue{}
	)

	backend_uid := p.hashName.StrToNum(workload.GetUid())
	for _, serviceName := range newServices {
		sk.ServiceId = p.hashName.StrToNum(serviceName)
		// the service already stored in map, add endpoint
		if err = p.bpf.ServiceLookup(&sk, &sv); err == nil {
			if err = p.storeEndpointWithService(&sk, &sv, backend_uid); err != nil {
				log.Errorf("storeEndpointWithService failed, err:%s", err)
				return err
			}
		} else {
			p.storeServiceEndpoint(workload.GetUid(), serviceName)
		}
	}
	return nil
}

func (p *Processor) updateWorkload(workload *workloadapi.Workload) error {
	var (
		err error
		bk  = bpf.BackendKey{}
		bv  = bpf.BackendValue{}
	)

	uid := p.hashName.StrToNum(workload.GetUid())
	ips := workload.GetAddresses()

	if waypoint := workload.GetWaypoint(); waypoint != nil {
		nets.CopyIpByteFromSlice(&bv.WaypointAddr, waypoint.GetAddress().Address)
		bv.WaypointPort = nets.ConvertPortToBigEndian(waypoint.GetHboneMtlsPort())
	}

	for serviceName := range workload.GetServices() {
		bv.Services[bv.ServiceCount] = p.hashName.StrToNum(serviceName)
		bv.ServiceCount++
		if bv.ServiceCount >= bpf.MaxServiceNum {
			log.Warnf("exceed the max service count, currently, a pod can belong to a maximum of 10 services")
			break
		}
	}

	for _, ip := range ips {
		bk.BackendUid = uid

		nets.CopyIpByteFromSlice(&bv.Ip, ip)
		if err = p.bpf.BackendUpdate(&bk, &bv); err != nil {
			log.Errorf("Update backend map failed, err:%s", err)
			return err
		}

		if err = p.storePodFrontendData(uid, ip); err != nil {
			log.Errorf("storePodFrontendData failed, err:%s", err)
			return err
		}
	}
	return nil
}

func (p *Processor) handleWorkload(workload *workloadapi.Workload) error {
	var deleteServices []string
	var newServices []string
	log.Debugf("handle workload: %s", workload.Uid)

	deleteServices, newServices = p.WorkloadCache.AddWorkload(workload)
	log.Debugf("delServices:%v, newServices:%v", deleteServices, newServices)

	// Delete Residual Services on the Workload
	if err := p.deleteResidualServicesWithWorkload(workload, deleteServices); err != nil {
		log.Errorf("deleteResidualServicesWithWorkload %s failed: %v", workload.GetUid(), err)
		return err
	}

	// Add new services associated with the workload
	if err := p.addNewServicesWithWorkload(workload, newServices); err != nil {
		log.Errorf("addNewServicesWithWorkload %s failed: %v", workload.Uid, err)
		return err
	}

	// Update workload
	if err := p.updateWorkload(workload); err != nil {
		log.Errorf("updateWorkload %s failed: %v", workload.Uid, err)
		return err
	}

	return nil
}

func (p *Processor) storeServiceFrontendData(serviceId uint32, service *workloadapi.Service) error {
	var (
		err error
		fk  = bpf.FrontendKey{}
		fv  = bpf.FrontendValue{}
	)

	fv.UpstreamId = serviceId
	for _, networkAddress := range service.GetAddresses() {
		nets.CopyIpByteFromSlice(&fk.Ip, networkAddress.Address)
		if err = p.bpf.FrontendUpdate(&fk, &fv); err != nil {
			log.Errorf("Update Frontend failed, err:%s", err)
			return err
		}
	}
	return nil
}

func (p *Processor) storeServiceData(serviceName string, waypoint *workloadapi.GatewayAddress, ports []*workloadapi.Port) error {
	var (
		err      error
		ek       = bpf.EndpointKey{}
		ev       = bpf.EndpointValue{}
		sk       = bpf.ServiceKey{}
		oldValue = bpf.ServiceValue{}
	)

	sk.ServiceId = p.hashName.StrToNum(serviceName)

	newValue := bpf.ServiceValue{}
	newValue.LbPolicy = LbPolicyRandom
	if waypoint != nil {
		nets.CopyIpByteFromSlice(&newValue.WaypointAddr, waypoint.GetAddress().Address)
		newValue.WaypointPort = nets.ConvertPortToBigEndian(waypoint.GetHboneMtlsPort())
	}

	for i, port := range ports {
		if i >= bpf.MaxPortNum {
			log.Warnf("exceed the max port count,current only support maximum of 10 ports")
			break
		}

		newValue.ServicePort[i] = nets.ConvertPortToBigEndian(port.ServicePort)
		if strings.Contains(serviceName, "waypoint") {
			newValue.TargetPort[i] = nets.ConvertPortToBigEndian(KmeshWaypointPort)
		} else {
			newValue.TargetPort[i] = nets.ConvertPortToBigEndian(port.TargetPort)
		}
	}

	// Already exists, it means this is service update.
	if err = p.bpf.ServiceLookup(&sk, &oldValue); err == nil {
		newValue.EndpointCount = oldValue.EndpointCount
	} else {
		// Only update the endpoint map when the service is first time added
		endpointCaches, ok := p.endpointsByService[serviceName]
		if ok {
			newValue.EndpointCount = uint32(len(endpointCaches))
			endpointIndex := uint32(0)
			for workloadUid := range endpointCaches {
				endpointIndex++
				ek.ServiceId = sk.ServiceId
				ek.BackendIndex = endpointIndex
				ev.BackendUid = p.hashName.StrToNum(workloadUid)

				if err = p.bpf.EndpointUpdate(&ek, &ev); err != nil {
					log.Errorf("Update Endpoint failed, err:%s", err)
					return err
				}
			}
		}
		delete(p.endpointsByService, serviceName)
	}

	if err = p.bpf.ServiceUpdate(&sk, &newValue); err != nil {
		log.Errorf("Update Service failed, err:%s", err)
	}

	return nil
}

func (p *Processor) handleService(service *workloadapi.Service) error {
	log.Debugf("service resource name: %s/%s", service.Namespace, service.Hostname)
	p.ServiceCache.AddOrUpdateService(service)
	serviceName := service.ResourceName()
	serviceId := p.hashName.StrToNum(serviceName)

	// store in frontend
	if err := p.storeServiceFrontendData(serviceId, service); err != nil {
		log.Errorf("storeServiceFrontendData failed, err:%s", err)
		return err
	}

	// get endpoint from ServiceCache, and update service and endpoint map
	if err := p.storeServiceData(serviceName, service.GetWaypoint(), service.GetPorts()); err != nil {
		log.Errorf("storeServiceData failed, err:%s", err)
		return err
	}
	return nil
}

func (p *Processor) handleRemovedAddresses(removed []string) error {
	var workloadNames []string
	var serviceNames []string
	for _, res := range removed {
		// workload resource name format: <cluster>/<group>/<kind>/<namespace>/<name></section-name>
		if strings.Count(res, "/") > 2 {
			workloadNames = append(workloadNames, res)
		} else {
			// service resource name format: namespace/hostname
			serviceNames = append(serviceNames, res)
		}
	}

	if err := p.removeWorkloadResource(workloadNames); err != nil {
		log.Errorf("RemoveWorkloadResource failed: %v", err)
	}
	if err := p.removeServiceResource(serviceNames); err != nil {
		log.Errorf("RemoveServiceResource failed: %v", err)
	}

	return nil
}

func (p *Processor) handleAddressTypeResponse(rsp *service_discovery_v3.DeltaDiscoveryResponse) error {
	var (
		err     error
		address = &workloadapi.Address{}
	)

	for _, resource := range rsp.GetResources() {
		if err = anypb.UnmarshalTo(resource.Resource, address, proto.UnmarshalOptions{}); err != nil {
			continue
		}

		log.Debugf("resource, %v", address)
		switch address.GetType().(type) {
		case *workloadapi.Address_Workload:
			workload := address.GetWorkload()
			err = p.handleWorkload(workload)
		case *workloadapi.Address_Service:
			service := address.GetService()
			err = p.handleService(service)
		default:
			log.Errorf("unknown type")
		}
	}
	if err != nil {
		log.Error(err)
	}

	_ = p.handleRemovedAddresses(rsp.RemovedResources)

	return err
}

func (p *Processor) handleAuthorizationTypeResponse(rsp *service_discovery_v3.DeltaDiscoveryResponse, rbac *auth.Rbac) error {
	if rbac == nil {
		return fmt.Errorf("Rbac module uninitialized")
	}
	// update resource
	for _, resource := range rsp.GetResources() {
		auth := &security.Authorization{}
		if err := anypb.UnmarshalTo(resource.Resource, auth, proto.UnmarshalOptions{}); err != nil {
			log.Errorf("unmarshal failed, err: %v", err)
			continue
		}
		log.Debugf("handle authorization policy %s, auth %s", resource.GetName(), auth.String())
		if err := rbac.UpdatePolicy(auth); err != nil {
			return err
		}
	}

	// delete resource by name
	for _, resourceName := range rsp.GetRemovedResources() {
		rbac.RemovePolicy(resourceName)
		log.Debugf("remove authorization policy %s", resourceName)
	}

	return nil
}

func (p *Processor) deleteEndpointRecords(endpoint_keys []bpf.EndpointKey) error {
	var (
		err               error
		skUpdate          = bpf.ServiceKey{}
		svUpdate          = bpf.ServiceValue{}
		lastEndpointKey   = bpf.EndpointKey{}
		lastEndpointValue = bpf.EndpointValue{}
	)

	for _, ek := range endpoint_keys {
		log.Debugf("Find EndpointKey: [%#v]", ek)
		// 2. find the service
		skUpdate.ServiceId = ek.ServiceId
		if err = p.bpf.ServiceLookup(&skUpdate, &svUpdate); err == nil {
			log.Debugf("Find ServiceValue: [%#v]", svUpdate)
			// 3. find the last indexed endpoint of the service
			lastEndpointKey.ServiceId = skUpdate.ServiceId
			lastEndpointKey.BackendIndex = svUpdate.EndpointCount
			if err = p.bpf.EndpointLookup(&lastEndpointKey, &lastEndpointValue); err == nil {
				log.Debugf("Find EndpointValue: [%#v]", lastEndpointValue)
				// 4. switch the index of the last with the current removed endpoint
				if err = p.bpf.EndpointUpdate(&ek, &lastEndpointValue); err != nil {
					log.Errorf("EndpointUpdate failed: %s", err)
					return err
				}
				if err = p.bpf.EndpointDelete(&lastEndpointKey); err != nil {
					log.Errorf("EndpointDelete failed: %s", err)
					return err
				}
				svUpdate.EndpointCount = svUpdate.EndpointCount - 1
				if err = p.bpf.ServiceUpdate(&skUpdate, &svUpdate); err != nil {
					log.Errorf("ServiceUpdate failed: %s", err)
					return err
				}
			} else {
				// last indexed endpoint not exists, this should not occur
				// we should delete the endpoint just in case leak
				if err = p.bpf.EndpointDelete(&ek); err != nil {
					log.Errorf("EndpointDelete failed: %s", err)
					return err
				}
			}
		} else { // service not exist, we should delete the endpoint
			if err = p.bpf.EndpointDelete(&ek); err != nil {
				log.Errorf("EndpointDelete failed: %s", err)
				return err
			}
		}
	}
	return nil
}
