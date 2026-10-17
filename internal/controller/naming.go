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
	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	labelAppName      = "app.kubernetes.io/name"
	labelAppInstance  = "app.kubernetes.io/instance"
	labelAppComponent = "app.kubernetes.io/component"

	// labelRole is set on each database pod by Patroni (not by this
	// operator) to "primary" or "replica". It is what the read-only Service
	// selects on. The read-write Service deliberately does not use it: a
	// primary that hangs keeps a stale role=primary label until its pod is
	// replaced, so routing writes by label could send them to a dead node.
	labelRole = "db.youroperator.io/role"

	appNameValue      = "postgrescluster"
	componentPostgres = "postgres"
	rolePrimary       = "primary"
	roleReplica       = "replica"
)

// labelsForCluster are the labels on every object the operator creates for a
// cluster, including backup Jobs. They are not a safe pod selector on their
// own, because backup Job pods carry them too; use podSelectorLabels for
// anything that must match database pods only.
func labelsForCluster(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return map[string]string{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
	}
}

// podSelectorLabels select the database pods of a cluster, and nothing else.
// They are also the labels Patroni stamps on the Endpoints and ConfigMaps it
// creates, which is how those leftovers are found for cleanup.
func podSelectorLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return selectorLabelsFor(cluster.Name)
}

func selectorLabelsFor(clusterName string) map[string]string {
	return map[string]string{
		labelAppName:      appNameValue,
		labelAppInstance:  clusterName,
		labelAppComponent: componentPostgres,
	}
}

func credentialsSecretName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-credentials"
}

// headlessServiceName is the StatefulSet's governing Service, giving each
// pod a stable DNS name.
func headlessServiceName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-headless"
}

func readOnlyServiceName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-readonly"
}

// configServiceName names a selector-less headless Service whose only job is
// to exist: Patroni keeps its dynamic configuration in an Endpoints object
// called <cluster>-config, and Kubernetes garbage-collects an Endpoints
// object that has no Service of the same name.
func configServiceName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-config"
}

// patroniAccountName names the ServiceAccount, Role and RoleBinding that let
// the database pods talk to the Kubernetes API as Patroni's DCS.
func patroniAccountName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-patroni"
}

func backupName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-backup"
}
