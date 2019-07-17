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
	"k8s.io/apimachinery/pkg/runtime/schema"
	capiv1alpha2 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MachineToConfigMapFunc returns a handler.ToRequestsFunc that watches for
// Machine events and returns reconciliation requests for a Configuration object.
func MachineToConfigMapFunc(gvk schema.GroupVersionKind) handler.ToRequestsFunc {
	return func(o handler.MapObject) []reconcile.Request {
		m, ok := o.Object.(*capiv1alpha2.Machine)
		if !ok {
			return nil
		}

		// ConfigRef is an optional field
		configRef := m.Spec.Bootstrap.ConfigRef

		if configRef == nil {
			return nil
		}

		// Return early if the GroupVersionKind doesn't match what we expect.
		boostrapGVK := configRef.GroupVersionKind()

		if gvk != boostrapGVK {
			return nil
		}

		return []reconcile.Request{
			{
				NamespacedName: client.ObjectKey{
					Namespace: m.Namespace,
					Name:      m.Spec.Bootstrap.ConfigRef.Name,
				},
			},
		}

	}
}
