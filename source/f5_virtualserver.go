/*
Copyright 2022 The Kubernetes Authors.

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

package source

import (
	"context"
	"fmt"
	"sort"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"

	"sigs.k8s.io/external-dns/endpoint"
)

var f5VirtualServerGVR = schema.GroupVersionResource{
	Group:    "cis.f5.com",
	Version:  "v1",
	Resource: "virtualservers",
}

// virtualServerSource is an implementation of Source for F5 VirtualServer objects.
type f5VirtualServerSource struct {
	dynamicKubeClient     dynamic.Interface
	virtualServerInformer informers.GenericInformer
	kubeClient            kubernetes.Interface
	annotationFilter      string
	namespace             string
	unstructuredConverter *unstructuredConverter
}

func NewF5VirtualServerSource(
	ctx context.Context,
	dynamicKubeClient dynamic.Interface,
	kubeClient kubernetes.Interface,
	namespace string,
	annotationFilter string,
) (Source, error) {
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicKubeClient, 0, namespace, nil)
	virtualServerInformer := informerFactory.ForResource(f5VirtualServerGVR)

	virtualServerInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
			},
		},
	)

	informerFactory.Start(ctx.Done())

	// wait for the local cache to be populated.
	if err := waitForDynamicCacheSync(context.Background(), informerFactory); err != nil {
		return nil, err
	}

	uc, err := newVSUnstructuredConverter()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to setup unstructured converter")
	}

	return &f5VirtualServerSource{
		dynamicKubeClient:     dynamicKubeClient,
		virtualServerInformer: virtualServerInformer,
		kubeClient:            kubeClient,
		namespace:             namespace,
		annotationFilter:      annotationFilter,
		unstructuredConverter: uc,
	}, nil
}

// Endpoints returns endpoint objects for each host-target combination that should be processed.
// Retrieves all VirtualServers in the source's namespace(s).
func (vs *f5VirtualServerSource) Endpoints(ctx context.Context) ([]*endpoint.Endpoint, error) {
	virtualServerObjects, err := vs.virtualServerInformer.Lister().ByNamespace(vs.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var virtualServers []*VirtualServer
	for _, vsObj := range virtualServerObjects {
		unstructuredHost, ok := vsObj.(*unstructured.Unstructured)
		if !ok {
			return nil, errors.New("could not convert")
		}

		virtualServer := &VirtualServer{}
		err := vs.unstructuredConverter.scheme.Convert(unstructuredHost, virtualServer, nil)
		if err != nil {
			return nil, err
		}
		virtualServers = append(virtualServers, virtualServer)
	}

	virtualServers, err = vs.filterByAnnotations(virtualServers)
	if err != nil {
		return nil, errors.Wrap(err, "failed to filter VirtualServers")
	}

	endpoints, err := vs.endpointsFromVirtualServers(virtualServers)
	if err != nil {
		return nil, err
	}

	// Sort endpoints
	for _, ep := range endpoints {
		sort.Sort(ep.Targets)
	}

	return endpoints, nil
}

func (vs *f5VirtualServerSource) AddEventHandler(ctx context.Context, handler func()) {
	log.Debug("Adding event handler for VirtualServer")

	vs.virtualServerInformer.Informer().AddEventHandler(eventHandlerFunc(handler))
}

// endpointsFromVirtualServers extracts the endpoints from a slice of VirtualServers
func (vs *f5VirtualServerSource) endpointsFromVirtualServers(virtualServers []*VirtualServer) ([]*endpoint.Endpoint, error) {
	var endpoints []*endpoint.Endpoint

	for _, virtualServer := range virtualServers {
		ttl, err := getTTLFromAnnotations(virtualServer.Annotations)
		if err != nil {
			return nil, err
		}

		if virtualServer.Spec.VirtualServerAddress != "" {
			ep := &endpoint.Endpoint{
				Targets: endpoint.Targets{
					virtualServer.Spec.VirtualServerAddress,
				},
				RecordType: "A",
				DNSName:    virtualServer.Spec.Host,
				Labels:     endpoint.NewLabels(),
				RecordTTL:  ttl,
			}

			vs.setResourceLabel(virtualServer, ep)
			endpoints = append(endpoints, ep)
			continue
		}

		if virtualServer.Status.VSAddress != "" {
			ep := &endpoint.Endpoint{
				Targets: endpoint.Targets{
					virtualServer.Status.VSAddress,
				},
				RecordType: "A",
				DNSName:    virtualServer.Spec.Host,
				Labels:     endpoint.NewLabels(),
				RecordTTL:  ttl,
			}

			vs.setResourceLabel(virtualServer, ep)
			endpoints = append(endpoints, ep)
			continue
		}
	}

	return endpoints, nil
}

// newUnstructuredConverter returns a new unstructuredConverter initialized
func newVSUnstructuredConverter() (*unstructuredConverter, error) {
	uc := &unstructuredConverter{
		scheme: runtime.NewScheme(),
	}

	// Add the core types we need
	uc.scheme.AddKnownTypes(f5VirtualServerGVR.GroupVersion(), &VirtualServer{}, &VirtualServerList{})
	if err := scheme.AddToScheme(uc.scheme); err != nil {
		return nil, err
	}

	return uc, nil
}

// filterByAnnotations filters a list of VirtualServers by a given annotation selector.
func (vs *f5VirtualServerSource) filterByAnnotations(virtualServers []*VirtualServer) ([]*VirtualServer, error) {
	labelSelector, err := metav1.ParseToLabelSelector(vs.annotationFilter)
	if err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}

	// empty filter returns original list
	if selector.Empty() {
		return virtualServers, nil
	}

	filteredList := []*VirtualServer{}

	for _, vs := range virtualServers {
		// convert the VirtualServer's annotations to an equivalent label selector
		annotations := labels.Set(vs.Annotations)

		// include VirtualServer if its annotations match the selector
		if selector.Matches(annotations) {
			filteredList = append(filteredList, vs)
		}
	}

	return filteredList, nil
}

func (vs *f5VirtualServerSource) setResourceLabel(virtualServer *VirtualServer, ep *endpoint.Endpoint) {
	ep.Labels[endpoint.ResourceLabelKey] = fmt.Sprintf("f5-virtualserver/%s/%s", virtualServer.Namespace, virtualServer.Name)
}

//
// The below structs and methods have been copy-pasted from
// https://github.com/F5Networks/k8s-bigip-ctlr/blob/master/config/apis/cis/v1/types.go
// since there's no way of importing these at the moment,
// please see this issue for more info: https://github.com/F5Networks/k8s-bigip-ctlr/issues/2153.
// When and if this issue can be fixed we can instead import these types.
//

// VirtualServer defines the VirtualServer resource.
type VirtualServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualServerSpec   `json:"spec"`
	Status VirtualServerStatus `json:"status,omitempty"`
}

// VirtualServerStatus is the status of the VirtualServer resource.
type VirtualServerStatus struct {
	VSAddress string `json:"vsAddress,omitempty"`
}

// VirtualServerSpec is the spec of the VirtualServer resource.
type VirtualServerSpec struct {
	Host                   string           `json:"host,omitempty"`
	VirtualServerAddress   string           `json:"virtualServerAddress,omitempty"`
	IPAMLabel              string           `json:"ipamLabel,omitempty"`
	VirtualServerName      string           `json:"virtualServerName,omitempty"`
	VirtualServerHTTPPort  int32            `json:"virtualServerHTTPPort,omitempty"`
	VirtualServerHTTPSPort int32            `json:"virtualServerHTTPSPort,omitempty"`
	Pools                  []Pool           `json:"pools,omitempty"`
	TLSProfileName         string           `json:"tlsProfileName,omitempty"`
	HTTPTraffic            string           `json:"httpTraffic,omitempty"`
	SNAT                   string           `json:"snat,omitempty"`
	WAF                    string           `json:"waf,omitempty"`
	RewriteAppRoot         string           `json:"rewriteAppRoot,omitempty"`
	AllowVLANs             []string         `json:"allowVlans,omitempty"`
	IRules                 []string         `json:"iRules,omitempty"`
	ServiceIPAddress       []ServiceAddress `json:"serviceAddress,omitempty"`
}

// ServiceAddress Service IP address definition (BIG-IP virtual-address).
type ServiceAddress struct {
	ArpEnabled         bool   `json:"arpEnabled,omitempty"`
	ICMPEcho           string `json:"icmpEcho,omitempty"`
	RouteAdvertisement string `json:"routeAdvertisement,omitempty"`
	TrafficGroup       string `json:"trafficGroup,omitempty"`
	SpanningEnabled    bool   `json:"spanningEnabled,omitempty"`
}

// Pool defines a pool object in BIG-IP.
type Pool struct {
	Path            string  `json:"path,omitempty"`
	Service         string  `json:"service"`
	ServicePort     int32   `json:"servicePort"`
	NodeMemberLabel string  `json:"nodeMemberLabel,omitempty"`
	Monitor         Monitor `json:"monitor"`
	Rewrite         string  `json:"rewrite,omitempty"`
}

// Monitor defines a monitor object in BIG-IP.
type Monitor struct {
	Type     string `json:"type"`
	Send     string `json:"send"`
	Recv     string `json:"recv"`
	Interval int    `json:"interval"`
	Timeout  int    `json:"timeout"`
}

// VirtualServerList is a list of the VirtualServer resources.
type VirtualServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []VirtualServer `json:"items"`
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VirtualServer) DeepCopyInto(out *VirtualServer) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VirtualServer.
func (in *VirtualServer) DeepCopy() *VirtualServer {
	if in == nil {
		return nil
	}
	out := new(VirtualServer)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *VirtualServer) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VirtualServerList) DeepCopyInto(out *VirtualServerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]VirtualServer, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VirtualServerList.
func (in *VirtualServerList) DeepCopy() *VirtualServerList {
	if in == nil {
		return nil
	}
	out := new(VirtualServerList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *VirtualServerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VirtualServerSpec) DeepCopyInto(out *VirtualServerSpec) {
	*out = *in
	if in.Pools != nil {
		in, out := &in.Pools, &out.Pools
		*out = make([]Pool, len(*in))
		copy(*out, *in)
	}
	if in.AllowVLANs != nil {
		in, out := &in.AllowVLANs, &out.AllowVLANs
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.IRules != nil {
		in, out := &in.IRules, &out.IRules
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.ServiceIPAddress != nil {
		in, out := &in.ServiceIPAddress, &out.ServiceIPAddress
		*out = make([]ServiceAddress, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VirtualServerSpec.
func (in *VirtualServerSpec) DeepCopy() *VirtualServerSpec {
	if in == nil {
		return nil
	}
	out := new(VirtualServerSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VirtualServerStatus) DeepCopyInto(out *VirtualServerStatus) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VirtualServerStatus.
func (in *VirtualServerStatus) DeepCopy() *VirtualServerStatus {
	if in == nil {
		return nil
	}
	out := new(VirtualServerStatus)
	in.DeepCopyInto(out)
	return out
}
