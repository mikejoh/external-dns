package source

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	fakeDynamic "k8s.io/client-go/dynamic/fake"
	fakeKube "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/external-dns/endpoint"
)

const defaultF5VirtualServerNamespace = "virtualserver"

func TestF5VirtualServerEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		virtualServer VirtualServer
		expected      []*endpoint.Endpoint
	}{
		{
			name: "F5 VirtualServer with host and virtualServerAddress set",
			virtualServer: VirtualServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: f5VirtualServerGVR.GroupVersion().String(),
					Kind:       "VirtualServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vs",
					Namespace: defaultF5VirtualServerNamespace,
				},
				Spec: VirtualServerSpec{
					Host:                 "www.example.com",
					VirtualServerAddress: "192.168.1.100",
				},
			},
			expected: []*endpoint.Endpoint{
				{
					DNSName:    "www.example.com",
					Targets:    []string{"192.168.1.100"},
					RecordType: endpoint.RecordTypeA,
					RecordTTL:  0,
					Labels: endpoint.Labels{
						"resource": "f5-virtualserver/virtualserver/test-vs",
					},
				},
			},
		},
		{
			name: "F5 VirtualServer with host set and IP address from the status field",
			virtualServer: VirtualServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: f5VirtualServerGVR.GroupVersion().String(),
					Kind:       "VirtualServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vs",
					Namespace: defaultF5VirtualServerNamespace,
				},
				Spec: VirtualServerSpec{
					Host: "www.example.com",
				},
				Status: VirtualServerStatus{
					VSAddress: "192.168.1.100",
				},
			},
			expected: []*endpoint.Endpoint{
				{
					DNSName:    "www.example.com",
					Targets:    []string{"192.168.1.100"},
					RecordType: endpoint.RecordTypeA,
					RecordTTL:  0,
					Labels: endpoint.Labels{
						"resource": "f5-virtualserver/virtualserver/test-vs",
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeKubernetesClient := fakeKube.NewSimpleClientset()
			scheme := runtime.NewScheme()
			scheme.AddKnownTypes(f5VirtualServerGVR.GroupVersion(), &VirtualServer{}, &VirtualServerList{})
			fakeDynamicClient := fakeDynamic.NewSimpleDynamicClient(scheme)

			virtualServer := unstructured.Unstructured{}

			virtualServerJSON, err := json.Marshal(tc.virtualServer)
			require.NoError(t, err)
			assert.NoError(t, virtualServer.UnmarshalJSON(virtualServerJSON))

			// Create VirtualServer resources
			_, err = fakeDynamicClient.Resource(f5VirtualServerGVR).Namespace(defaultF5VirtualServerNamespace).Create(context.Background(), &virtualServer, metav1.CreateOptions{})
			assert.NoError(t, err)

			source, err := NewF5VirtualServerSource(context.TODO(), fakeDynamicClient, fakeKubernetesClient, defaultF5VirtualServerNamespace)
			require.NoError(t, err)
			assert.NotNil(t, source)

			count := &unstructured.UnstructuredList{}
			for len(count.Items) < 1 {
				count, _ = fakeDynamicClient.Resource(f5VirtualServerGVR).Namespace(defaultF5VirtualServerNamespace).List(context.Background(), metav1.ListOptions{})
			}

			endpoints, err := source.Endpoints(context.Background())
			require.NoError(t, err)
			assert.Len(t, endpoints, len(tc.expected))
			assert.Equal(t, endpoints, tc.expected)
		})
	}
}
