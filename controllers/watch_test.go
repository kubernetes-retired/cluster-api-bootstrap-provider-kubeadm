/*
Copyright 2019 The Kubernetes Authors.

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

package controllers

import (
	"testing"

	"reflect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestMachineToInfrastructureMapFunc(t *testing.T) {
	var testcases = []struct {
		name    string
		input   schema.GroupVersionKind
		request *corev1.ObjectReference
		output  []reconcile.Request
	}{
		{
			name: "reconcile infra-1",
			input: schema.GroupVersionKind{
				Group:   "foo.cluster.sigs.k8s.io",
				Version: "v1alpha2",
				Kind:    "TestMachine",
			},
			request: &corev1.ObjectReference{
				APIVersion: "foo.cluster.sigs.k8s.io/v1alpha2",
				Kind:       "TestMachine",
				Name:       "infra-1",
			},
			output: []reconcile.Request{
				{
					NamespacedName: client.ObjectKey{
						Namespace: "default",
						Name:      "infra-1",
					},
				},
			},
		},
		{
			name: "should return no matching reconcile requests",
			input: schema.GroupVersionKind{
				Group:   "foo.cluster.sigs.k8s.io",
				Version: "v1alpha2",
				Kind:    "TestMachine",
			},
			request: &corev1.ObjectReference{
				APIVersion: "bar.cluster.sigs.k8s.io/v1alpha2",
				Kind:       "TestMachine",
				Name:       "bar-1",
			},
			output: nil,
		},
		{
			name: "undefined optional field ConfigRef",
			input: schema.GroupVersionKind{
				Group:   "foo.cluster.sigs.k8s.io",
				Version: "v1alpha2",
				Kind:    "TestMachine",
			},
			request: nil,
			output:  nil,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fn := MachineToConfigMapFunc(tc.input)
			out := fn(handler.MapObject{
				Object: &clusterv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "test-1",
					},
					Spec: clusterv1.MachineSpec{
						Bootstrap: clusterv1.Bootstrap{
							ConfigRef: tc.request,
						},
					},
				},
			})
			if !reflect.DeepEqual(out, tc.output) {
				t.Fatalf("Unexpected output. Got: %v, Want: %v", out, tc.output)
			}
		})
	}
}
