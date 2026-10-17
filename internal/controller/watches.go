/*
Copyright 2026.

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

package controller

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// patroniEndpoints names the core Endpoints type once. It is deprecated in
// Kubernetes, but it is where Patroni's Kubernetes DCS keeps the leader
// record, so it is what has to be watched and read.
type patroniEndpoints = corev1.Endpoints //nolint:staticcheck // see above

// clusterForObject maps a database pod, or the Endpoints Patroni keeps for a
// cluster, back to the PostgresCluster it belongs to. Objects without the
// database labels, such as backup Job pods, map to nothing.
func clusterForObject(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[labelAppName] != appNameValue || labels[labelAppComponent] != componentPostgres || labels[labelAppInstance] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(), Name: labels[labelAppInstance],
	}}}
}

// podChangePredicate passes pod creation and deletion, and updates that
// change something the operator reports: readiness, the pod's address, a
// deletion in progress, or Patroni's view of the member (role, state,
// timeline, replication state). It deliberately ignores the WAL position in
// that view, which moves on every write and would otherwise wake the
// controller every few seconds for each pod; lag is picked up by the periodic
// refresh instead.
func podChangePredicate() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return true
			}
			return podReady(oldPod) != podReady(newPod) ||
				oldPod.Status.PodIP != newPod.Status.PodIP ||
				oldPod.Status.Phase != newPod.Status.Phase ||
				stableMemberStatus(oldPod) != stableMemberStatus(newPod)
		},
	}
}

// stableMemberStatus is Patroni's status annotation with the WAL position
// zeroed, so two readings of an otherwise unchanged member compare equal.
func stableMemberStatus(pod *corev1.Pod) memberStatus {
	var status memberStatus
	if raw := pod.Annotations[annotationStatus]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &status)
	}
	status.XLogLocation = 0
	return status
}

// leaderChangePredicate passes Endpoints creation and deletion, and updates
// that change who the leader is. Patroni also rewrites the lock's renewTime
// every loop, which says nothing new.
func leaderChangePredicate() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetAnnotations()[annotationLeader] != e.ObjectNew.GetAnnotations()[annotationLeader]
		},
	}
}
